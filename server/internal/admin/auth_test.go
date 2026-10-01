package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hush/server/internal/admin"
	"hush/server/internal/api"
	"hush/server/internal/store"
)

// 每一条管理接口：未登录 401、角色不足 403、写操作缺 CSRF 令牌 403、跨源写请求 403。
func TestEveryRouteEnforcesAuth(t *testing.T) {
	e := newEnv(t)
	sessions := map[string]*session{
		store.AdminViewer:    e.mkAdmin("viewer1", store.AdminViewer),
		store.AdminModerator: e.mkAdmin("mod1", store.AdminModerator),
		store.AdminOwner:     e.mkAdmin("owner1", store.AdminOwner),
	}
	rank := map[string]int{store.AdminViewer: 1, store.AdminModerator: 2, store.AdminOwner: 3}
	anon := &session{e: e, client: http.DefaultClient}
	routes := e.adm.Routes()
	if len(routes) < 40 {
		t.Fatalf("路由数异常: %d", len(routes))
	}
	for _, rt := range routes {
		path := rt.Path
		for strings.Contains(path, "{") {
			i, j := strings.Index(path, "{"), strings.Index(path, "}")
			path = path[:i] + "x" + path[j+1:]
		}
		name := rt.Method + " " + rt.Path
		if status, body := anon.do(rt.Method, path, nil); status != 401 {
			t.Errorf("%s 未登录：期望 401，得到 %d %s", name, status, body)
		}
		for role, s := range sessions {
			if rank[role] >= rank[rt.MinRole] {
				continue
			}
			if status, body := s.do(rt.Method, path, map[string]any{}); status != 403 || !bytes.Contains(body, []byte(`"forbidden"`)) {
				t.Errorf("%s 角色 %s：期望 403 forbidden，得到 %d %s", name, role, status, body)
			}
		}
		if rt.Method == "GET" {
			continue
		}
		owner := sessions[store.AdminOwner]
		if status, body, _ := owner.raw(rt.Method, path, map[string]any{}, map[string]string{"X-CSRF-Token": ""}); status != 403 || !bytes.Contains(body, []byte(`"csrf"`)) {
			t.Errorf("%s 缺 CSRF 令牌：期望 403 csrf，得到 %d %s", name, status, body)
		}
		if status, body, _ := owner.raw(rt.Method, path, map[string]any{}, map[string]string{"X-CSRF-Token": "wrong"}); status != 403 {
			t.Errorf("%s 错误 CSRF 令牌：期望 403，得到 %d %s", name, status, body)
		}
		if status, body, _ := owner.raw(rt.Method, path, map[string]any{}, map[string]string{"Origin": "http://127.0.0.1:1"}); status != 403 || !bytes.Contains(body, []byte(`"csrf"`)) {
			t.Errorf("%s 跨源写请求：期望 403 csrf，得到 %d %s", name, status, body)
		}
	}
	// 以上被拒绝的写请求都不应产生副作用或审计记录（只有三次登录）
	list, _, _ := e.st.ListAudit(context.Background(), store.AuditFilter{}, 1, 100)
	for _, a := range list {
		if a.Action != "auth.login" {
			t.Errorf("被拒绝的请求不应写审计: %+v", a)
		}
	}
}

