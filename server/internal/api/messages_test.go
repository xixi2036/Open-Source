package api_test

import (
	"encoding/base64"
	"testing"
	"time"
)

func olmMsg(to, text string, urgent bool) map[string]any {
	return map[string]any{"messages": []map[string]any{{
		"to": to, "type": 0, "body": base64.StdEncoding.EncodeToString([]byte(text)),
		"client_id": "cid-" + text, "urgent": urgent,
	}}}
}

type sendResp struct {
	Results []struct {
		To       string `json:"to"`
		ClientID string `json:"client_id"`
		ID       string `json:"id"`
		ServerTS int64  `json:"server_ts"`
		Error    string `json:"error"`
	} `json:"results"`
}

func TestFriendRequestFlowAndNonFriendRejected(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	bws := b.dial()
	bws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	// 非好友发消息被拒（逐条错误）
	var sr sendResp
	a.must("POST", "/v1/messages", olmMsg(b.ID, "hello", true), &sr)
	if len(sr.Results) != 1 || sr.Results[0].Error != "not_friends" || sr.Results[0].ClientID != "cid-hello" || sr.Results[0].To != b.ID {
		t.Fatalf("非好友消息应被拒: %+v", sr)
	}

	// 好友申请 → B 收到 friend_request 事件
	var fr map[string]any
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID, "message": "我是 A"}, &fr)
	if fr["status"] != "pending" || fr["message"] != "我是 A" || fr["from"].(map[string]any)["id"] != a.ID || fr["to"].(map[string]any)["id"] != b.ID {
		t.Fatalf("FriendRequest 异常: %v", fr)
	}
	ev := bws.event("friend_request")
	if ev["request"].(map[string]any)["id"] != fr["id"] {
		t.Fatalf("friend_request 事件异常: %v", ev)
	}
	bws.send(map[string]any{"t": "ack", "ids": []any{ev["id"]}})

	var lists struct {
		Incoming []map[string]any `json:"incoming"`
		Outgoing []map[string]any `json:"outgoing"`
	}
	b.must("GET", "/v1/friends/requests", nil, &lists)
	if len(lists.Incoming) != 1 || len(lists.Outgoing) != 0 {
		t.Fatalf("B 的申请列表异常: %+v", lists)
	}
	a.must("GET", "/v1/friends/requests", nil, &lists)
	if len(lists.Incoming) != 0 || len(lists.Outgoing) != 1 {
		t.Fatalf("A 的申请列表异常: %+v", lists)
	}

	// 只有接收方能处理申请
	a.expectErr("POST", "/v1/friends/requests/"+fr["id"].(string)+"/accept", nil, 404, "not_found")

	aws := a.dial()
	aws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	b.must("POST", "/v1/friends/requests/"+fr["id"].(string)+"/accept", nil, nil)
	if acc := aws.event("friend_accepted"); acc["user_id"] != b.ID {
		t.Fatalf("friend_accepted 异常: %v", acc)
	}
	b.expectErr("POST", "/v1/friends/requests/"+fr["id"].(string)+"/accept", nil, 409, "conflict")

	var friends []map[string]any
	a.must("GET", "/v1/friends", nil, &friends)
	if len(friends) != 1 || friends[0]["id"] != b.ID {
		t.Fatalf("好友列表异常: %v", friends)
	}
	a.expectErr("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, 409, "conflict")

	// 现在可以发消息
	a.must("POST", "/v1/messages", olmMsg(b.ID, "hello", true), &sr)
	if sr.Results[0].Error != "" || sr.Results[0].ID == "" || sr.Results[0].ServerTS == 0 {
		t.Fatalf("好友消息应成功: %+v", sr)
	}

	// 删除好友 → 对方收到 friend_removed，之后不能发消息
	b.must("DELETE", "/v1/friends/"+a.ID, nil, nil)
	if rm := aws.event("friend_removed"); rm["user_id"] != b.ID {
		t.Fatalf("friend_removed 异常: %v", rm)
	}
	a.must("POST", "/v1/messages", olmMsg(b.ID, "after", true), &sr)
	if sr.Results[0].Error != "not_friends" {
		t.Fatalf("删除好友后应被拒: %+v", sr)
	}
}

func TestMutualRequestAutoAccept(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, nil)
	var fr map[string]any
	b.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": a.ID}, &fr)
	if fr["status"] != "accepted" {
		t.Fatalf("互发申请应直接成为好友: %v", fr)
	}
	var friends []map[string]any
	a.must("GET", "/v1/friends", nil, &friends)
	if len(friends) != 1 {
		t.Fatalf("应为好友: %v", friends)
	}
	var inbox inboxResp
	a.must("GET", "/v1/messages", nil, &inbox)
	found := false
	for _, ev := range inbox.Events {
		if ev["type"] == "friend_accepted" && ev["user_id"] == b.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("A 应收到 friend_accepted: %+v", inbox.Events)
	}
}

func TestReject(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	var fr map[string]any
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, &fr)
	b.must("POST", "/v1/friends/requests/"+fr["id"].(string)+"/reject", nil, nil)
	var lists struct {
		Incoming []any `json:"incoming"`
	}
	b.must("GET", "/v1/friends/requests", nil, &lists)
	if len(lists.Incoming) != 0 {
		t.Fatal("拒绝后不应再有待处理申请")
	}
}

