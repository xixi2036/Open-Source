package admin_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"hush/server/internal/admin"
	"hush/server/internal/api"
	"hush/server/internal/store"
)

func setSettings(t *testing.T, owner *session, kv map[string]string) {
	t.Helper()
	owner.must("PUT", "/admin/api/settings", map[string]any{"values": kv}, nil)
}

func TestRegistrationModesAndInvites(t *testing.T) {
	e := newEnv(t)
	owner := e.mkAdmin("owner1", store.AdminOwner)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	viewer := e.mkAdmin("viewer1", store.AdminViewer)
	old := e.register("old") // 开放模式下注册的老用户

	// 默认开放：带不带 invite_code 都能注册（现有客户端行为不变）
	if st, code, _ := e.verify("open1@example.com", ""); st != 200 {
		t.Fatalf("开放模式应能注册: %d %s", st, code)
	}
	if st, _, _ := e.verify("open2@example.com", "whatever"); st != 200 {
		t.Fatal("开放模式下多余的 invite_code 应被忽略")
	}

	// 邀请码管理：moderator 可生成，参数校验
	mod.expect("POST", "/admin/api/invites", map[string]any{"count": 101}, 400, "bad_request")
	mod.expect("POST", "/admin/api/invites", map[string]any{"max_uses": -1}, 400, "bad_request")
	mod.expect("POST", "/admin/api/invites", map[string]any{"expires_at": 1}, 400, "bad_request")
	var made []store.Invite
	mod.must("POST", "/admin/api/invites", map[string]any{"count": 2, "max_uses": 2, "note": "内测"}, &made)
	if len(made) != 2 || made[0].Code == made[1].Code || len(made[0].Code) != 11 || made[0].MaxUses != 2 {
		t.Fatalf("生成结果不对: %+v", made)
	}
	if a := e.audited("invite.create", "invite"); strings.Contains(a.Detail, made[0].Code) {
		t.Fatalf("邀请码不应进审计: %s", a.Detail)
	}
	code := made[0].Code

	setSettings(t, owner, map[string]string{"registration_mode": "invite"})
	// 新用户：缺邀请码 / 无效邀请码
	if st, c, _ := e.verify("new1@example.com", ""); st != 403 || c != "invite_required" {
		t.Fatalf("应为 403 invite_required: %d %s", st, c)
	}
	if st, c, _ := e.verify("new1@example.com", "BOGUS-CODE1"); st != 403 || c != "invite_invalid" {
		t.Fatalf("应为 403 invite_invalid: %d %s", st, c)
	}
	// 被拒绝的注册不创建账号
	var users pageOf[map[string]any]
	owner.must("GET", "/admin/api/users?q=new1", nil, &users)
	if users.Total != 0 {
		t.Fatal("被拒绝的注册不应创建账号")
	}
	// 有效邀请码（大小写、空白不敏感）
	st, c, n1 := e.verify("new1@example.com", " "+strings.ToLower(code)+" ")
	if st != 200 {
		t.Fatalf("有效邀请码应能注册: %d %s", st, c)
	}
	// 老用户登录不受影响（不需要邀请码，也不消耗次数）
	if st, c, u := e.verify(old.Email, ""); st != 200 || u.ID != old.ID {
		t.Fatalf("老用户登录不应受影响: %d %s", st, c)
	}
	if st, _, u := e.verify("new1@example.com", ""); st != 200 || u.ID != n1.ID {
		t.Fatal("已注册用户再次登录不需要邀请码")
	}
	// 次数上限 2：第二个新用户可以，第三个不行
	if st, _, _ := e.verify("new2@example.com", code); st != 200 {
		t.Fatal("第二次使用应成功")
	}
	if st, c, _ := e.verify("new3@example.com", code); st != 403 || c != "invite_invalid" {
		t.Fatalf("次数用完应为 invite_invalid: %d %s", st, c)
	}
	var invites struct {
		Items []store.Invite `json:"items"`
		Mode  string         `json:"registration_mode"`
	}
	mod.must("GET", "/admin/api/invites", nil, &invites)
	used := map[string]int{}
	for _, in := range invites.Items {
		used[in.Code] = in.Used
	}
	if invites.Mode != "invite" || used[code] != 2 || used[made[1].Code] != 0 {
		t.Fatalf("使用次数不对: %+v", invites)
	}
	var uses []store.InviteUse
	mod.must("GET", "/admin/api/invites/"+code+"/uses", nil, &uses)
	if len(uses) != 2 || uses[0].Email != "new1@example.com" || uses[0].UserID != n1.ID {
		t.Fatalf("使用记录不对: %+v", uses)
	}
	// 只读角色看不到完整邀请码
	status, body := viewer.do("GET", "/admin/api/invites", nil)
	if status != 200 || bytes.Contains(body, []byte(code)) || bytes.Contains(body, []byte(made[1].Code)) {
		t.Fatalf("viewer 不应看到完整邀请码: %s", body)
	}
	// 作废
	mod.must("POST", "/admin/api/invites/"+made[1].Code+"/revoke", nil, nil)
	mod.expect("POST", "/admin/api/invites/NOPE/revoke", nil, 404, "not_found")
	if st, c, _ := e.verify("new4@example.com", made[1].Code); st != 403 || c != "invite_invalid" {
		t.Fatalf("已作废的邀请码应无效: %d %s", st, c)
	}
	e.audited("invite.revoke", "invite:")
	// 过期
	var exp []store.Invite
	mod.must("POST", "/admin/api/invites", map[string]any{"expires_at": time.Now().Add(300 * time.Millisecond).UnixMilli()}, &exp)
	time.Sleep(400 * time.Millisecond)
	if st, c, _ := e.verify("new5@example.com", exp[0].Code); st != 403 || c != "invite_invalid" {
		t.Fatalf("过期的邀请码应无效: %d %s", st, c)
	}

	// 真实验证码路径：invite_required 不消费验证码，补上邀请码后同一个验证码可用；邀请码试错有次数上限
	var fresh []store.Invite
	mod.must("POST", "/admin/api/invites", map[string]any{}, &fresh)
	email := "real@example.com"
	if st, b := e.apiDo("POST", "/v1/auth/code", "", map[string]any{"email": email}); st != 200 {
		t.Fatalf("申请验证码失败: %d %s", st, b)
	}
	real := e.mailer.code(email)
	req := map[string]any{"email": email, "code": real}
	if st, b := e.apiDo("POST", "/v1/auth/verify", "", req); st != 403 || !bytes.Contains(b, []byte("invite_required")) {
		t.Fatalf("应为 invite_required: %d %s", st, b)
	}
	req["invite_code"] = "WRONG-WRONG"
	if st, b := e.apiDo("POST", "/v1/auth/verify", "", req); st != 403 || !bytes.Contains(b, []byte("invite_invalid")) {
		t.Fatalf("应为 invite_invalid: %d %s", st, b)
	}
	req["invite_code"] = fresh[0].Code
	if st, b := e.apiDo("POST", "/v1/auth/verify", "", req); st != 200 {
		t.Fatalf("补上邀请码后同一验证码应可注册: %d %s", st, b)
	}
	email2 := "brute@example.com"
	e.apiDo("POST", "/v1/auth/code", "", map[string]any{"email": email2})
	req = map[string]any{"email": email2, "code": e.mailer.code(email2), "invite_code": "WRONG-WRONG"}
	for i := 0; i < store.CodeMaxAttempts; i++ {
		if st, b := e.apiDo("POST", "/v1/auth/verify", "", req); st != 403 {
			t.Fatalf("第 %d 次应为 403: %d %s", i+1, st, b)
		}
	}
	if st, b := e.apiDo("POST", "/v1/auth/verify", "", req); st != 401 {
		t.Fatalf("邀请码试错 5 次后验证码应作废: %d %s", st, b)
	}

	// 关闭注册：新用户一律拒绝，老用户照常登录
	setSettings(t, owner, map[string]string{"registration_mode": "closed"})
	if st, c, _ := e.verify("new6@example.com", code); st != 403 || c != "registration_closed" {
		t.Fatalf("应为 registration_closed: %d %s", st, c)
	}
	if st, _, _ := e.verify(old.Email, ""); st != 200 {
		t.Fatal("关闭注册不影响老用户")
	}
	// 邮箱域名白名单（只约束新用户）
	setSettings(t, owner, map[string]string{"registration_mode": "open", "email_domains": "Corp.cn, @example.org"})
	if st, c, _ := e.verify("x@example.com", ""); st != 403 || c != "email_not_allowed" {
		t.Fatalf("应为 email_not_allowed: %d %s", st, c)
	}
	if st, _, _ := e.verify("x@corp.cn", ""); st != 200 {
		t.Fatal("白名单内的域名应能注册")
	}
	if st, _, _ := e.verify(old.Email, ""); st != 200 {
		t.Fatal("白名单不影响老用户")
	}
}

