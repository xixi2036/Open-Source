package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hush/server/internal/api"
	"hush/server/internal/store"
)

// hasKey 判断 JSON 对象里是否出现某个键。
func hasKey(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("不是 JSON 对象: %s", raw)
	}
	_, found := m[key]
	return found
}

// S-A：单条动态接口，可见性与点赞/评论过滤规则与 feed 一致，不可见与不存在同为 404。
func TestGetSingleMoment(t *testing.T) {
	e := newEnv(t, true)
	a, b, c, d := e.register("a"), e.register("b"), e.register("c"), e.register("d")
	befriend(t, a, b)
	befriend(t, a, c) // c 与 b 不是好友
	befriend(t, a, d)

	m := post(a, "hello", "exclude", []string{d.ID})
	c.must("POST", "/v1/moments/"+m.ID+"/like", nil, nil)
	c.must("POST", "/v1/moments/"+m.ID+"/comments", map[string]any{"text": "from c"}, nil)
	b.must("POST", "/v1/moments/"+m.ID+"/like", nil, nil)

	// 作者：可见，带 visibility，看到全部点赞
	var got momentResp
	a.must("GET", "/v1/moments/"+m.ID, nil, &got)
	if got.ID != m.ID || got.Visibility == nil || len(got.Likes) != 2 || len(got.Comments) != 1 {
		t.Fatalf("作者视角不对: %+v", got)
	}
	// 好友 b：可见，不带 visibility，看不到非好友 c 的点赞与评论；与 feed 里的同一条一致
	b.must("GET", "/v1/moments/"+m.ID, nil, &got)
	if got.Visibility != nil || len(got.Likes) != 1 || got.Likes[0].ID != b.ID || len(got.Comments) != 0 {
		t.Fatalf("好友视角过滤不对: %+v", got)
	}
	feed := feedIDs(b, "/v1/moments/feed")
	fm, found := feed["hello"]
	if !found {
		t.Fatal("feed 里应有该动态")
	}
	gj, _ := json.Marshal(got)
	fj, _ := json.Marshal(fm)
	if string(gj) != string(fj) {
		t.Fatalf("单条与 feed 结果不一致:\n%s\n%s", gj, fj)
	}
	// 被排除的好友、陌生人、不存在的 id：一律 404 not_found
	d.expectErr("GET", "/v1/moments/"+m.ID, nil, 404, "not_found")
	stranger := e.register("s")
	stranger.expectErr("GET", "/v1/moments/"+m.ID, nil, 404, "not_found")
	a.expectErr("GET", "/v1/moments/00000000-0000-0000-0000-000000000000", nil, 404, "not_found")
	// 删除后 404
	a.must("DELETE", "/v1/moments/"+m.ID, nil, nil)
	b.expectErr("GET", "/v1/moments/"+m.ID, nil, 404, "not_found")
	// 固定路由不受影响
	a.must("GET", "/v1/moments/feed", nil, nil)
	a.must("GET", "/v1/moments/notifications", nil, nil)
}

