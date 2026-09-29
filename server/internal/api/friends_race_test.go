package api_test

import (
	"sync"
	"testing"
)

// 客户端重试导致的并发重复申请：不会产生重复的待处理申请，也不会 500（审查 S11）。
func TestConcurrentDuplicateFriendRequests(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = a.call("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID, "message": "hi"}, nil)
		}(i)
	}
	wg.Wait()
	for _, st := range statuses {
		if st != 200 {
			t.Fatalf("重复申请应都返回 200: %v", statuses)
		}
	}
	var lists struct {
		Incoming []map[string]any `json:"incoming"`
	}
	b.must("GET", "/v1/friends/requests", nil, &lists)
	if len(lists.Incoming) != 1 {
		t.Fatalf("应只有一条待处理申请，得到 %d", len(lists.Incoming))
	}
}

// 互相同时发申请：结果必然是成为好友，不会留下两条交叉的待处理申请。
func TestConcurrentMutualFriendRequests(t *testing.T) {
	for round := 0; round < 5; round++ {
		e := newEnv(t, true)
		a := e.register("a")
		b := e.register("b")
		var wg sync.WaitGroup
		for _, pair := range [][2]*client{{a, b}, {b, a}} {
			wg.Add(1)
			go func(from, to *client) {
				defer wg.Done()
				from.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": to.ID}, nil)
			}(pair[0], pair[1])
		}
		wg.Wait()
		var friends []map[string]any
		a.must("GET", "/v1/friends", nil, &friends)
		var lists struct {
			Incoming []map[string]any `json:"incoming"`
			Outgoing []map[string]any `json:"outgoing"`
		}
		a.must("GET", "/v1/friends/requests", nil, &lists)
		if len(friends) != 1 || len(lists.Incoming)+len(lists.Outgoing) != 0 {
			t.Fatalf("互相申请应直接成为好友: friends=%d pending=%+v", len(friends), lists)
		}
	}
}

// 并发接受同一条申请：只有一次成功，申请人只收到一条 friend_accepted。
func TestConcurrentAccept(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	var fr struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/friends/requests", map[string]any{"to_user_id": b.ID}, &fr)
	var wg sync.WaitGroup
	statuses := make([]int, 5)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], _ = b.call("POST", "/v1/friends/requests/"+fr.ID+"/accept", nil, nil)
		}(i)
	}
	wg.Wait()
	okN := 0
	for _, st := range statuses {
		switch st {
		case 200:
			okN++
		case 409:
		default:
			t.Fatalf("意外状态: %v", statuses)
		}
	}
	if okN != 1 {
		t.Fatalf("应恰好一次接受成功: %v", statuses)
	}
	var inbox inboxResp
	a.must("GET", "/v1/messages?limit=500", nil, &inbox)
	n := 0
	for _, ev := range inbox.Events {
		if ev["type"] == "friend_accepted" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("friend_accepted 应只发一次，得到 %d", n)
	}
}