func TestAnnouncements(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	online, offline, disabled := e.register("on"), e.register("off"), e.register("dis")
	mod.must("POST", "/admin/api/users/"+disabled.ID+"/disable", nil, nil)
	conn := online.dial()

	mod.expect("POST", "/admin/api/announcements", map[string]any{"title": "", "body": "x"}, 400, "bad_request")
	mod.expect("POST", "/admin/api/announcements", map[string]any{"title": "t", "body": ""}, 400, "bad_request")
	mod.expect("POST", "/admin/api/announcements", map[string]any{"title": "t", "body": "b", "expires_at": 5}, 400, "bad_request")
	var res struct {
		Announcement store.Announcement `json:"announcement"`
		Recipients   int                `json:"recipients"`
	}
	mod.must("POST", "/admin/api/announcements", map[string]any{"title": "停机维护 <b>", "body": "今晚 23:00 & 明早"}, &res)
	if res.Recipients != 2 || res.Announcement.ID == "" {
		t.Fatalf("发布结果不对（停用账号不应收到）: %+v", res)
	}
	id := res.Announcement.ID
	// 在线用户实时收到事件：事件 id 即公告 id，字段与契约一致
	ev := conn.event("announcement")
	if ev["id"] != id || ev["title"] != "停机维护 <b>" || ev["body"] != "今晚 23:00 & 明早" || ev["created_at"] == nil {
		t.Fatalf("announcement 事件不对: %v", ev)
	}
	// 离线用户：事件已入队（outbox），可 ack
	if !offline.hasEvent("announcement", func(ev map[string]any) bool { return ev["id"] == id }) {
		t.Fatalf("离线用户队列里应有公告事件: %v", offline.events())
	}
	offline.must("POST", "/v1/messages/ack", map[string]any{"ids": []string{id}}, nil)
	if len(offline.events()) != 0 {
		t.Fatal("公告事件应可按 id ack")
	}
	if n, _, _ := e.st.QueueUsage(context.Background(), disabled.ID); n != 0 {
		t.Fatal("停用账号不应收到公告事件")
	}
	// 带截止时间的公告：到期后不再出现在客户端历史里
	mod.must("POST", "/admin/api/announcements", map[string]any{"title": "短期", "body": "b", "expires_at": time.Now().Add(300 * time.Millisecond).UnixMilli()}, &res)
	short := res.Announcement.ID
	var list []store.Announcement
	offline.must("GET", "/v1/announcements", nil, &list)
	if len(list) != 2 || list[0].ID != short || list[1].ID != id || list[1].CreatedBy != "" {
		t.Fatalf("公告历史不对: %+v", list)
	}
	time.Sleep(400 * time.Millisecond)
	offline.must("GET", "/v1/announcements", nil, &list)
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("过期公告不应返回: %+v", list)
	}
	offline.must("GET", "/v1/announcements?limit=1&before="+fmt.Sprint(list[0].CreatedAt), nil, &list)
	if len(list) != 0 {
		t.Fatalf("before 游标不生效: %+v", list)
	}
	if st, _ := e.apiDo("GET", "/v1/announcements", "", nil); st != 401 {
		t.Fatal("公告历史需要登录")
	}
	// 后台列表含已过期的，带发布人；撤下后客户端不再返回
	mod.must("GET", "/admin/api/announcements", nil, &list)
	if len(list) != 2 || list[0].CreatedBy != "mod1" {
		t.Fatalf("后台公告列表不对: %+v", list)
	}
	mod.must("DELETE", "/admin/api/announcements/"+id, nil, nil)
	mod.expect("DELETE", "/admin/api/announcements/"+id, nil, 404, "not_found")
	offline.must("GET", "/v1/announcements", nil, &list)
	if len(list) != 0 {
		t.Fatalf("撤下的公告不应返回: %+v", list)
	}
	e.audited("announcement.publish", "announcement:"+id)
	e.audited("announcement.delete", "announcement:"+id)
}

