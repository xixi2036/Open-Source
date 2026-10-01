package api_test

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

	"hush/server/internal/api"
	"hush/server/internal/netx"
	"hush/server/internal/store"
)

// 本文件是 2026-09-30 只读审查报告 A1–A11 与 B 类问题的回归测试。

func (e *env) rawDB() *sql.DB {
	e.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(e.dataDir, "hush.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	return db
}

func scalar(t *testing.T, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// makeGroup 建一个 owner + n 个成员的群，返回群与成员。
func makeGroup(t *testing.T, e *env, owner *client, n int) (groupResp, []*client) {
	t.Helper()
	var members []*client
	var ids []string
	for i := 0; i < n; i++ {
		m := e.register(fmt.Sprint("m", i, "x"))
		befriend(t, owner, m)
		members = append(members, m)
		ids = append(ids, m.ID)
	}
	var g groupResp
	owner.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": ids}, &g)
	return g, members
}

// A1：群消息正文只存一份——20 个接收方、60KB 的消息，库里只有一份正文；全部 ack 之后正文被删除。
func TestA1GroupMessageStoredOnce(t *testing.T) {
	e := newEnv(t, true)
	owner := e.register("o")
	g, members := makeGroup(t, e, owner, 20)
	for _, m := range members {
		var in inboxResp
		m.must("GET", "/v1/messages?limit=500", nil, &in) // 清掉建群事件
		var ids []string
		for _, ev := range in.Events {
			ids = append(ids, ev["id"].(string))
		}
		m.must("POST", "/v1/messages/ack", map[string]any{"ids": ids}, nil)
	}
	body := strings.Repeat("A", 60<<10)
	var sent struct {
		ID string `json:"id"`
	}
	owner.must("POST", "/v1/groups/"+g.ID+"/messages", map[string]any{"session_id": "s1", "body": body}, &sent)
	db := e.rawDB()
	if n := scalar(t, db, "SELECT COUNT(*) FROM queue WHERE id = ?", sent.ID); n != 20 {
		t.Fatalf("应有 20 条队列引用，得到 %d", n)
	}
	if n := scalar(t, db, "SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM queue WHERE id = ?", sent.ID); n != 0 {
		t.Fatalf("队列行不应再内联正文，得到 %d 字节", n)
	}
	if n := scalar(t, db, "SELECT COUNT(*) FROM envelopes WHERE id = ?", sent.ID); n != 1 {
		t.Fatalf("正文应只存一份，得到 %d", n)
	}
	// 每个成员收到的内容完整、id 相同；配额仍按每人的逻辑大小计
	for i, m := range members {
		var in inboxResp
		m.must("GET", "/v1/messages?limit=500", nil, &in)
		if len(in.Messages) != 1 || in.Messages[0].ID != sent.ID || in.Messages[0].Body != body {
			t.Fatalf("成员 %d 收到的群消息不对: %d 条", i, len(in.Messages))
		}
		if n, b, _ := e.st.EnvelopeUsage(context.Background(), m.ID); n != 1 || b < 60<<10 {
			t.Fatalf("用量应按逻辑大小计: %d / %d", n, b)
		}
		if i < len(members)-1 {
			m.must("POST", "/v1/messages/ack", map[string]any{"ids": []string{sent.ID}}, nil)
			if n := scalar(t, db, "SELECT COUNT(*) FROM envelopes WHERE id = ?", sent.ID); n != 1 {
				t.Fatal("还有人没 ack 时正文不能删")
			}
		}
	}
	members[len(members)-1].must("POST", "/v1/messages/ack", map[string]any{"ids": []string{sent.ID}}, nil)
	if n := scalar(t, db, "SELECT COUNT(*) FROM envelopes WHERE id = ?", sent.ID); n != 0 {
		t.Fatal("最后一个引用 ack 之后正文应被删除")
	}
}

// A1：群消息 body 上限 256KB；发送侧限速（条数与字节）。
func TestA1SendLimits(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) {
		c.SendOlmPerMinute, c.SendGroupPerMinute, c.SendBytesPerMinute = 5, 2, 1<<20
	})
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	a.expectErr("POST", "/v1/groups/"+g.ID+"/messages", map[string]any{"session_id": "s", "body": strings.Repeat("A", 256<<10+1)}, 413, "too_large")
	a.must("POST", "/v1/groups/"+g.ID+"/messages", map[string]any{"session_id": "s", "body": strings.Repeat("A", 256<<10)}, nil)
	a.must("POST", "/v1/groups/"+g.ID+"/messages", megolm("2"), nil)
	a.expectErr("POST", "/v1/groups/"+g.ID+"/messages", megolm("3"), 429, "rate_limited")
	// 单聊按条目计：5 条之后 429（整个请求被拒，不产生部分投递）
	for i := 0; i < 5; i++ {
		a.must("POST", "/v1/messages", olmMsg(b.ID, fmt.Sprint("m", i), true), nil)
	}
	a.expectErr("POST", "/v1/messages", olmMsg(b.ID, "m5", true), 429, "rate_limited")
	// 别的用户不受影响
	b.must("POST", "/v1/messages", olmMsg(a.ID, "hi", true), nil)
	// 字节限速：c 一分钟 1MB
	c := e.register("c")
	befriend(t, c, b)
	big := map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 1, "body": strings.Repeat("A", 600<<10)}}}
	c.must("POST", "/v1/messages", big, nil)
	c.expectErr("POST", "/v1/messages", big, 429, "rate_limited")
}

