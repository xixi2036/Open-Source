package api_test

import (
	"context"
	"strings"
	"testing"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// 信封字段校验：to 必须是 UUID，client_id / session_id 限长且限字符集。
func TestEnvelopeFieldValidation(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)

	var sr sendResp
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{
		{"to": "not-a-uuid", "type": 1, "body": "eA==", "client_id": "c1"},
		{"to": strings.ToUpper(b.ID) + "x", "type": 1, "body": "eA==", "client_id": "c2"},
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": strings.Repeat("a", 129)},
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": "含中文"},
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": `x"<>`},
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": "Zm9v+/=_-" + strings.Repeat("a", 119)},
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": "a4d0a1a2-7d0e-4c1b-9d1a-0c2b3e4f5a6b"},
	}}, &sr)
	for i := 0; i < 5; i++ {
		if sr.Results[i].Error != "bad_request" {
			t.Fatalf("第 %d 条应为 bad_request: %+v", i, sr.Results[i])
		}
	}
	for i := 5; i < 7; i++ {
		if sr.Results[i].Error != "" || sr.Results[i].ID == "" {
			t.Fatalf("第 %d 条应成功: %+v", i, sr.Results[i])
		}
	}

	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	path := "/v1/groups/" + g.ID + "/messages"
	a.expectErr("POST", path, map[string]any{"session_id": strings.Repeat("s", 129), "body": "eA==", "client_id": "c"}, 400, "bad_request")
	a.expectErr("POST", path, map[string]any{"session_id": "sess<script>", "body": "eA==", "client_id": "c"}, 400, "bad_request")
	a.expectErr("POST", path, map[string]any{"session_id": "sess", "body": "eA==", "client_id": "c c"}, 400, "bad_request")
	a.must("POST", path, map[string]any{"session_id": "AbC+/0123456789_-", "body": "eA==", "client_id": "c"}, nil)
}

// escLT / escAmp 是 encoding/json 默认 HTML 转义产生的序列（反斜杠 + u003c / u0026）。
var escLT, escAmp = "\\" + "u003c", "\\" + "u0026"

// 队列与 WS 帧不做 HTML 转义：<>& 原样输出，不会膨胀成 <。
func TestFramesNotHTMLEscaped(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	bws := b.dial()
	bws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	body := "<b>&amp;</b>"
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{
		{"to": b.ID, "type": 1, "body": body, "client_id": "c1"},
	}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3e9)
	defer cancel()
	for {
		_, raw, err := bws.conn.Read(ctx)
		if err != nil {
			t.Fatalf("读取帧失败: %v", err)
		}
		s := string(raw)
		if !strings.Contains(s, `"t":"envelope"`) {
			continue
		}
		if !strings.Contains(s, body) || strings.Contains(s, escLT) || strings.Contains(s, escAmp) {
			t.Fatalf("帧不应做 HTML 转义: %s", s)
		}
		break
	}
	_, raw := b.call("GET", "/v1/messages", nil, nil)
	if !strings.Contains(string(raw), body) || strings.Contains(string(raw), escLT) {
		t.Fatalf("HTTP 拉取也不应做 HTML 转义: %s", raw)
	}
}

// 1MB 的 body 可以投递；序列化后超过帧上限的直接 too_large（不会入队卡住接收方）。
func TestEnvelopeFrameLimit(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)

	var sr sendResp
	big := strings.Repeat("A", 1<<20)
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{
		{"to": b.ID, "type": 1, "body": big, "client_id": "big"},
		{"to": b.ID, "type": 1, "body": big + "A", "client_id": "bigger"},
	}}, &sr)
	if sr.Results[0].Error != "" || sr.Results[1].Error != "too_large" {
		t.Fatalf("1MB 应成功、超过应 too_large: %+v", sr)
	}
}

// 兜底：队列里已存在超过帧上限的项（例如旧数据）时，推送时删除它，后续数据照常投递，不会卡住队列。
func TestOversizedQueueItemDropped(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)

	huge := `{"id":"huge","kind":"olm","body":"` + strings.Repeat("A", ws.MaxFrameBytes) + `"}`
	if _, err := e.st.Enqueue(context.Background(), []store.QueueInsert{{UserID: b.ID, ID: "huge", Kind: store.KindEnvelope, Payload: []byte(huge)}}, store.QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	var sr sendResp
	a.must("POST", "/v1/messages", olmMsg(b.ID, "after-huge", true), &sr)

	bws := b.dial()
	var ids []string
	for {
		f := bws.read()
		if f.T == "queue_empty" {
			break
		}
		if f.T == "envelope" {
			ids = append(ids, f.envelope().ID)
		}
	}
	if len(ids) != 1 || ids[0] != sr.Results[0].ID {
		t.Fatalf("应跳过超限项、只收到后续信封: %v", ids)
	}
	var inbox inboxResp
	b.must("GET", "/v1/messages", nil, &inbox)
	for _, m := range inbox.Messages {
		if m.ID == "huge" {
			t.Fatal("超限项应已从队列删除")
		}
	}
}