func TestSettingsTakeEffectLive(t *testing.T) {
	e := newEnv(t)
	owner := e.mkAdmin("owner1", store.AdminOwner)
	a, b := e.register("alice"), e.register("bob")
	befriend(t, a, b)
	send := func() string {
		var res struct {
			Results []struct {
				Error string `json:"error"`
			} `json:"results"`
		}
		a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 0, "body": "QUJD"}}}, &res)
		return res.Results[0].Error
	}
	ackAll(b)
	var got struct {
		Fields     []api.SettingField `json:"fields"`
		Values     map[string]string  `json:"values"`
		Defaults   map[string]string  `json:"defaults"`
		Overridden []string           `json:"overridden"`
	}
	owner.must("GET", "/admin/api/settings", nil, &got)
	if len(got.Fields) < 10 || got.Values["registration_mode"] != "open" || got.Values["queue_max_items"] != "10000" || len(got.Overridden) != 0 {
		t.Fatalf("默认设置不对: %+v", got)
	}
	// 校验：未知项、越界、非法枚举 → 400 且不生效
	owner.expect("PUT", "/admin/api/settings", map[string]any{"values": map[string]string{"nope": "1"}}, 400, "bad_request")
	owner.expect("PUT", "/admin/api/settings", map[string]any{"values": map[string]string{"queue_max_items": "0"}}, 400, "bad_request")
	owner.expect("PUT", "/admin/api/settings", map[string]any{"values": map[string]string{"queue_max_items": "5", "registration_mode": "weird"}}, 400, "bad_request")
	owner.expect("PUT", "/admin/api/settings", map[string]any{"values": map[string]string{"email_domains": "not a domain"}}, 400, "bad_request")
	owner.expect("PUT", "/admin/api/settings", map[string]any{}, 400, "bad_request")
	if e.api.Settings().QueueMaxItems != 10000 {
		t.Fatal("校验失败的请求不应部分生效")
	}
	// 队列配额：改成 1 条后立即生效，不需要重启
	if err := send(); err != "" {
		t.Fatalf("第一条应成功: %s", err)
	}
	if err := send(); err != "" {
		t.Fatalf("默认配额下第二条应成功: %s", err)
	}
	ackAll(b)
	owner.must("PUT", "/admin/api/settings", map[string]any{"values": map[string]string{"queue_max_items": "1"}}, &got)
	if got.Values["queue_max_items"] != "1" || len(got.Overridden) != 1 {
		t.Fatalf("保存后的返回不对: %+v", got)
	}
	if err := send(); err != "" {
		t.Fatalf("配额 1：第一条应成功: %s", err)
	}
	if err := send(); err != "too_large" {
		t.Fatalf("配额 1：第二条应被拒收，得到 %q", err)
	}
	// 限速：写操作每分钟 1 次
	setSettings(t, owner, map[string]string{"writes_per_minute": "1"})
	a.must("PUT", "/v1/me/profile", map[string]any{"about": "1"}, nil)
	a.expect("PUT", "/v1/me/profile", map[string]any{"about": "2"}, 429, "rate_limited")
	// 上传配额
	setSettings(t, owner, map[string]string{"daily_upload_bytes": "1048576"})
	a.upload(bytes.Repeat([]byte("x"), 900<<10))
	a.expect("POST", "/v1/attachments", bytes.Repeat([]byte("x"), 200<<10), 429, "rate_limited")
	// 令牌有效期：改成 1 小时后，2 小时没用过的令牌失效
	setSettings(t, owner, map[string]string{"token_ttl_hours": "1"})
	a.must("GET", "/v1/me", nil, nil)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dataDir, "hush.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE tokens SET last_used_at = ? WHERE user_id = ?", time.Now().Add(-2*time.Hour).UnixMilli(), b.ID); err != nil {
		t.Fatal(err)
	}
	e.st.InvalidateAuthCache() // 上面绕过服务端直接改了库
	b.expect("GET", "/v1/me", nil, 401, "unauthorized")
	// 审计记录了改动
	au := e.audited("settings.update", "settings")
	if !strings.Contains(au.Detail, "token_ttl_hours") {
		t.Fatalf("审计应含修改项: %+v", au)
	}
	// 持久化：重启（新建 api.Server 后 LoadSettings）仍然生效
	fresh := api.NewServer(api.Config{Store: e.st, Hub: e.hub, DataDir: e.dataDir})
	if skipped, err := fresh.LoadSettings(context.Background()); err != nil || len(skipped) != 0 {
		t.Fatalf("LoadSettings: %v %v", skipped, err)
	}
	if s := fresh.Settings(); s.QueueMaxItems != 1 || s.WritesPerMinute != 1 || s.TokenTTL != time.Hour || s.DailyUploadBytes != 1<<20 {
		t.Fatalf("重启后设置未生效: %+v", s)
	}
	// 恢复默认
	owner.must("PUT", "/admin/api/settings", map[string]any{"reset": []string{"queue_max_items", "writes_per_minute"}}, &got)
	if got.Values["queue_max_items"] != "10000" || e.api.Settings().QueueMaxItems != 10000 {
		t.Fatalf("恢复默认不生效: %+v", got.Values)
	}
	for _, k := range got.Overridden {
		if k == "queue_max_items" {
			t.Fatal("恢复默认后不应再是覆盖项")
		}
	}
}