// A2：一个好友灌满的只是他自己的子配额，别人照常能发；灌满者得到 too_large。
func TestA2SenderSubQuota(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.SenderQueueMaxItems = 10 })
	a := e.register("a")
	b := e.register("b")
	c := e.register("c")
	befriend(t, a, b)
	befriend(t, c, b)
	msgs := []map[string]any{}
	for i := 0; i < 15; i++ {
		msgs = append(msgs, map[string]any{"to": b.ID, "type": 1, "body": "eA=="})
	}
	var sr sendResp
	a.must("POST", "/v1/messages", map[string]any{"messages": msgs}, &sr)
	okN, full := 0, 0
	for _, r := range sr.Results {
		if r.Error == "" {
			okN++
		} else if r.Error == "too_large" {
			full++
		}
	}
	if okN != 10 || full != 5 {
		t.Fatalf("发送方子配额 10：应成功 10、拒收 5，得到 %d/%d", okN, full)
	}
	c.must("POST", "/v1/messages", olmMsg(b.ID, "from-c", true), &sr)
	if sr.Results[0].Error != "" {
		t.Fatalf("a 灌满自己的子配额后，c 的消息不应被拒: %+v", sr.Results[0])
	}
	// 只是 a 的子配额满了，b 的队列没满：不产生 messages_rejected
	var in inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &in)
	for _, ev := range in.Events {
		if ev["type"] == "messages_rejected" {
			t.Fatal("子配额被拒不应给接收方发 messages_rejected")
		}
	}
	// b ack 掉 a 的消息后 a 可以继续发
	var ids []string
	for _, m := range in.Messages {
		ids = append(ids, m.ID)
	}
	b.must("POST", "/v1/messages/ack", map[string]any{"ids": ids}, nil)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "again", true), &sr)
	if sr.Results[0].Error != "" {
		t.Fatalf("ack 后子配额应释放: %+v", sr.Results[0])
	}
}

// A3：事件不占用信封配额——评论再多也挡不住 Olm 消息；同一（动态, 评论者）的评论通知事件有上限。
func TestA3EventsDoNotConsumeEnvelopeQuota(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.QueueMaxItems = 20 })
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	m := post(b, "hello", "all", nil)
	for i := 0; i < 40; i++ {
		a.must("POST", "/v1/moments/"+m.ID+"/comments", map[string]any{"text": fmt.Sprint("c", i)}, nil)
	}
	var sr sendResp
	a.must("POST", "/v1/messages", olmMsg(b.ID, "still-works", true), &sr)
	if sr.Results[0].Error != "" {
		t.Fatalf("事件不应占用信封配额: %+v", sr.Results[0])
	}
	var in inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &in)
	notes := 0
	for _, ev := range in.Events {
		if ev["type"] == "moment_notification" {
			notes++
		}
	}
	if notes > 3 {
		t.Fatalf("同一人对同一条动态的评论通知事件应有上限（3），队列里有 %d 条", notes)
	}
	// 通知本身一条不少，仍可通过接口拉到
	var list []map[string]any
	b.must("GET", "/v1/moments/notifications?limit=200", nil, &list)
	if len(list) != 40 {
		t.Fatalf("通知列表应有 40 条，得到 %d", len(list))
	}
}