// S-B：email 只对本人与互为好友的人返回；好友申请双方可见彼此邮箱；其他地方一律省略。
func TestProfileEmailVisibility(t *testing.T) {
	e := newEnv(t, true)
	a, b, c := e.register("a"), e.register("b"), e.register("c")
	befriend(t, a, b)

	var p struct {
		ID    string  `json:"id"`
		Email *string `json:"email"`
	}
	a.must("GET", "/v1/me", nil, &p)
	if p.Email == nil || *p.Email != a.Email {
		t.Fatalf("/v1/me 应带本人邮箱: %+v", p)
	}
	a.must("GET", "/v1/users/"+a.ID, nil, &p)
	if p.Email == nil || *p.Email != a.Email {
		t.Fatal("查看自己应带邮箱")
	}
	a.must("GET", "/v1/users/"+b.ID, nil, &p)
	if p.Email == nil || *p.Email != b.Email {
		t.Fatal("查看好友应带邮箱")
	}
	_, raw := a.call("GET", "/v1/users/"+c.ID, nil, nil)
	if hasKey(t, raw, "email") {
		t.Fatalf("非好友不应返回 email: %s", raw)
	}
	var friends []struct {
		ID    string  `json:"id"`
		Email *string `json:"email"`
	}
	a.must("GET", "/v1/friends", nil, &friends)
	if len(friends) != 1 || friends[0].Email == nil || *friends[0].Email != b.Email {
		t.Fatalf("/v1/friends 应带邮箱: %+v", friends)
	}
	// 搜索接口维持原样：不带 email
	_, raw = a.call("GET", "/v1/users/search?email="+c.Email, nil, nil)
	if hasKey(t, raw, "email") {
		t.Fatalf("搜索结果不应带 email: %s", raw)
	}

	// 好友申请：双方可见彼此邮箱（列表与事件）
	wsA := a.dial()
	wsA.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	c.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": a.ID, "message": "hi"}, nil)
	type reqView struct {
		From struct {
			Email *string `json:"email"`
		} `json:"from"`
		To struct {
			Email *string `json:"email"`
		} `json:"to"`
	}
	ev := wsA.event("friend_request")
	rawReq, _ := json.Marshal(ev["request"])
	var rv reqView
	json.Unmarshal(rawReq, &rv)
	if rv.From.Email == nil || *rv.From.Email != c.Email || rv.To.Email == nil || *rv.To.Email != a.Email {
		t.Fatalf("friend_request 事件应带双方邮箱: %s", rawReq)
	}
	var lists struct {
		Incoming []reqView `json:"incoming"`
		Outgoing []reqView `json:"outgoing"`
	}
	a.must("GET", "/v1/friends/requests", nil, &lists)
	if len(lists.Incoming) != 1 || lists.Incoming[0].From.Email == nil || *lists.Incoming[0].From.Email != c.Email {
		t.Fatalf("收到的申请应带申请人邮箱: %+v", lists)
	}
	c.must("GET", "/v1/friends/requests", nil, &lists)
	if len(lists.Outgoing) != 1 || lists.Outgoing[0].To.Email == nil || *lists.Outgoing[0].To.Email != a.Email {
		t.Fatalf("发出的申请应带对方邮箱: %+v", lists)
	}

	// 其他地方一律不带：改资料的响应与 profile_updated 事件、朋友圈作者、群
	wsB := b.dial()
	wsB.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	_, raw = a.call("PUT", "/v1/me/profile", map[string]any{"name": "A2"}, nil)
	if hasKey(t, raw, "email") {
		t.Fatalf("PUT /v1/me/profile 响应不带 email: %s", raw)
	}
	pu := wsB.event("profile_updated")
	if prof, _ := pu["profile"].(map[string]any); prof == nil || prof["email"] != nil {
		t.Fatalf("profile_updated 不应带 email: %v", pu)
	}
	m := post(a, "moment", "", nil)
	_, raw = b.call("GET", "/v1/moments/"+m.ID, nil, nil)
	if strings.Contains(string(raw), `"email"`) {
		t.Fatalf("朋友圈资料不应带 email: %s", raw)
	}
	_, raw = a.call("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, nil)
	if strings.Contains(string(raw), `"email"`) {
		t.Fatalf("群信息不应带 email: %s", raw)
	}
}

// S-C：令牌超过 TokenTTL 未使用即失效，HTTP 接口与 WS 握手都返回 401。
func TestTokenExpiry(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.TokenTTL = time.Second })
	a := e.register("a")
	a.must("GET", "/v1/me", nil, nil)
	time.Sleep(1200 * time.Millisecond)
	a.expectErr("GET", "/v1/me", nil, 401, "unauthorized")

	b := e.register("b")
	time.Sleep(1200 * time.Millisecond)
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + b.Token}}})
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("过期令牌的 WS 握手应 401: err=%v resp=%v", err, resp)
	}
	// 重新登录拿到新令牌后恢复
	b2 := e.login(b.Email)
	b2.must("GET", "/v1/me", nil, nil)
}