func TestStorageCleanupAndBackups(t *testing.T) {
	e := newEnv(t)
	owner := e.mkAdmin("owner1", store.AdminOwner)
	a, b := e.register("alice"), e.register("bob")
	a.upload(bytes.Repeat([]byte("a"), 3000))
	a.upload(bytes.Repeat([]byte("a"), 2000))
	b.upload(bytes.Repeat([]byte("b"), 1000))
	var st struct {
		Attachments     int64            `json:"attachments"`
		AttachmentBytes int64            `json:"attachment_bytes"`
		DBBytes         int64            `json:"db_bytes"`
		Top             []store.Uploader `json:"top_uploaders"`
		Backups         []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"backups"`
	}
	owner.must("GET", "/admin/api/storage", nil, &st)
	if st.Attachments != 3 || st.AttachmentBytes != 6000 || st.DBBytes == 0 || len(st.Top) != 2 ||
		st.Top[0].UserID != a.ID || st.Top[0].Bytes != 5000 || st.Top[0].Files != 2 || st.Top[0].Email != a.Email || len(st.Backups) != 0 {
		t.Fatalf("存储概况不对: %+v", st)
	}
	// 手动清理：残留的过期上传临时文件被清掉
	tmp := filepath.Join(e.dataDir, "attachments", ".upload-stale")
	os.WriteFile(tmp, []byte("x"), 0o600)
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(tmp, oldTime, oldTime)
	var cs struct {
		TempFiles int `json:"temp_files"`
	}
	owner.must("POST", "/admin/api/maintenance/cleanup", nil, &cs)
	if cs.TempFiles != 1 {
		t.Fatalf("清理结果不对: %+v", cs)
	}
	e.audited("maintenance.cleanup", "")

	// 在线备份：文件可用、权限 0600、只保留最近 N 份
	setSettings(t, owner, map[string]string{"backup_keep": "1"})
	var bk struct {
		Backup struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"backup"`
		Removed []string `json:"removed"`
	}
	owner.must("POST", "/admin/api/backups", nil, &bk)
	first := bk.Backup.Name
	path := filepath.Join(e.dataDir, "backups", first)
	info, err := os.Stat(path)
	if err != nil || info.Size() != bk.Backup.Size || info.Size() == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("备份文件不对: %v %+v", err, info)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var users int
	var integrity string
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users); err != nil || users != 2 {
		t.Fatalf("备份里的数据不对: %d %v", users, err)
	}
	db.QueryRow("PRAGMA integrity_check").Scan(&integrity)
	db.Close()
	if integrity != "ok" {
		t.Fatalf("备份完整性检查失败: %s", integrity)
	}
	e.audited("backup.create", "backup:"+first)
	time.Sleep(1100 * time.Millisecond) // 文件名精确到秒
	owner.must("POST", "/admin/api/backups", nil, &bk)
	if len(bk.Removed) != 1 || bk.Removed[0] != first {
		t.Fatalf("应删除超出保留份数的旧备份: %+v", bk)
	}
	owner.must("GET", "/admin/api/storage", nil, &st)
	if len(st.Backups) != 1 || st.Backups[0].Name != bk.Backup.Name {
		t.Fatalf("备份列表不对: %+v", st.Backups)
	}
	// 下载：owner 可下载并记审计；文件名严格校验，不能穿越目录
	status, body, h := owner.raw("GET", "/admin/api/backups/"+bk.Backup.Name, nil, nil)
	if status != 200 || !bytes.HasPrefix(body, []byte("SQLite format 3")) || !strings.Contains(h.Get("Content-Disposition"), bk.Backup.Name) {
		t.Fatalf("下载备份失败: %d %v", status, h)
	}
	e.audited("backup.download", "backup:"+bk.Backup.Name)
	for _, name := range []string{first, "..%2Fhush.db", "hush.db", "hush-00000000-000000.db", "..%2F..%2Fturn_secret"} {
		owner.expect("GET", "/admin/api/backups/"+name, nil, 404, "not_found")
	}
}

