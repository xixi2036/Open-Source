package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"hush/server/internal/admin"
	"hush/server/internal/api"
	"hush/server/internal/netx"
	"hush/server/internal/store"
)

// P5：pprof 只在管理监听地址上，且需要 owner 会话或指标令牌。
func TestPprofRequiresOwnerOrToken(t *testing.T) {
	e := newEnv(t, func(_ *api.Config, m *admin.Config) { m.MetricsToken = "metrics-secret" })
	owner := e.mkAdmin("root", store.AdminOwner)
	viewer := e.mkAdmin("view", store.AdminViewer)
	get := func(hdr map[string]string) int {
		req, _ := http.NewRequest("GET", e.admSrv.URL+"/debug/pprof/cmdline", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(nil); got != 401 {
		t.Fatalf("未登录访问 pprof 应 401，得到 %d", got)
	}
	if got := get(map[string]string{"Authorization": "Bearer wrong"}); got != 401 {
		t.Fatalf("错误令牌应 401，得到 %d", got)
	}
	if got := get(map[string]string{"Authorization": "Bearer metrics-secret"}); got != 200 {
		t.Fatalf("指标令牌应可访问 pprof，得到 %d", got)
	}
	if st, _, _ := viewer.raw("GET", "/debug/pprof/cmdline", nil, nil); st != 403 {
		t.Fatalf("viewer 不能访问 pprof，得到 %d", st)
	}
	if st, _, _ := owner.raw("GET", "/debug/pprof/cmdline", nil, nil); st != 200 {
		t.Fatalf("owner 应可访问 pprof，得到 %d", st)
	}
	// 客户端监听地址上没有 pprof
	if st, _ := e.apiDo("GET", "/debug/pprof/", "", nil); st != 404 {
		t.Fatalf("客户端地址上不应有 pprof，得到 %d", st)
	}
}

// B1：配置可信代理后，后台登录限速按 X-Forwarded-For 里的客户端 IP 分桶，审计记录的也是客户端 IP。
func TestAdminLoginLimitBehindTrustedProxy(t *testing.T) {
	proxies, _ := netx.ParseProxies("127.0.0.1")
	e := newEnv(t, func(_ *api.Config, m *admin.Config) { m.LoginPerIPPerMinute = 2; m.TrustedProxies = proxies })
	login := func(xff string) int {
		req, _ := http.NewRequest("POST", e.admSrv.URL+"/admin/api/login", strings.NewReader(`{"username":"nobody","password":"wrong-password-123"}`))
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("Origin", e.admSrv.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	login("198.51.100.1")
	login("198.51.100.1")
	if got := login("198.51.100.1"); got != 429 {
		t.Fatalf("同一客户端 IP 第 3 次应 429，得到 %d", got)
	}
	if got := login("198.51.100.2"); got != 401 {
		t.Fatalf("另一个客户端 IP 不应被限速（应为 401），得到 %d", got)
	}
}

// 举报列表：对象摘要批量解析，结果与逐条解析一致（含已删除的对象）。
func TestReportListBatchTargets(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod", store.AdminModerator)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	mid := b.post("违规内容")
	gid := b.createGroup("群", a)
	for _, body := range []map[string]any{
		{"target_type": "user", "target_id": b.ID, "reason": "骚扰"},
		{"target_type": "moment", "target_id": mid, "reason": "低俗"},
		{"target_type": "group", "target_id": gid, "reason": "诈骗"},
	} {
		a.must("POST", "/v1/reports", body, nil)
	}
	b.must("DELETE", "/v1/moments/"+mid, nil, nil)
	var res struct {
		Items []struct {
			TargetType string             `json:"target_type"`
			Target     store.ReportTarget `json:"target"`
		} `json:"items"`
	}
	mod.must("GET", "/admin/api/reports", nil, &res)
	if len(res.Items) != 3 {
		t.Fatalf("应有 3 条举报: %+v", res)
	}
	for _, it := range res.Items {
		switch it.TargetType {
		case "user":
			if !it.Target.Exists || it.Target.OwnerID != b.ID {
				t.Fatalf("用户举报的对象不对: %+v", it.Target)
			}
		case "group":
			if !it.Target.Exists || it.Target.Summary != "群" || it.Target.OwnerID != b.ID {
				t.Fatalf("群举报的对象不对: %+v", it.Target)
			}
		case "moment":
			if it.Target.Exists {
				t.Fatalf("已删除的动态应标记为不存在: %+v", it.Target)
			}
		}
	}
}

// 公告分批扇出：用户数超过一批也全部收到；扇出中断后（进度已落库）重启续发，不重复、不遗漏。
func TestAnnouncementFanoutBatchedAndResumable(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod", store.AdminModerator)
	ctx := context.Background()
	n := store.AnnouncementFanoutBatch + 37
	for i := 0; i < n; i++ {
		if _, err := e.st.Login(ctx, fmt.Sprintf("bulk%d@example.com", i), "android", "t", fmt.Sprintf("bulk-token-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	online := e.register("online")
	conn := online.dial()
	var res struct {
		Announcement store.Announcement `json:"announcement"`
		Recipients   int                `json:"recipients"`
	}
	mod.must("POST", "/admin/api/announcements", map[string]any{"title": "t", "body": "b"}, &res)
	if res.Recipients != n+1 {
		t.Fatalf("应发给全部 %d 个用户，得到 %d", n+1, res.Recipients)
	}
	if ev := conn.event("announcement"); ev["id"] != res.Announcement.ID {
		t.Fatalf("在线用户应实时收到: %v", ev)
	}
	var queued, copies int
	if err := e.db().QueryRow("SELECT COUNT(*) FROM queue WHERE id = ?", res.Announcement.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := e.db().QueryRow("SELECT COUNT(*) FROM envelopes WHERE id = ?", res.Announcement.ID).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if queued != n+1 || copies != 1 {
		t.Fatalf("每人一条队列引用、正文只存一份：%d 条 / %d 份", queued, copies)
	}
	// 模拟「发到一半进程退出」：把一条已完成的公告的进度改回一半，并删掉后半段用户的事件
	var cursor string
	if err := e.db().QueryRow("SELECT id FROM users ORDER BY id LIMIT 1 OFFSET 200").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db().Exec("DELETE FROM queue WHERE id = ? AND user_id > ?", res.Announcement.ID, cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db().Exec("UPDATE announcements SET fanout_done = 0, fanout_cursor = ?, recipients = 201 WHERE id = ?", cursor, res.Announcement.ID); err != nil {
		t.Fatal(err)
	}
	if resumed, err := e.st.ResumeAnnouncementFanouts(ctx); err != nil || resumed != 1 {
		t.Fatalf("应续发 1 条公告: %d %v", resumed, err)
	}
	if err := e.db().QueryRow("SELECT COUNT(*) FROM queue WHERE id = ?", res.Announcement.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != n+1 {
		t.Fatalf("续发后每人恰好一条，得到 %d", queued)
	}
	if resumed, _ := e.st.ResumeAnnouncementFanouts(ctx); resumed != 0 {
		t.Fatal("已完成的公告不应再续发")
	}
}

// db 直接打开测试库（绕过服务端，用来核对表里的数据）。
func (e *env) db() *sql.DB {
	e.t.Helper()
	if e.rawDB == nil {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dataDir, "hush.db")+"?_pragma=busy_timeout(5000)")
		if err != nil {
			e.t.Fatal(err)
		}
		e.rawDB = db
		e.t.Cleanup(func() { db.Close() })
	}
	return e.rawDB
}