func TestBlocks(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	b.must("POST", "/v1/blocks/"+a.ID, nil, nil)
	var blocked []string
	b.must("GET", "/v1/blocks", nil, &blocked)
	if len(blocked) != 1 || blocked[0] != a.ID {
		t.Fatalf("拉黑列表异常: %v", blocked)
	}
	var sr sendResp
	a.must("POST", "/v1/messages", olmMsg(b.ID, "x", true), &sr)
	if sr.Results[0].Error != "blocked" {
		t.Fatalf("被拉黑应返回 blocked: %+v", sr)
	}
	b.must("POST", "/v1/messages", olmMsg(a.ID, "x", true), &sr)
	if sr.Results[0].Error != "blocked" {
		t.Fatalf("拉黑方发消息也应返回 blocked: %+v", sr)
	}
	c := e.register("c")
	b.must("POST", "/v1/blocks/"+c.ID, nil, nil)
	c.expectErr("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, 403, "blocked")
	b.must("DELETE", "/v1/blocks/"+a.ID, nil, nil)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "x", true), &sr)
	if sr.Results[0].Error != "" {
		t.Fatalf("取消拉黑后应可发送: %+v", sr)
	}
}

func TestOlmDeliveryWSAndAck(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)

	// B 离线：消息入队，urgent 触发推送，非 urgent 不触发
	a.must("POST", "/v1/messages", olmMsg(b.ID, "m1", true), nil)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "m2", false), nil)
	var sr sendResp
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{
		{"to": b.ID, "type": 1, "body": "bTM=", "client_id": "c3"},
		{"to": b.ID, "type": 7, "body": "eA==", "client_id": "bad-type"},
		{"to": a.ID, "type": 1, "body": "eA==", "client_id": "self"},
	}}, &sr)
	if sr.Results[0].Error != "" || sr.Results[1].Error != "bad_request" || sr.Results[2].Error != "bad_request" {
		t.Fatalf("批量逐条结果异常: %+v", sr)
	}
	if e.pusher.count(b.ID) == 0 {
		t.Fatal("离线 urgent 消息应触发推送")
	}

	// B 上线：先收到积压（按顺序），再 queue_empty
	bws := b.dial()
	var got []envelope
	for {
		f := bws.read()
		if f.T == "queue_empty" {
			break
		}
		if f.T == "envelope" {
			got = append(got, f.envelope())
		}
	}
	if len(got) != 3 {
		t.Fatalf("应收到 3 条积压信封，得到 %d", len(got))
	}
	first := got[0]
	if first.Kind != "olm" || first.From != a.ID || first.Type != 0 || first.GroupID != nil || first.SessionID != nil ||
		first.ServerTS == 0 || first.ID == "" {
		t.Fatalf("信封字段异常: %+v", first)
	}
	if body, _ := base64.StdEncoding.DecodeString(first.Body); string(body) != "m1" {
		t.Fatalf("信封顺序/内容异常: %+v", got)
	}
	if b2, _ := base64.StdEncoding.DecodeString(got[1].Body); string(b2) != "m2" || got[2].Type != 1 {
		t.Fatalf("信封顺序异常: %+v", got)
	}

	// 在线实时推送
	pushesBefore := e.pusher.count(b.ID)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "live", true), &sr)
	live := bws.until("实时信封", func(f frame) bool { return f.T == "envelope" }).envelope()
	if live.ID != sr.Results[0].ID {
		t.Fatalf("实时信封 id 应与发送结果一致: %v vs %v", live.ID, sr.Results[0].ID)
	}
	if e.pusher.count(b.ID) != pushesBefore {
		t.Fatal("在线时不应触发推送")
	}

	// ping → pong
	bws.send(map[string]any{"t": "ping"})
	bws.until("pong", func(f frame) bool { return f.T == "pong" })

	// 经 WS ack 前两条；剩余两条经 HTTP 拉取
	bws.send(map[string]any{"t": "ack", "ids": []string{got[0].ID, got[1].ID}})
	deadline := time.Now().Add(2 * time.Second)
	var inbox inboxResp
	for time.Now().Before(deadline) {
		b.must("GET", "/v1/messages?limit=100", nil, &inbox)
		if len(inbox.Messages) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(inbox.Messages) != 2 || inbox.Messages[0].ID != got[2].ID || inbox.Messages[1].ID != live.ID {
		t.Fatalf("WS ack 后应剩 2 条: %+v", inbox.Messages)
	}
	ids := []string{}
	for _, m := range inbox.Messages {
		ids = append(ids, m.ID)
	}
	for _, ev := range inbox.Events {
		ids = append(ids, ev["id"].(string))
	}
	b.must("POST", "/v1/messages/ack", map[string]any{"ids": ids}, nil)
	b.must("GET", "/v1/messages", nil, &inbox)
	if len(inbox.Messages) != 0 || len(inbox.Events) != 0 {
		t.Fatalf("HTTP ack 后应为空: %+v", inbox)
	}
}

func TestWSRedeliveryAndReplace(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "keep", true), nil)

	// 未 ack 的信封在重连后再次投递
	w1 := b.dial()
	e1 := w1.until("信封", func(f frame) bool { return f.T == "envelope" }).envelope()
	w1.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	// 新连接挤掉旧连接
	w2 := b.dial()
	e2 := w2.until("信封", func(f frame) bool { return f.T == "envelope" }).envelope()
	if e1.ID != e2.ID {
		t.Fatal("未确认的信封应重新投递")
	}
	w2.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	if _, err := w1.tryRead(2 * time.Second); err == nil {
		t.Fatal("旧连接应被关闭")
	}
	// 新连接仍可接收实时消息
	a.must("POST", "/v1/messages", olmMsg(b.ID, "after-replace", true), nil)
	w2.until("实时信封", func(f frame) bool { return f.T == "envelope" })
}

func TestWSRequiresAuth(t *testing.T) {
	e := newEnv(t, true)
	st, body := e.do("GET", "/v1/ws?token=bogus", "", nil)
	assertErr(t, st, body, 401, "unauthorized")
}