// A3：改群资料与群消息交替，group_updated 不能无限堆积（最多两条），且事件内容不变（仍带完整 Group）。
func TestA3GroupUpdatedBounded(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g0", "member_ids": []string{b.ID}}, &g)
	for i := 0; i < 30; i++ {
		a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"name": fmt.Sprint("g", i+1)}, nil)
		a.must("POST", "/v1/groups/"+g.ID+"/messages", megolm(fmt.Sprint("m", i)), nil)
	}
	var in inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &in)
	var updates []map[string]any
	for _, ev := range in.Events {
		if ev["type"] == "group_updated" {
			updates = append(updates, ev)
		}
	}
	if len(updates) == 0 || len(updates) > 2 {
		t.Fatalf("交替 30 次后 group_updated 应不超过 2 条，得到 %d", len(updates))
	}
	for _, ev := range updates {
		grp, _ := ev["group"].(map[string]any)
		if grp == nil || grp["name"] != "g30" || len(grp["members"].([]any)) != 2 || ev["id"] == "" {
			t.Fatalf("group_updated 应带完整的最新 Group: %v", ev)
		}
	}
	if len(in.Messages) != 30 {
		t.Fatalf("群消息一条不少: %d", len(in.Messages))
	}
	// WS 上的顺序：第一条 group_updated 先于全部群消息
	w := b.dial()
	f := w.until("第一帧事件或信封", func(f frame) bool {
		return f.T == "envelope" || (f.T == "event" && f.event()["type"] == "group_updated")
	})
	if f.T != "event" {
		t.Fatal("group_updated 应先于群消息到达")
	}
}

// A4：拉黑之后，对方看不到我的朋友圈、不能点赞评论、不能拉我进群，我也收不到他的通知。
func TestA4BlockSemantics(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a") // 受害者
	b := e.register("b") // 被拉黑的人
	c := e.register("c")
	befriend(t, a, b)
	befriend(t, a, c)
	befriend(t, b, c)
	m := post(a, "my moment", "all", nil)
	post(b, "b moment", "all", nil)
	c.must("POST", "/v1/moments/"+m.ID+"/comments", map[string]any{"text": "c says"}, nil)
	a.must("POST", "/v1/blocks/"+b.ID, nil, nil)
	var in inboxResp
	a.must("GET", "/v1/messages?limit=500", nil, &in)
	before := len(in.Events)

	// 被拉黑的人：看不到、不能互动
	if _, ok := feedIDs(b, "/v1/moments/feed")["my moment"]; ok {
		t.Fatal("被拉黑后不应在动态流里看到对方的动态")
	}
	if len(feedIDs(b, "/v1/moments/user/"+a.ID)) != 0 {
		t.Fatal("被拉黑后不应看到对方的个人动态")
	}
	b.expectErr("GET", "/v1/moments/"+m.ID, nil, 404, "not_found")
	b.expectErr("POST", "/v1/moments/"+m.ID+"/like", nil, 404, "not_found")
	b.expectErr("POST", "/v1/moments/"+m.ID+"/comments", map[string]any{"text": "x"}, 404, "not_found")
	// 拉黑方也看不到对方（任一方拉黑即互不可见，与消息的判定一致）
	if _, ok := feedIDs(a, "/v1/moments/feed")["b moment"]; ok {
		t.Fatal("拉黑之后不应再看到对方的动态")
	}
	// 拉群：建群与加人都被拒
	b.expectErr("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{a.ID}}, 403, "blocked")
	var g groupResp
	b.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{c.ID}}, &g)
	b.expectErr("POST", "/v1/groups/"+g.ID+"/members", map[string]any{"user_ids": []string{a.ID}}, 403, "blocked")
	// 回复通知：b 在 c 的动态下回复 a，a 不应收到
	cm := post(c, "c moment", "all", nil)
	b.must("POST", "/v1/moments/"+cm.ID+"/comments", map[string]any{"text": "@a", "reply_to_user_id": a.ID}, nil)
	a.must("GET", "/v1/messages?limit=500", nil, &in)
	if len(in.Events) != before {
		t.Fatalf("被拉黑的人不应再给我产生任何事件: %v", in.Events[before:])
	}
	// 别人动态下，被拉黑者的评论 / 点赞对我不可见
	var got momentResp
	a.must("GET", "/v1/moments/"+cm.ID, nil, &got)
	if len(got.Comments) != 0 {
		t.Fatalf("不应看到被拉黑者的评论: %+v", got.Comments)
	}
	// 取消拉黑后恢复
	a.must("DELETE", "/v1/blocks/"+b.ID, nil, nil)
	if _, ok := feedIDs(b, "/v1/moments/feed")["my moment"]; !ok {
		t.Fatal("取消拉黑后应恢复可见")
	}
}

