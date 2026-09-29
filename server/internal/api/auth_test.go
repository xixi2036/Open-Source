package api_test

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAuthCodeFlow(t *testing.T) {
	e := newEnv(t, false)
	email := "Alice@Example.com"
	norm := "alice@example.com"

	if st, body := e.do("POST", "/v1/auth/code", "", map[string]any{"email": email}); st != 200 || string(body) != "{}\n" {
		t.Fatalf("request code: %d %s", st, body)
	}
	code := e.mailer.code(norm)
	if len(code) != 6 {
		t.Fatalf("验证码应为 6 位数字，得到 %q", code)
	}
	// 60 秒内限发一次
	st, body := e.do("POST", "/v1/auth/code", "", map[string]any{"email": norm})
	assertErr(t, st, body, 429, "rate_limited")

	// 非法邮箱
	st, body = e.do("POST", "/v1/auth/code", "", map[string]any{"email": "not-an-email"})
	assertErr(t, st, body, 400, "bad_request")

	// 错误验证码
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	verify := func(c string) (int, []byte) {
		return e.do("POST", "/v1/auth/verify", "", map[string]any{"email": email, "code": c, "device_name": "Pixel", "platform": "android"})
	}
	st, body = verify(wrong)
	assertErr(t, st, body, 401, "unauthorized")

	// 正确验证码 → 新用户
	st, body = verify(code)
	if st != 200 {
		t.Fatalf("verify: %d %s", st, body)
	}
	var res struct {
		UserID string `json:"user_id"`
		Token  string `json:"token"`
		IsNew  bool   `json:"is_new"`
	}
	json.Unmarshal(body, &res)
	if res.UserID == "" || res.Token == "" || !res.IsNew {
		t.Fatalf("verify 响应异常: %s", body)
	}
	// 验证码一次性
	st, body = verify(code)
	assertErr(t, st, body, 401, "unauthorized")

	// /v1/me 返回 Profile + email
	c := &client{e: e, ID: res.UserID, Token: res.Token}
	var me map[string]any
	c.must("GET", "/v1/me", nil, &me)
	for _, k := range []string{"id", "name", "about", "avatar_id", "updated_at", "email"} {
		if _, found := me[k]; !found {
			t.Fatalf("/v1/me 缺少字段 %s: %v", k, me)
		}
	}
	if me["email"] != norm || me["id"] != res.UserID || me["avatar_id"] != nil {
		t.Fatalf("/v1/me 内容异常: %v", me)
	}

}

func TestAuthCodeInvalidatedAfterFiveFailures(t *testing.T) {
	e := newEnv(t, false)
	email := "bob@example.com"
	e.do("POST", "/v1/auth/code", "", map[string]any{"email": email})
	code := e.mailer.code(email)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		e.do("POST", "/v1/auth/verify", "", map[string]any{"email": email, "code": wrong, "platform": "ios"})
	}
	st, body := e.do("POST", "/v1/auth/verify", "", map[string]any{"email": email, "code": code, "platform": "ios"})
	assertErr(t, st, body, 401, "unauthorized")
}

func TestDevCodeAlwaysValid(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("dev")
	b := e.register("dev")
	if a.ID == "" || b.ID == "" || a.ID == b.ID {
		t.Fatal("dev code 注册失败")
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	a.must("POST", "/v1/auth/logout", nil, nil)
	a.expectErr("GET", "/v1/me", nil, 401, "unauthorized")
	// 无 token
	st, body := e.do("GET", "/v1/me", "", nil)
	assertErr(t, st, body, 401, "unauthorized")
}

func TestSingleDeviceLogin(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("alice")
	b := e.register("bob")
	befriend(t, a, b)

	a.must("PUT", "/v1/keys", keyUpload("IK-A", "SK-A", 5, true), nil)
	var id1 map[string]any
	b.must("GET", "/v1/keys/identity/"+a.ID, nil, &id1)
	if id1["device_generation"].(float64) != 1 {
		t.Fatalf("初始 device_generation 应为 1: %v", id1)
	}

	// B 给 A 发一条消息，A 未取走
	b.must("POST", "/v1/messages", olmMsg(a.ID, "pending-msg", true), nil)

	oldWS := a.dial()
	oldWS.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	bws := b.dial()
	bws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	// A 在另一台设备登录
	a2 := e.login(a.Email)
	if a2.ID != a.ID || a2.Token == a.Token {
		t.Fatal("重新登录应返回同一 user_id 与新 token")
	}

	// 旧连接收到 logged_out 后被关闭
	f := oldWS.until("logged_out", func(f frame) bool { return f.T == "event" })
	ev := f.event()
	if ev["type"] != "logged_out" || ev["reason"] != "logged_in_elsewhere" {
		t.Fatalf("期望 logged_out，得到 %s", f.D)
	}
	if _, err := oldWS.tryRead(2 * time.Second); err == nil {
		t.Fatal("旧连接应被关闭")
	}

	// 旧 token 失效
	a.expectErr("GET", "/v1/me", nil, 401, "unauthorized")
	a2.must("GET", "/v1/me", nil, nil)

	// 好友收到 key_changed
	kc := bws.event("key_changed")
	if kc["user_id"] != a.ID || kc["id"] == nil {
		t.Fatalf("key_changed 内容异常: %v", kc)
	}

	// 旧预密钥与未投递消息被清空
	var cnt map[string]int
	a2.must("GET", "/v1/keys/count", nil, &cnt)
	if cnt["one_time_key_count"] != 0 {
		t.Fatalf("旧 OTK 应清空: %v", cnt)
	}
	b.expectErr("GET", "/v1/keys/identity/"+a.ID, nil, 404, "not_found")
	var inbox inboxResp
	a2.must("GET", "/v1/messages", nil, &inbox)
	if len(inbox.Messages) != 0 {
		t.Fatalf("旧未投递消息应清空: %+v", inbox)
	}

	// 新设备上传新身份 → device_generation=2
	a2.must("PUT", "/v1/keys", keyUpload("IK-A2", "SK-A2", 1, false), nil)
	var id2 map[string]any
	b.must("GET", "/v1/keys/identity/"+a.ID, nil, &id2)
	if id2["device_generation"].(float64) != 2 || id2["identity_key"] != "IK-A2" {
		t.Fatalf("新身份异常: %v", id2)
	}
}

func assertErr(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	var eb struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	json.Unmarshal(body, &eb)
	if status != wantStatus || eb.Error != wantCode || eb.Message == "" {
		t.Fatalf("期望 %d/%s，得到 %d %s", wantStatus, wantCode, status, body)
	}
}

type inboxResp struct {
	Messages []envelope       `json:"messages"`
	Events   []map[string]any `json:"events"`
}
