package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"hush/server/internal/api"
)

func TestAttachments(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")

	data := make([]byte, 200_000)
	rand.Read(data)
	var up struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	a.must("POST", "/v1/attachments", data, &up)
	if up.ID == "" || up.Size != int64(len(data)) {
		t.Fatalf("上传结果异常: %+v", up)
	}
	st, got := b.call("GET", "/v1/attachments/"+up.ID, nil, nil)
	if st != 200 || !bytes.Equal(got, data) {
		t.Fatalf("下载内容不一致: status=%d len=%d", st, len(got))
	}
	b.expectErr("GET", "/v1/attachments/00000000-0000-4000-8000-000000000000", nil, 404, "not_found")
	b.expectErr("GET", "/v1/attachments/..%2Fhush.db", nil, 404, "not_found")
	a.expectErr("POST", "/v1/attachments", []byte{}, 400, "bad_request")

	// 超过上限（测试环境配置为 1MB）
	a.expectErr("POST", "/v1/attachments", make([]byte, 1<<20+1), 413, "too_large")

	// 未认证
	st, body := e.do("GET", "/v1/attachments/"+up.ID, "", nil)
	assertErr(t, st, body, 401, "unauthorized")
}

func TestTurnCredentials(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	var tc struct {
		URLs       []string `json:"urls"`
		Username   string   `json:"username"`
		Credential string   `json:"credential"`
		TTL        int64    `json:"ttl"`
	}
	a.must("GET", "/v1/calls/turn", nil, &tc)
	if len(tc.URLs) != 2 || tc.URLs[0] != "stun:127.0.0.1:3478" || tc.URLs[1] != "turn:127.0.0.1:3478?transport=udp" {
		t.Fatalf("TURN urls 异常: %v", tc.URLs)
	}
	if tc.TTL != 3600 {
		t.Fatalf("ttl 应为 3600 秒: %d", tc.TTL)
	}
	expStr, uid, found := strings.Cut(tc.Username, ":")
	if !found || uid != a.ID {
		t.Fatalf("username 应为 <expiry>:<user_id>: %q", tc.Username)
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp < time.Now().Unix()+3500 || exp > time.Now().Unix()+3700 {
		t.Fatalf("expiry 应为 now+ttl 的 Unix 秒: %q", expStr)
	}
	mac := hmac.New(sha1.New, []byte(testTurnSecret))
	mac.Write([]byte(tc.Username))
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); tc.Credential != want {
		t.Fatalf("credential 应为 base64(HMAC-SHA1(secret, username))：%s != %s", tc.Credential, want)
	}
}

func TestProfileAndUsers(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("alice")
	b := e.register("bob")
	befriend(t, a, b)
	bws := b.dial()
	bws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	// 头像须为已上传附件
	a.expectErr("PUT", "/v1/me/profile", map[string]any{"avatar_id": "00000000-0000-4000-8000-000000000000"}, 404, "not_found")
	var up struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/attachments", []byte("png-bytes"), &up)

	var p map[string]any
	a.must("PUT", "/v1/me/profile", map[string]any{"name": "爱丽丝", "about": "hi", "avatar_id": up.ID}, &p)
	if p["name"] != "爱丽丝" || p["about"] != "hi" || p["avatar_id"] != up.ID || p["id"] != a.ID {
		t.Fatalf("更新资料异常: %v", p)
	}
	ev := bws.event("profile_updated")
	if ev["profile"].(map[string]any)["name"] != "爱丽丝" {
		t.Fatalf("profile_updated 异常: %v", ev)
	}
	a.expectErr("PUT", "/v1/me/profile", map[string]any{"name": ""}, 400, "bad_request")

	// 路由：/v1/users/search 与 /v1/users/{id} 不冲突
	var found map[string]any
	b.must("GET", "/v1/users/search?email="+strings.ToUpper(a.Email), nil, &found)
	if found["id"] != a.ID || found["email"] != nil {
		t.Fatalf("按邮箱搜索异常（且不应暴露 email）: %v", found)
	}
	b.expectErr("GET", "/v1/users/search?email=nobody@example.com", nil, 404, "not_found")
	var byID map[string]any
	b.must("GET", "/v1/users/"+a.ID, nil, &byID)
	if byID["name"] != "爱丽丝" {
		t.Fatalf("按 id 查询异常: %v", byID)
	}
	b.expectErr("GET", "/v1/users/00000000-0000-4000-8000-000000000000", nil, 404, "not_found")

	// 统一错误体：未知路由、错误方法、坏 JSON
	st, body := e.do("GET", "/v1/nope", a.Token, nil)
	assertErr(t, st, body, 404, "not_found")
	st, body = e.do("PATCH", "/v1/me", a.Token, nil)
	assertErr(t, st, body, 405, "method_not_allowed")
	st, body = e.do("PUT", "/v1/me/profile", a.Token, []byte("{bad json"))
	assertErr(t, st, body, 400, "bad_request")
	var eb map[string]string
	json.Unmarshal(body, &eb)
	if len(eb) != 2 {
		t.Fatalf("错误体应只有 error/message: %v", eb)
	}
}

// TURN 未启用或启动失败（TurnPort=0）时返回空的 urls，其余字段照常（审查 S15）。
func TestTurnDisabledReturnsNoURLs(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.TurnPort = 0 })
	a := e.register("a")
	var tc map[string]any
	a.must("GET", "/v1/calls/turn", nil, &tc)
	urls, isList := tc["urls"].([]any)
	if !isList || len(urls) != 0 {
		t.Fatalf("TURN 未启用时 urls 应为空数组: %v", tc["urls"])
	}
	for _, k := range []string{"username", "credential", "ttl"} {
		if _, found := tc[k]; !found {
			t.Fatalf("缺少字段 %s: %v", k, tc)
		}
	}
}