// A5：OTK 总量有上限；朋友圈发布有每日上限、可见名单只保留好友；邮箱搜索限速。
func TestA5Caps(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.MomentsPerDay, c.SearchPerMinute = 3, 5 })
	a := e.register("a")
	b := e.register("b")
	stranger := e.register("s")
	befriend(t, a, b)
	// OTK：每次 120 个，传 3 次，总量不超过上限，保留的是最新上传的
	var cnt map[string]int
	for round := 0; round < 3; round++ {
		list := []map[string]string{}
		for i := 0; i < 120; i++ {
			id := fmt.Sprintf("r%d-%d", round, i)
			list = append(list, map[string]string{"key_id": id, "key": "k" + id, "signature": "s"})
		}
		a.must("PUT", "/v1/keys", map[string]any{"identity_key": "ik", "signing_key": "sk", "one_time_keys": list}, &cnt)
	}
	if cnt["one_time_key_count"] != store.MaxOneTimeKeys {
		t.Fatalf("OTK 总量应封顶在 %d，得到 %d", store.MaxOneTimeKeys, cnt["one_time_key_count"])
	}
	db := e.rawDB()
	if n := scalar(t, db, "SELECT COUNT(*) FROM one_time_keys WHERE user_id = ? AND key_id LIKE 'r2-%'", a.ID); n != 120 {
		t.Fatalf("应保留最新上传的 OTK，r2 批次剩 %d", n)
	}
	// 可见名单：非好友与乱填的 id 被丢弃
	m := post(a, "vis", "include", []string{b.ID, stranger.ID, "not-a-uuid", b.ID})
	if m.Visibility == nil || len(m.Visibility.UserIDs) != 1 || m.Visibility.UserIDs[0] != b.ID {
		t.Fatalf("可见名单应只保留好友: %+v", m.Visibility)
	}
	// 每日上限 3：已经发了 1 条
	post(a, "2", "all", nil)
	post(a, "3", "all", nil)
	a.expectErr("POST", "/v1/moments", map[string]any{"text": "4"}, 429, "rate_limited")
	b.must("POST", "/v1/moments", map[string]any{"text": "b"}, nil)
	// 搜索限速
	for i := 0; i < 5; i++ {
		a.call("GET", "/v1/users/search?email=nobody@example.com", nil, nil)
	}
	a.expectErr("GET", "/v1/users/search?email="+b.Email, nil, 429, "rate_limited")
}