func TestLoginSessionAndHeaders(t *testing.T) {
	e := newEnv(t)
	// 没有默认账号
	for _, cred := range [][2]string{{"admin", "admin"}, {"admin", "admin123456789"}, {"root", "password1234"}} {
		if _, status, _ := e.loginAs(cred[0], cred[1]); status != 401 {
			t.Fatalf("不应存在默认账号 %v: %d", cred, status)
		}
	}
	s := e.mkAdmin("boss", store.AdminOwner)
	if s.csrf == "" || s.admin.Role != store.AdminOwner {
		t.Fatalf("登录响应缺少 csrf / admin: %+v", s)
	}
	// 登录响应里不含密码哈希
	_, _, body := e.loginAs("boss", testPassword)
	if bytes.Contains(body, []byte("$2a$")) || bytes.Contains(bytes.ToLower(body), []byte("password")) {
		t.Fatalf("登录响应泄露密码哈希: %s", body)
	}
	// Cookie 属性
	resp, err := http.Post(e.admSrv.URL+"/admin/api/login", "application/json",
		strings.NewReader(`{"username":"boss","password":"`+testPassword+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ck := resp.Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteStrictMode || ck[0].Path != "/" {
		t.Fatalf("会话 Cookie 属性不对: %+v", ck)
	}
	// 库里只存哈希，不存令牌本身
	if _, err := e.st.AdminSession(context.Background(), ck[0].Value, time.Now().UnixMilli(), time.Hour); err == nil {
		t.Fatal("admin_sessions 不应以明文令牌为键")
	}
	// 安全响应头（页面与接口都有）
	for _, path := range []string{"/", "/static/app.js", "/admin/api/me"} {
		status, _, h := s.raw("GET", path, nil, nil)
		if status != 200 {
			t.Fatalf("GET %s: %d", path, status)
		}
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s CSP 不对: %q", path, csp)
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s 缺少安全响应头: %v", path, h)
		}
	}
	// me 返回同一个 CSRF 令牌（页面刷新后取回）
	var me struct {
		CSRF  string      `json:"csrf"`
		Admin store.Admin `json:"admin"`
	}
	s.must("GET", "/admin/api/me", nil, &me)
	if me.CSRF != s.csrf || me.Admin.Username != "boss" {
		t.Fatalf("me 不对: %+v", me)
	}
	// 12 小时无操作过期；期间有操作则续期
	e.clock.advance(11 * time.Hour)
	s.must("GET", "/admin/api/me", nil, nil)
	e.clock.advance(11 * time.Hour)
	s.must("GET", "/admin/api/me", nil, nil)
	e.clock.advance(12*time.Hour + time.Minute)
	s.expect("GET", "/admin/api/me", nil, 401, "unauthorized")
	// 退出登录后会话立即失效
	s2, _, _ := e.loginAs("boss", testPassword)
	s2.must("POST", "/admin/api/logout", nil, nil)
	s2.expect("GET", "/admin/api/me", nil, 401, "unauthorized")
	e.audited("auth.login", "admin:")
	e.audited("auth.logout", "admin:")
	// 登录接口拒绝跨源请求
	req, _ := http.NewRequest("POST", e.admSrv.URL+"/admin/api/login", strings.NewReader(`{"username":"boss","password":"`+testPassword+`"}`))
	req.Header.Set("Origin", "http://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("跨源登录应被拒绝: %d", resp.StatusCode)
	}
	// 未知接口统一 JSON 404
	s3, _, _ := e.loginAs("boss", testPassword)
	s3.expect("GET", "/admin/api/nope", nil, 404, "not_found")
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t, func(_ *api.Config, m *admin.Config) { m.LoginPerIPPerMinute = 5 })
	e.clock.advance(time.Hour)
	for i := 0; i < 5; i++ {
		if _, status, _ := e.loginAs("nobody", "whatever-password"); status != 401 {
			t.Fatalf("第 %d 次应为 401，得到 %d", i+1, status)
		}
	}
	if _, status, body := e.loginAs("nobody", "whatever-password"); status != 429 || !bytes.Contains(body, []byte("rate_limited")) {
		t.Fatalf("第 6 次应被限速: %d %s", status, body)
	}
	e.clock.advance(61 * time.Second)
	if _, status, _ := e.loginAs("nobody", "whatever-password"); status != 401 {
		t.Fatalf("下一分钟应恢复: %d", status)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	e.mkAdmin("alice", store.AdminOwner)
	for i := 0; i < 9; i++ {
		if _, status, _ := e.loginAs("alice", "wrong-password-000"); status != 401 {
			t.Fatalf("第 %d 次应为 401，得到 %d", i+1, status)
		}
	}
	// 第 9 次失败后仍可用正确密码登录，且成功登录清零计数
	if _, status, _ := e.loginAs("alice", testPassword); status != 200 {
		t.Fatalf("未达上限时应能登录: %d", status)
	}
	for i := 0; i < 10; i++ {
		e.loginAs("alice", "wrong-password-000")
	}
	// 连续 10 次失败 → 锁定 15 分钟，正确密码也拒绝
	if _, status, body := e.loginAs("alice", testPassword); status != 429 || !bytes.Contains(body, []byte(`"locked"`)) {
		t.Fatalf("应已锁定: %d %s", status, body)
	}
	e.audited("auth.locked", "admin:")
	e.clock.advance(14 * time.Minute)
	if _, status, _ := e.loginAs("alice", testPassword); status != 429 {
		t.Fatalf("14 分钟时仍应锁定: %d", status)
	}
	e.clock.advance(2 * time.Minute)
	if _, status, _ := e.loginAs("alice", testPassword); status != 200 {
		t.Fatalf("15 分钟后应解锁: %d", status)
	}
	// 不存在的用户名表现一致（不能靠「是否会锁定」探测用户名）
	for i := 0; i < 10; i++ {
		if _, status, _ := e.loginAs("ghost", "wrong-password-000"); status != 401 {
			t.Fatalf("ghost 第 %d 次应为 401，得到 %d", i+1, status)
		}
	}
	if _, status, body := e.loginAs("ghost", "wrong-password-000"); status != 429 || !bytes.Contains(body, []byte(`"locked"`)) {
		t.Fatalf("不存在的用户名也应表现为锁定: %d %s", status, body)
	}
}

func TestAdminManagement(t *testing.T) {
	e := newEnv(t)
	owner := e.mkAdmin("owner1", store.AdminOwner)
	// 创建：校验用户名、角色、密码强度、重名
	owner.expect("POST", "/admin/api/admins", map[string]any{"username": "A B", "password": testPassword, "role": "viewer"}, 400, "bad_request")
	owner.expect("POST", "/admin/api/admins", map[string]any{"username": "mod", "password": "short", "role": "moderator"}, 400, "bad_request")
	owner.expect("POST", "/admin/api/admins", map[string]any{"username": "mod", "password": testPassword, "role": "god"}, 400, "bad_request")
	var mod store.Admin
	owner.must("POST", "/admin/api/admins", map[string]any{"username": "Mod", "password": testPassword, "role": "moderator"}, &mod)
	if mod.Username != "mod" || mod.Role != store.AdminModerator {
		t.Fatalf("创建结果不对: %+v", mod)
	}
	owner.expect("POST", "/admin/api/admins", map[string]any{"username": "mod", "password": testPassword, "role": "viewer"}, 409, "conflict")
	a := e.audited("admin.create", "admin:"+mod.ID)
	if strings.Contains(a.Detail, testPassword) {
		t.Fatalf("审计里不应有密码: %s", a.Detail)
	}
	status, body := owner.do("GET", "/admin/api/admins", nil)
	if status != 200 || bytes.Contains(body, []byte("$2a$")) || bytes.Contains(body, []byte("password")) {
		t.Fatalf("管理员列表泄露密码哈希: %d %s", status, body)
	}
	// 重置密码 → 对方会话失效，新密码可登录
	ms, st, _ := e.loginAs("mod", testPassword)
	if st != 200 {
		t.Fatal("mod 登录失败")
	}
	owner.must("PUT", "/admin/api/admins/"+mod.ID, map[string]any{"password": "new-password-123456"}, nil)
	ms.expect("GET", "/admin/api/me", nil, 401, "unauthorized")
	if _, st, _ := e.loginAs("mod", testPassword); st != 401 {
		t.Fatal("旧密码应失效")
	}
	ms, st, _ = e.loginAs("mod", "new-password-123456")
	if st != 200 {
		t.Fatal("新密码应可登录")
	}
	if a := e.audited("admin.update", "admin:"+mod.ID); strings.Contains(a.Detail, "new-password") {
		t.Fatalf("审计里不应有密码: %s", a.Detail)
	}
	// 停用 → 会话失效、无法登录；启用后恢复
	owner.must("PUT", "/admin/api/admins/"+mod.ID, map[string]any{"disabled": true}, nil)
	ms.expect("GET", "/admin/api/me", nil, 401, "unauthorized")
	if _, st, _ := e.loginAs("mod", "new-password-123456"); st != 401 {
		t.Fatal("停用的管理员不应能登录")
	}
	owner.must("PUT", "/admin/api/admins/"+mod.ID, map[string]any{"disabled": false, "role": "viewer"}, &mod)
	if mod.Role != store.AdminViewer || mod.Disabled {
		t.Fatalf("更新结果不对: %+v", mod)
	}
	// 最后一个 owner 不能被降级、停用、删除（包括自己操作自己）
	owner.expect("PUT", "/admin/api/admins/"+owner.admin.ID, map[string]any{"role": "viewer"}, 409, "conflict")
	owner.expect("PUT", "/admin/api/admins/"+owner.admin.ID, map[string]any{"disabled": true}, 409, "conflict")
	owner.expect("DELETE", "/admin/api/admins/"+owner.admin.ID, nil, 400, "bad_request")
	owner.must("GET", "/admin/api/me", nil, nil) // 被拒绝的修改没有踢掉自己的会话
	// 有第二个 owner 后可以降级
	var o2 store.Admin
	owner.must("POST", "/admin/api/admins", map[string]any{"username": "owner2", "password": testPassword, "role": "owner"}, &o2)
	o2s, _, _ := e.loginAs("owner2", testPassword)
	o2s.must("PUT", "/admin/api/admins/"+owner.admin.ID, map[string]any{"role": "viewer"}, nil)
	o2s.expect("DELETE", "/admin/api/admins/"+o2.ID, nil, 400, "bad_request")
	o2s.must("DELETE", "/admin/api/admins/"+mod.ID, nil, nil)
	o2s.expect("DELETE", "/admin/api/admins/"+mod.ID, nil, 404, "not_found")
	e.audited("admin.delete", "admin:"+mod.ID)
	// 自己改密码：原密码错误拒绝；成功后需重新登录
	o2s.expect("POST", "/admin/api/me/password", map[string]any{"old_password": "bad", "new_password": "another-password-1"}, 403, "forbidden")
	o2s.expect("POST", "/admin/api/me/password", map[string]any{"old_password": testPassword, "new_password": "short"}, 400, "bad_request")
	o2s.must("POST", "/admin/api/me/password", map[string]any{"old_password": testPassword, "new_password": "another-password-1"}, nil)
	o2s.expect("GET", "/admin/api/me", nil, 401, "unauthorized")
	o2s, st, _ = e.loginAs("owner2", "another-password-1")
	if st != 200 {
		t.Fatal("改密码后应能用新密码登录")
	}
	// 审计日志查询与筛选；没有任何删除审计的接口
	var page struct {
		Items []store.AuditEntry `json:"items"`
		Total int                `json:"total"`
	}
	o2s.must("GET", "/admin/api/audit?action=admin.&admin=owner1", nil, &page)
	if page.Total < 3 {
		t.Fatalf("审计筛选结果过少: %+v", page)
	}
	for _, it := range page.Items {
		if it.AdminName != "owner1" || !strings.HasPrefix(it.Action, "admin.") {
			t.Fatalf("筛选不生效: %+v", it)
		}
	}
	o2s.must("GET", "/admin/api/audit?from=1&to=2", nil, &page)
	if page.Total != 0 {
		t.Fatalf("时间筛选不生效: %d", page.Total)
	}
	for _, rt := range e.adm.Routes() {
		if strings.Contains(rt.Path, "audit") && rt.Method != "GET" {
			t.Fatalf("不应存在修改审计日志的接口: %+v", rt)
		}
	}
}

func TestCLICreateAdmin(t *testing.T) {
	dir := t.TempDir()
	run := func(password string, args ...string) (int, string) {
		t.Setenv("HUSH_ADMIN_PASSWORD", password)
		var out, errb bytes.Buffer
		code := admin.RunCLI(args, strings.NewReader(""), &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, msg := run("short", "create", "--data", dir, "--username", "root"); code == 0 || !strings.Contains(msg, "12") {
		t.Fatalf("弱密码应被拒绝: %d %s", code, msg)
	}
	if code, msg := run(testPassword, "create", "--data", dir, "--username", "x"); code == 0 {
		t.Fatalf("非法用户名应被拒绝: %s", msg)
	}
	if code, msg := run(testPassword, "create", "--data", dir+"/missing", "--username", "root"); code == 0 {
		t.Fatalf("数据目录不存在应失败: %s", msg)
	}
	if code, msg := run(testPassword, "create", "--data", dir, "--username", "root"); code != 0 {
		t.Fatalf("创建失败: %s", msg)
	}
	if code, _ := run(testPassword, "create", "--data", dir, "--username", "root"); code == 0 {
		t.Fatal("重名应失败")
	}
	// 密码也可以从标准输入读
	t.Setenv("HUSH_ADMIN_PASSWORD", "")
	var out, errb bytes.Buffer
	if code := admin.RunCLI([]string{"create", "--data", dir, "--username", "second", "--role", "viewer"}, strings.NewReader("stdin-password-123\n"), &out, &errb); code != 0 {
		t.Fatalf("从标准输入读密码失败: %s", errb.String())
	}
	if code, msg := run("reset-password-1234", "passwd", "--data", dir, "--username", "root"); code != 0 {
		t.Fatalf("重置密码失败: %s", msg)
	}
	if code, msg := run("", "list", "--data", dir); code != 0 || !strings.Contains(msg, "root\towner") || !strings.Contains(msg, "second\tviewer") {
		t.Fatalf("list 输出不对: %d %s", code, msg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.GetAdminByUsername(context.Background(), "root")
	if err != nil || a.Role != store.AdminOwner || !strings.HasPrefix(a.PasswordHash, "$2") || strings.Contains(a.PasswordHash, "reset-password") {
		t.Fatalf("管理员记录不对: %+v %v", a, err)
	}
	raw, _ := json.Marshal(a)
	if bytes.Contains(raw, []byte("$2")) {
		t.Fatalf("Admin 的 JSON 不应含密码哈希: %s", raw)
	}
}