func TestDashboardMonitorAndMetrics(t *testing.T) {
	e := newEnv(t, func(_ *api.Config, m *admin.Config) { m.MetricsToken = "scrape-token" })
	viewer := e.mkAdmin("viewer1", store.AdminViewer)
	a, b, c := e.register("alice"), e.register("bob"), e.register("carol")
	befriend(t, a, b)
	befriend(t, a, c)
	gid := a.createGroup("群", b, c)
	a.post("hi")
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 0, "body": "QUJD"}}}, nil)
	a.must("POST", "/v1/groups/"+gid+"/messages", map[string]any{"session_id": "s", "body": "QUJD"}, nil)
	a.dial()
	e.apiDo("GET", "/v1/nope", "", nil)
	// 一条已结束的 WS 连接（请求在连接结束时才计入指标）
	cw := c.dial()
	cw.conn.CloseNow()
	for deadline := time.Now().Add(3 * time.Second); e.api.Metrics().Snapshot().Requests < 13 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}

	var d struct {
		Totals store.Totals `json:"totals"`
		Online int          `json:"online"`
		Uptime int64        `json:"uptime_seconds"`
		Ver    string       `json:"version"`
		Trend  []struct {
			Day           string `json:"day"`
			Registrations int64  `json:"registrations"`
			Active        int64  `json:"active"`
		} `json:"trend"`
		Hourly []struct {
			TS        int64 `json:"ts"`
			Envelopes int64 `json:"envelopes"`
		} `json:"hourly"`
	}
	viewer.must("GET", "/admin/api/dashboard", nil, &d)
	tt := d.Totals
	if tt.Users != 3 || tt.NewToday != 3 || tt.ActiveToday != 3 || tt.Groups != 1 || tt.Moments != 1 || tt.QueueItems == 0 || tt.QueueBytes == 0 ||
		d.Online != 1 || d.Ver != "test" {
		t.Fatalf("仪表盘计数不对: %+v online=%d", tt, d.Online)
	}
	if len(d.Trend) != 30 || d.Trend[29].Day != time.Now().Format("2006-01-02") || d.Trend[29].Registrations != 3 || d.Trend[29].Active != 3 || d.Trend[0].Registrations != 0 {
		t.Fatalf("趋势不对: %+v", d.Trend[29])
	}
	// 1 条单聊 + 群消息扇出给 2 人 = 3 条信封
	if len(d.Hourly) != 24 || d.Hourly[23].Envelopes != 3 {
		t.Fatalf("每小时投递数不对: %+v", d.Hourly[23])
	}
	var u store.AdminUserDetail
	viewer.must("GET", "/admin/api/users/"+a.ID, nil, &u)
	if u.LastActiveAt == 0 {
		t.Fatal("最近活跃时间应已记录")
	}

	var mon struct {
		Online  int `json:"online"`
		Metrics struct {
			Requests uint64 `json:"requests"`
			Routes   []struct {
				Route string  `json:"route"`
				Count uint64  `json:"count"`
				P50   float64 `json:"p50_ms"`
				P99   float64 `json:"p99_ms"`
			} `json:"routes"`
		} `json:"metrics"`
		DB      map[string]float64 `json:"db"`
		Runtime struct {
			Goroutines int    `json:"goroutines"`
			HeapAlloc  uint64 `json:"heap_alloc"`
		} `json:"runtime"`
		QueueItems int64 `json:"queue_items"`
	}
	viewer.must("GET", "/admin/api/monitor", nil, &mon)
	routes := map[string]uint64{}
	for _, r := range mon.Metrics.Routes {
		routes[r.Route] = r.Count
		if strings.Contains(r.Route, a.ID) || strings.Contains(r.Route, gid) {
			t.Fatalf("指标标签必须是路由模式，不能含原始 id: %s", r.Route)
		}
		if r.P99 < r.P50 {
			t.Fatalf("分位数不单调: %+v", r)
		}
	}
	if mon.Online != 1 || routes["POST /v1/auth/verify"] != 3 || routes["POST /v1/groups/{id}/messages"] != 1 || routes["unmatched"] != 1 ||
		routes["GET /v1/ws"] != 1 || mon.Runtime.Goroutines == 0 || mon.Runtime.HeapAlloc == 0 || mon.QueueItems == 0 {
		t.Fatalf("监控数据不对: %+v", mon)
	}
	if _, has := mon.DB["wait_count"]; !has {
		t.Fatalf("缺少数据库等待指标: %+v", mon.DB)
	}

	// /metrics：未认证 401；后台会话或抓取令牌可读；Prometheus 文本格式
	get := func(hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", e.admSrv.URL+"/metrics", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}
	if st, _ := get(nil); st != 401 {
		t.Fatalf("/metrics 未认证应为 401: %d", st)
	}
	if st, _ := get(map[string]string{"Authorization": "Bearer wrong"}); st != 401 {
		t.Fatalf("/metrics 错误令牌应为 401: %d", st)
	}
	st, text := get(map[string]string{"Authorization": "Bearer scrape-token"})
	if st != 200 {
		t.Fatalf("/metrics 抓取令牌应可读: %d", st)
	}
	for _, want := range []string{
		`hush_http_requests_total{route="POST /v1/auth/verify",code="200"} 3`,
		`# TYPE hush_http_request_duration_seconds histogram`,
		`hush_http_request_duration_seconds_bucket{route="POST /v1/auth/verify",le="+Inf"} 3`,
		`hush_http_request_duration_seconds_count{route="POST /v1/messages"} 1`,
		"hush_ws_connections 1", "hush_users 3", "hush_envelopes_enqueued_total 3", "go_goroutines ", "hush_queue_items ", "hush_db_wait_count ",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/metrics 缺少 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `route="GET /v1/ws",le=`) {
		t.Fatal("WebSocket 长连接不应进入耗时直方图")
	}
	if status, body, h := viewer.raw("GET", "/metrics", nil, nil); status != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || !bytes.Contains(body, []byte("hush_uptime_seconds")) {
		t.Fatalf("后台会话应可读 /metrics: %d", status)
	}
	// 主 API 监听地址上没有 /metrics，也没有任何 /admin 接口
	for _, p := range []string{"/metrics", "/admin/api/me", "/admin/api/users"} {
		if st, _ := e.apiDo("GET", p, "", nil); st != 404 {
			t.Fatalf("主 API 上不应有 %s: %d", p, st)
		}
	}
}