// A6：同一个 client_id 重发得到同一个 id，对方只收到一条（单聊与群消息）。
func TestA6IdempotentSend(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	c := e.register("c")
	befriend(t, a, b)
	befriend(t, a, c)
	msg := func(to, cid string) map[string]any {
		return map[string]any{"messages": []map[string]any{{"to": to, "type": 1, "body": "eA==", "client_id": cid}}}
	}
	var r1, r2, r3 sendResp
	a.must("POST", "/v1/messages", msg(b.ID, "cid-1"), &r1)
	a.must("POST", "/v1/messages", msg(b.ID, "cid-1"), &r2)
	if r1.Results[0].ID == "" || r1.Results[0].ID != r2.Results[0].ID || r1.Results[0].ServerTS != r2.Results[0].ServerTS || r2.Results[0].Error != "" {
		t.Fatalf("重发应返回原 id 与 server_ts: %+v vs %+v", r1.Results[0], r2.Results[0])
	}
	// 同一个 client_id 发给另一个人是另一条消息（客户端给多人分发密钥时各条用同一个 client_id）
	a.must("POST", "/v1/messages", msg(c.ID, "cid-1"), &r3)
	if r3.Results[0].ID == r1.Results[0].ID || r3.Results[0].Error != "" {
		t.Fatalf("不同接收方不应被当成重复: %+v", r3.Results[0])
	}
	// 同一个请求里重复的条目也只投递一次
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": "cid-2"},
		{"to": b.ID, "type": 1, "body": "eA==", "client_id": "cid-2"},
	}}, &r2)
	if r2.Results[0].ID != r2.Results[1].ID {
		t.Fatalf("同一请求里的重复条目应得到同一个 id: %+v", r2.Results)
	}
	// 不带 client_id 的不去重
	plain := map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 1, "body": "eA=="}}}
	a.must("POST", "/v1/messages", plain, nil)
	a.must("POST", "/v1/messages", plain, nil)
	var in inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &in)
	if len(in.Messages) != 4 {
		t.Fatalf("b 应收到 4 条（cid-1、cid-2 各一条 + 两条不带 client_id 的），得到 %d", len(in.Messages))
	}
	c.must("GET", "/v1/messages?limit=500", nil, &in)
	if len(in.Messages) != 1 {
		t.Fatalf("c 应收到 1 条，得到 %d", len(in.Messages))
	}
	// 群消息
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	var g1, g2 struct {
		ID       string `json:"id"`
		ServerTS int64  `json:"server_ts"`
	}
	gm := map[string]any{"session_id": "s", "body": "eA==", "client_id": "gcid"}
	a.must("POST", "/v1/groups/"+g.ID+"/messages", gm, &g1)
	a.must("POST", "/v1/groups/"+g.ID+"/messages", gm, &g2)
	if g1.ID != g2.ID || g1.ServerTS != g2.ServerTS {
		t.Fatalf("群消息重发应返回原 id: %+v %+v", g1, g2)
	}
	b.must("GET", "/v1/messages?limit=500", nil, &in)
	if len(in.Messages) != 5 {
		t.Fatalf("群消息只应投递一次，b 共 %d 条", len(in.Messages))
	}
	// 超过 24 小时的幂等键不再生效
	db := e.rawDB()
	if _, err := db.Exec("UPDATE send_dedup SET server_ts = server_ts - ?", (25 * time.Hour).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	a.must("POST", "/v1/messages", msg(b.ID, "cid-1"), &r2)
	if r2.Results[0].ID == r1.Results[0].ID {
		t.Fatal("24 小时之后同一个 client_id 应视为新消息")
	}
}

// A7：不能把别人上传的附件设成头像或放进动态；图片宽高有范围。
func TestA7AttachmentOwnershipAndDimensions(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	var mine, theirs upResp
	a.must("POST", "/v1/attachments", []byte("mine"), &mine)
	b.must("POST", "/v1/attachments", []byte("theirs"), &theirs)
	a.expectErr("PUT", "/v1/me/profile", map[string]any{"avatar_id": theirs.ID}, 404, "not_found")
	a.must("PUT", "/v1/me/profile", map[string]any{"avatar_id": mine.ID}, nil)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	a.expectErr("PUT", "/v1/groups/"+g.ID, map[string]any{"avatar_id": theirs.ID}, 404, "not_found")
	a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"avatar_id": mine.ID}, nil)
	media := func(id string, w, h int) map[string]any {
		return map[string]any{"text": "p", "media": []map[string]any{{"attachment_id": id, "width": w, "height": h}}}
	}
	a.expectErr("POST", "/v1/moments", media(theirs.ID, 10, 10), 404, "not_found")
	a.expectErr("POST", "/v1/moments", media(mine.ID, -5, 10), 400, "bad_request")
	a.expectErr("POST", "/v1/moments", media(mine.ID, 10, 1<<40), 400, "bad_request")
	a.must("POST", "/v1/moments", media(mine.ID, 1080, 1920), nil)
}

