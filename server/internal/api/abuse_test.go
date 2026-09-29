package api_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"hush/server/internal/api"
)

// 多个发送方并发发送时，接收方队列配额仍然严格生效（配额检查在入队事务内）。
func TestConcurrentSendsRespectQueueQuota(t *testing.T) {
	const maxItems = 20
	e := newEnv(t, true, func(c *api.Config) { c.QueueMaxItems = maxItems })
	b := e.register("b")
	var senders []*client
	for i := 0; i < 4; i++ {
		a := e.register("a")
		befriend(t, a, b) // b 队列里每人一个 friend_request 事件
		senders = append(senders, a)
	}
	var wg sync.WaitGroup
	for _, a := range senders {
		wg.Add(1)
		go func(a *client) {
			defer wg.Done()
			msgs := []map[string]any{}
			for i := 0; i < 10; i++ {
				msgs = append(msgs, map[string]any{"to": b.ID, "type": 1, "body": "eA==", "client_id": fmt.Sprint("c", i)})
			}
			a.call("POST", "/v1/messages", map[string]any{"messages": msgs}, nil)
		}(a)
	}
	wg.Wait()
	n, _, err := e.st.QueueUsage(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != maxItems {
		t.Fatalf("并发发送后队列应恰好填满到 %d 条，得到 %d", maxItems, n)
	}
}

// GET /v1/messages 单页按累计字节截断（至少 1 条），剩余的下一页取。
func TestFetchMessagesByteCap(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	big := strings.Repeat("A", 1<<20)
	for i := 0; i < 10; i++ {
		var sr sendResp
		a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": b.ID, "type": 1, "body": big, "client_id": fmt.Sprint("c", i)}}}, &sr)
		if sr.Results[0].Error != "" {
			t.Fatalf("发送失败: %+v", sr)
		}
	}
	var inbox inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	got := len(inbox.Messages) + len(inbox.Events)
	if got < 1 || got >= 10 {
		t.Fatalf("单页应按字节截断: 得到 %d 条", got)
	}
	ids := []string{}
	for _, m := range inbox.Messages {
		ids = append(ids, m.ID)
	}
	for _, ev := range inbox.Events {
		ids = append(ids, ev["id"].(string))
	}
	b.must("POST", "/v1/messages/ack", map[string]any{"ids": ids}, nil)
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	if len(inbox.Messages) == 0 {
		t.Fatal("剩余信封应在下一页返回")
	}
}

// 陌生人反复发好友申请：受害者队列里只保留一条 friend_request 事件（审查 S1 的复现）。
func TestFriendRequestFloodCoalesced(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.QueueMaxItems = 5 })
	a := e.register("a")
	b := e.register("b")
	for i := 0; i < 50; i++ {
		a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID, "message": fmt.Sprint("hi ", i)}, nil)
	}
	var inbox inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	if len(inbox.Events) != 1 {
		t.Fatalf("应只保留最新一条 friend_request 事件，得到 %d", len(inbox.Events))
	}
	req := inbox.Events[0]["request"].(map[string]any)
	if req["message"] != "hi 49" {
		t.Fatalf("保留的应是最新一次推送: %v", req["message"])
	}
}

// 反复修改群资料 / 个人资料：每个成员（联系人）队列里只保留最新的一条。
func TestGroupAndProfileUpdatesCoalesced(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	for i := 0; i < 20; i++ {
		a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"announcement": fmt.Sprint("n", i)}, nil)
		a.must("PUT", "/v1/me/profile", map[string]any{"about": fmt.Sprint("about", i)}, nil)
	}
	var inbox inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	counts := map[string]int{}
	for _, ev := range inbox.Events {
		counts[ev["type"].(string)]++
	}
	if counts["group_updated"] != 1 || counts["profile_updated"] != 1 {
		t.Fatalf("同一主体的事件应合并: %v", counts)
	}
	// 合并后保留的是最新状态
	for _, ev := range inbox.Events {
		if ev["type"] == "group_updated" && ev["group"].(map[string]any)["announcement"] != "n19" {
			t.Fatalf("应保留最新群资料: %v", ev["group"])
		}
	}

	// 群里有新消息之后再改资料：旧事件在信封之前，不能合并（保持事件与消息的相对顺序）
	a.must("POST", "/v1/groups/"+g.ID+"/messages", megolm("m"), nil)
	a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"announcement": "after"}, nil)
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	counts = map[string]int{}
	for _, ev := range inbox.Events {
		counts[ev["type"].(string)]++
	}
	if counts["group_updated"] != 2 || len(inbox.Messages) != 1 {
		t.Fatalf("信封之前的 group_updated 应保留: %v, 信封 %d", counts, len(inbox.Messages))
	}
}

// 并发上传不能突破 24 小时上传配额（审查 S5 的复现：配额 1MB 时 10 个 600KB 并发上传曾有 2 个成功）。
func TestConcurrentUploadsRespectDailyQuota(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.DailyUploadBytes = 1 << 20 })
	a := e.register("a")
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = a.call("POST", "/v1/attachments", make([]byte, 600<<10), nil)
		}(i)
	}
	wg.Wait()
	okN := 0
	for _, st := range statuses {
		switch st {
		case 200:
			okN++
		case 429:
		default:
			t.Fatalf("意外状态: %v", statuses)
		}
	}
	if okN != 1 {
		t.Fatalf("配额 1MB 下 600KB 的并发上传应只成功 1 个: %v", statuses)
	}
	// 失败的上传释放了预占：剩余额度仍可用
	a.must("POST", "/v1/attachments", make([]byte, 300<<10), nil)
}

// 会向他人队列写事件的写接口按 (用户, 操作类别) 限速。
func TestWriteRateLimit(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.WritesPerMinute = 3 })
	a := e.register("a")
	b := e.register("b")
	for i := 0; i < 3; i++ {
		a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, nil)
	}
	a.expectErr("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, 429, "rate_limited")
	a.must("PUT", "/v1/me/profile", map[string]any{"about": "x"}, nil)              // 不同类别互不影响
	b.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": a.ID}, nil) // 不同用户互不影响
}

// 未认证的登录接口按对端 IP 限速。
func TestAuthPerIPRateLimit(t *testing.T) {
	e := newEnv(t, false, func(c *api.Config) { c.AuthCodePerIPPerMinute = 2; c.AuthVerifyPerIPPerMinute = 3 })
	for i := 0; i < 2; i++ {
		if st, body := e.do("POST", "/v1/auth/code", "", map[string]any{"email": fmt.Sprintf("u%d@example.com", i)}); st != 200 {
			t.Fatalf("前 2 次应成功: %d %s", st, body)
		}
	}
	st, body := e.do("POST", "/v1/auth/code", "", map[string]any{"email": "u9@example.com"})
	assertErr(t, st, body, 429, "rate_limited")
	for i := 0; i < 3; i++ {
		st, body := e.do("POST", "/v1/auth/verify", "", map[string]any{"email": "u0@example.com", "code": "000000"})
		assertErr(t, st, body, 401, "unauthorized")
	}
	st, body = e.do("POST", "/v1/auth/verify", "", map[string]any{"email": "u0@example.com", "code": "000000"})
	assertErr(t, st, body, 429, "rate_limited")
}