// 隐私边界：任何管理接口的响应里都不出现队列里的信封内容。
func TestNoAdminEndpointReturnsEnvelopeBodies(t *testing.T) {
	e := newEnv(t)
	owner := e.mkAdmin("owner1", store.AdminOwner)
	a, b := e.register("alice"), e.register("bob")
	befriend(t, a, b)
	gid := a.createGroup("群", b)
	const olmMarker, megolmMarker = "T0xNLUNJUEhFUlRFWFQtTUFSS0VS", "TUVHT0xNLUNJUEhFUlRFWFQtTUFSS0VS"
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 0, "body": olmMarker}}}, nil)
	a.must("POST", "/v1/groups/"+gid+"/messages", map[string]any{"session_id": "sess-marker", "body": megolmMarker}, nil)
	mid := a.post("公开动态")
	b.must("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": a.ID, "reason": "spam"}, nil)
	var bk struct {
		Backup struct {
			Name string `json:"name"`
		} `json:"backup"`
	}
	owner.must("POST", "/admin/api/backups", nil, &bk)
	markers := []string{olmMarker, megolmMarker, "sess-marker"}

	// 备份下载：原始字节里不出现任何信封内容（包括已删除行残留的空闲页），其余数据完整
	status, raw := owner.do("GET", "/admin/api/backups/"+bk.Backup.Name, nil)
	if status != 200 || !bytes.HasPrefix(raw, []byte("SQLite format 3")) {
		t.Fatalf("下载备份失败: %d", status)
	}
	for _, marker := range markers {
		if bytes.Contains(raw, []byte(marker)) {
			t.Fatalf("备份文件里包含信封内容 %q", marker)
		}
	}
	if !bytes.Contains(raw, []byte(b.Email)) || !bytes.Contains(raw, []byte("公开动态")) {
		t.Fatal("备份应保留账号与朋友圈数据")
	}
	entries, _ := os.ReadDir(filepath.Join(e.dataDir, "backups"))
	if len(entries) != 1 || entries[0].Name() != bk.Backup.Name {
		t.Fatalf("备份目录里不应有临时文件或日志文件: %v", entries)
	}
	bdb, err := sql.Open("sqlite", "file:"+filepath.Join(e.dataDir, "backups", bk.Backup.Name)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var envelopes, events, usageItems int
	var integrity string
	bdb.QueryRow("SELECT COUNT(*) FROM queue WHERE kind = 'envelope'").Scan(&envelopes)
	bdb.QueryRow("SELECT COUNT(*) FROM queue WHERE kind = 'event'").Scan(&events)
	bdb.QueryRow("SELECT COALESCE(SUM(items), 0) FROM queue_usage").Scan(&usageItems)
	bdb.QueryRow("PRAGMA integrity_check").Scan(&integrity)
	bdb.Close()
	if envelopes != 0 || events == 0 || usageItems != events || integrity != "ok" {
		t.Fatalf("备份里的队列不对: 信封 %d 事件 %d 用量 %d 完整性 %s", envelopes, events, usageItems, integrity)
	}
	// 线上库不受影响：信封还在，照常投递
	var inbox struct {
		Messages []map[string]any `json:"messages"`
	}
	b.must("GET", "/v1/messages?limit=100", nil, &inbox)
	if len(inbox.Messages) != 2 {
		t.Fatalf("生成备份不应影响线上队列: %d", len(inbox.Messages))
	}

	fill := map[string]string{"/admin/api/users/{id}": b.ID, "/admin/api/groups/{id}": gid, "/admin/api/moments/{id}/comments": mid}
	checked := 0
	for _, rt := range e.adm.Routes() {
		if rt.Method != "GET" || strings.Contains(rt.Path, "/backups/") || strings.Contains(rt.Path, "/media/") || strings.Contains(rt.Path, "/invites/{code}") {
			continue // 备份下载已在上面单独检查；其余为图片 / 需要特定参数
		}
		path := rt.Path
		if id, ok := fill[rt.Path]; ok {
			path = strings.Replace(strings.Replace(path, "{id}", id, 1), "{uid}", id, 1)
		}
		for _, q := range []string{"", "?q=" + b.ID[:8], "?status=pending", "?size=100"} {
			status, body := owner.do("GET", path+q, nil)
			if status != 200 {
				t.Fatalf("GET %s%s: %d %s", path, q, status, body)
			}
			for _, marker := range markers {
				if bytes.Contains(body, []byte(marker)) {
					t.Fatalf("GET %s%s 的响应包含信封内容 %q", path, q, marker)
				}
			}
			checked++
		}
	}
	if status, body := owner.do("GET", "/metrics", nil); status != 200 || bytes.Contains(body, []byte(olmMarker)) {
		t.Fatalf("/metrics 不应含信封内容: %d", status)
	}
	if checked < 40 {
		t.Fatalf("检查的接口太少: %d", checked)
	}
	// 用户详情里确实只有计数与字节数
	var d map[string]any
	owner.must("GET", "/admin/api/users/"+b.ID, nil, &d)
	if d["queue_items"].(float64) < 2 || d["queue_bytes"].(float64) <= 0 {
		t.Fatalf("应展示队列条数与字节数: %v", d)
	}
	for k := range d {
		if strings.Contains(k, "payload") || strings.Contains(k, "body") || strings.Contains(k, "envelope") {
			t.Fatalf("用户详情不应有消息内容字段: %s", k)
		}
	}
}