// A8：--token-ttl 小于 1 小时时仍是滑动过期——持续使用的令牌不会在 ttl 后失效。
func TestA8SlidingExpiryWithSmallTTL(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.TokenTTL = 1200 * time.Millisecond })
	a := e.register("a")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.must("GET", "/v1/me", nil, nil)
		time.Sleep(100 * time.Millisecond)
	}
	// 停止使用超过 ttl 之后才失效
	time.Sleep(1500 * time.Millisecond)
	a.expectErr("GET", "/v1/me", nil, 401, "unauthorized")
}

// A9：清理时移进回收站（.trash-）的文件，保留期从移入时算起（移入前先更新 mtime——rename 不改 mtime，
// 否则 90 天没动过的文件一进回收站就「已过期」）；元数据还在的（清理中途崩溃）恢复原名而不是删除。
func TestA9TrashRetention(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	var up, gone upResp
	a.must("POST", "/v1/attachments", []byte("still-referenced"), &up)
	a.must("POST", "/v1/attachments", []byte("really-deleted"), &gone)
	dir := filepath.Join(e.dataDir, "attachments")
	old := time.Now().Add(-100 * 24 * time.Hour)
	for _, id := range []string{up.ID, gone.ID} {
		if err := os.Chtimes(filepath.Join(dir, id), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// 用清理任务自己的移动方式把两个文件移进回收站，模拟它在「改名之后、删元数据之前」崩溃
	for _, id := range []string{up.ID, gone.ID} {
		if err := api.MoveToTrash(dir, id, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, ".trash-"+gone.ID))
	if err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("移入回收站时应更新 mtime: %v %v", info, err)
	}
	db := e.rawDB()
	if _, err := db.Exec("DELETE FROM attachments WHERE id = ?", gone.ID); err != nil { // 这一个的元数据确实已删
		t.Fatal(err)
	}
	if _, err := api.Cleanup(context.Background(), e.st, e.dataDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if status, body := a.call("GET", "/v1/attachments/"+up.ID, nil, nil); status != 200 || string(body) != "still-referenced" {
		t.Fatalf("元数据还在的附件应恢复原名并可下载，得到 %d", status)
	}
	if _, err := os.Stat(filepath.Join(dir, ".trash-"+gone.ID)); err != nil {
		t.Fatal("刚进回收站的文件应保留到保留期结束")
	}
	// 保留期之后才删除
	if _, err := api.Cleanup(context.Background(), e.st, e.dataDir, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".trash-"+gone.ID)); !os.IsNotExist(err) {
		t.Fatal("保留期过后应删除")
	}
}

// B4：上传中途崩溃留下的孤儿文件（有文件、没有元数据）由清理任务回收；刚上传的不动。
func TestB4OrphanAttachmentFilesSwept(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	var up upResp
	a.must("POST", "/v1/attachments", []byte("kept"), &up)
	dir := filepath.Join(e.dataDir, "attachments")
	orphanOld, orphanNew := store.NewID(), store.NewID()
	for _, id := range []string{orphanOld, orphanNew} {
		if err := os.WriteFile(filepath.Join(dir, id), []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, orphanOld), old, old)
	os.Chtimes(filepath.Join(dir, up.ID), old, old)
	if _, err := api.Cleanup(context.Background(), e.st, e.dataDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, orphanOld)); !os.IsNotExist(err) {
		t.Fatal("没有元数据的旧文件应被回收")
	}
	if _, err := os.Stat(filepath.Join(dir, orphanNew)); err != nil {
		t.Fatal("刚写入的文件可能正在登记元数据，不能删")
	}
	if _, err := os.Stat(filepath.Join(dir, up.ID)); err != nil {
		t.Fatal("有元数据的附件不能删")
	}
}