// TokenTTL=0：永不过期。
func TestTokenNoExpiry(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	time.Sleep(50 * time.Millisecond)
	a.must("GET", "/v1/me", nil, nil)
}

// S-D（端到端）：业务提交之后、在线推送之前进程崩溃——事件已随业务写入一起提交，
// 在线连接没有被唤醒，但重连后作为积压收到。
func TestOutboxEventSurvivesCrashBeforePush(t *testing.T) {
	e := newEnv(t, true)
	a, b := e.register("a"), e.register("b")
	wsB := b.dial()
	wsB.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	e.st.SetCommitHook(func([]store.QueueInsert) {}) // 模拟：提交后、通知在线连接前崩溃
	var fr struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID, "message": "hi"}, &fr)
	wsB.expectSilence(300 * time.Millisecond)

	items, err := e.st.QueueAfter(context.Background(), b.ID, 0, 100, 0)
	if err != nil || len(items) != 1 || !strings.Contains(string(items[0].Payload), fr.ID) {
		t.Fatalf("事件应已与好友申请一起提交: %v %v", items, err)
	}
	wsB2 := b.dial()
	f := wsB2.read()
	if f.T != "event" || f.event()["type"] != "friend_request" {
		t.Fatalf("重连后应先收到积压的 friend_request: %s %s", f.T, f.D)
	}
	wsB2.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
}

// S-F：Olm 批量发送中各接收方的关系校验（一次集合查询）逐条给出正确结果，错误优先级不变。
func TestOlmBatchRelationCheck(t *testing.T) {
	e := newEnv(t, true)
	a, friend, mate, stranger, blocker := e.register("a"), e.register("f"), e.register("m"), e.register("s"), e.register("b")
	befriend(t, a, friend)
	befriend(t, a, blocker)
	befriend(t, a, mate)
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{mate.ID}}, nil)
	a.must("DELETE", "/v1/friends/"+mate.ID, nil, nil) // 只剩共同群关系
	blocker.must("POST", "/v1/blocks/"+a.ID, nil, nil)
	missing := "00000000-0000-0000-0000-000000000000"
	msg := func(to string, typ any, body string) map[string]any {
		return map[string]any{"to": to, "type": typ, "body": body}
	}
	big := strings.Repeat("x", 1<<20+1)
	batch := []map[string]any{
		msg(friend.ID, 0, "ok1"),  // 0 好友
		msg(mate.ID, 1, "ok2"),    // 1 共同群
		msg(stranger.ID, 0, "x"),  // 2 陌生人
		msg(blocker.ID, 0, "x"),   // 3 被拉黑
		msg(missing, 0, "x"),      // 4 不存在
		msg(a.ID, 0, "x"),         // 5 发给自己
		msg("not-a-uuid", 0, "x"), // 6 格式错误
		msg(stranger.ID, 0, big),  // 7 过大优先于关系错误
		msg(friend.ID, 0, "ok3"),  // 8 重复接收方
		msg(stranger.ID, 7, "x"),  // 9 type 非法优先于关系错误
	}
	var sr sendResp
	a.must("POST", "/v1/messages", map[string]any{"messages": batch}, &sr)
	want := []string{"", "", "not_friends", "blocked", "not_found", "bad_request", "bad_request", "too_large", "", "bad_request"}
	if len(sr.Results) != len(want) {
		t.Fatalf("结果条数不对: %+v", sr)
	}
	for i, w := range want {
		r := sr.Results[i]
		if r.Error != w || (w == "") != (r.ID != "") {
			t.Fatalf("第 %d 条: 期望 %q，得到 %+v", i, w, r)
		}
	}
}