// B5：只增不减的表有清理。
func TestB5Pruning(t *testing.T) {
	e := newEnv(t, true)
	ctx := context.Background()
	a := e.register("a")
	b := e.register("b")
	c := e.register("c")
	befriend(t, a, b)
	// 一个申请了验证码、输错一次、再也没回来的邮箱
	e.do("POST", "/v1/auth/code", "", map[string]any{"email": "ghost@example.com"})
	e.do("POST", "/v1/auth/verify", "", map[string]any{"email": "ghost@example.com", "code": "000000"})
	var fr struct {
		ID string `json:"id"`
	}
	c.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": a.ID}, &fr)
	a.must("POST", "/v1/friends/requests/"+fr.ID+"/reject", nil, nil)
	m := post(a, "x", "all", nil)
	b.must("POST", "/v1/moments/"+m.ID+"/like", nil, nil)
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 1, "body": "eA==", "client_id": "c1"}}}, nil)
	db := e.rawDB()
	for _, table := range []string{"friend_requests", "moment_notifications", "send_dedup", "auth_codes", "auth_failures"} {
		if scalar(t, db, "SELECT COUNT(*) FROM "+table) == 0 {
			t.Fatalf("%s 应有数据", table)
		}
	}
	// 时间拨到很久以后再清理
	if _, err := api.Cleanup(ctx, e.st, e.dataDir, time.Now().Add(200*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM friend_requests WHERE status <> 'pending'",
		"SELECT COUNT(*) FROM moment_notifications",
		"SELECT COUNT(*) FROM send_dedup",
		"SELECT COUNT(*) FROM auth_codes",
		"SELECT COUNT(*) FROM auth_failures",
	} {
		if n := scalar(t, db, q); n != 0 {
			t.Fatalf("%s 清理后应为 0，得到 %d", q, n)
		}
	}
	// 好友关系、动态本身不受影响
	if scalar(t, db, "SELECT COUNT(*) FROM friendships") != 2 || scalar(t, db, "SELECT COUNT(*) FROM moments") != 1 {
		t.Fatal("清理不应动业务数据")
	}
}

// P5：/readyz 查库；请求并发打满时返回 503 而不是无限排队。
func TestP5ReadyzAndOverload(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.MaxInflight = 2 })
	if status, _ := e.do("GET", "/readyz", "", nil); status != 200 {
		t.Fatalf("/readyz 应为 200，得到 %d", status)
	}
	a := e.register("a")
	// 占住两个名额：两个不发完请求体的 POST
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			pr := &blockingReader{started: started, release: release, head: []byte(`{"messages":[`)}
			req, _ := http.NewRequest("POST", e.srv.URL+"/v1/messages", pr)
			req.Header.Set("Authorization", "Bearer "+a.Token)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}()
	}
	<-started
	<-started
	time.Sleep(100 * time.Millisecond)
	status, body := a.call("GET", "/v1/me", nil, nil)
	if status != 503 || !bytes.Contains(body, []byte(`"unavailable"`)) {
		t.Fatalf("名额用完应返回 503 unavailable，得到 %d %s", status, body)
	}
	// 健康检查与就绪探针不占名额
	if status, _ := e.do("GET", "/healthz", "", nil); status != 200 {
		t.Fatalf("/healthz 不应受并发上限影响: %d", status)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if status, _ := a.call("GET", "/v1/me", nil, nil); status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("名额释放后应恢复")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	head    []byte
	sent    bool
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, b.head)
		b.started <- struct{}{}
		return n, nil
	}
	<-b.release
	return 0, fmt.Errorf("aborted")
}

// B1：配置了可信代理后，登录限速按 X-Forwarded-For 里的客户端 IP 分桶；没配置时忽略该头。
func TestB1TrustedProxyRateLimit(t *testing.T) {
	proxies, _ := netx.ParseProxies("127.0.0.1")
	for _, tc := range []struct {
		name    string
		proxies *netx.Proxies
		second  int // 换一个 XFF 之后的期望状态
	}{
		{"可信代理", proxies, 200},
		{"未配置：忽略 XFF，所有请求共用对端 IP 的额度", nil, 429},
	} {
		e := newEnv(t, false, func(c *api.Config) { c.AuthCodePerIPPerMinute = 2; c.TrustedProxies = tc.proxies })
		codeReq := func(email, xff string) int {
			req, _ := http.NewRequest("POST", e.srv.URL+"/v1/auth/code", strings.NewReader(`{"email":"`+email+`"}`))
			req.Header.Set("X-Forwarded-For", xff)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		codeReq("a1@example.com", "198.51.100.1")
		codeReq("a2@example.com", "198.51.100.1")
		if got := codeReq("a3@example.com", "198.51.100.1"); got != 429 {
			t.Fatalf("%s: 同一客户端第 3 次应 429，得到 %d", tc.name, got)
		}
		if got := codeReq("a4@example.com", "198.51.100.2"); got != tc.second {
			t.Fatalf("%s: 另一个客户端 IP 应得到 %d，实际 %d", tc.name, tc.second, got)
		}
	}
}

// P5：连接数上限与每 IP 连接数上限。
func TestP5ConnectionLimits(t *testing.T) {
	e := newEnv(t, true)
	e.hub.SetLimits(0, 2)
	var conns []*wsClient
	for i := 0; i < 2; i++ {
		c := e.register(fmt.Sprint("u", i))
		conns = append(conns, c.dial())
	}
	c3 := e.register("u3")
	if status, _ := c3.call("GET", "/v1/ws", nil, nil); status != 429 {
		t.Fatalf("超过每 IP 连接数应返回 429，得到 %d", status)
	}
	conns[0].conn.CloseNow()
	deadline := time.Now().Add(3 * time.Second)
	for {
		url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/ws?token=" + c3.Token
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, _, err := wsDial(ctx, url)
		cancel()
		if err == nil {
			conn.CloseNow()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("旧连接断开后应能连上: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.hub.SetLimits(1, 0)
	c4 := e.register("u4")
	if status, _ := c4.call("GET", "/v1/ws", nil, nil); status != 503 {
		t.Fatalf("超过总连接数应返回 503，得到 %d", status)
	}
}

// A3：反复加好友 / 删好友、反复拉进群 / 踢出群，对方队列里的同类事件被合并，不会无限堆积；最终状态仍然正确。
func TestA3RepeatedFriendAndGroupEventsCoalesce(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	for i := 0; i < 8; i++ {
		befriend(t, b, a) // b 申请、a 接受 → b 收到 friend_accepted
		a.must("DELETE", "/v1/friends/"+b.ID, nil, nil)
	}
	befriend(t, b, a)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	for i := 0; i < 8; i++ {
		a.must("DELETE", "/v1/groups/"+g.ID+"/members/"+b.ID, nil, nil)
		a.must("POST", "/v1/groups/"+g.ID+"/members", map[string]any{"user_ids": []string{b.ID}}, nil)
	}
	var in inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &in)
	count := map[string]int{}
	var order []string
	for _, ev := range in.Events {
		typ := ev["type"].(string)
		count[typ]++
		order = append(order, typ)
	}
	for _, typ := range []string{"friend_accepted", "friend_removed", "group_removed", "group_updated"} {
		if count[typ] != 1 {
			t.Fatalf("%s 应合并成 1 条，得到 %d（%v）", typ, count[typ], order)
		}
	}
	// 最终状态：是好友、在群里——各类事件里最后发生的那一条排在后面
	pos := map[string]int{}
	for i, typ := range order {
		pos[typ] = i
	}
	if pos["friend_accepted"] < pos["friend_removed"] || pos["group_updated"] < pos["group_removed"] {
		t.Fatalf("事件顺序应反映最终状态: %v", order)
	}
}
