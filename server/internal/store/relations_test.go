package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// S-F：Relations 一次集合查询得到的结果与逐个 Relation 完全一致，且不受 SQL 变量个数限制。
func TestRelationsSetQuery(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	login := func(email string) string {
		t.Helper()
		r, err := s.Login(ctx, email, "android", "t", "h-"+email)
		if err != nil {
			t.Fatal(err)
		}
		return r.User.ID
	}
	me := login("me@example.com")
	friend := login("friend@example.com")
	mate := login("mate@example.com")
	stranger := login("stranger@example.com")
	blocker := login("blocker@example.com")
	blockedByMe := login("victim@example.com")
	if err := makeFriendsTx(s, me, friend); err != nil {
		t.Fatal(err)
	}
	if err := makeFriendsTx(s, me, blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup(ctx, me, "g", []string{mate}); err != nil {
		t.Fatal(err)
	}
	if err := s.Block(ctx, blocker, me); err != nil {
		t.Fatal(err)
	}
	if err := s.Block(ctx, me, blockedByMe); err != nil {
		t.Fatal(err)
	}
	missing := "00000000-0000-0000-0000-000000000000"
	ids := []string{friend, mate, stranger, blocker, blockedByMe, missing, friend}
	rels, err := s.Relations(ctx, me, ids)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Relation{
		friend:      {Exists: true, Friends: true},
		mate:        {Exists: true, ShareGroup: true},
		stranger:    {Exists: true},
		blocker:     {Exists: true, Blocked: true, Friends: true},
		blockedByMe: {Exists: true, Blocked: true},
		missing:     {},
	}
	if len(rels) != len(want) {
		t.Fatalf("应按 id 去重返回 %d 项，得到 %d", len(want), len(rels))
	}
	for id, w := range want {
		if rels[id] != w {
			t.Fatalf("%s: 期望 %+v，得到 %+v", id, w, rels[id])
		}
		single, err := s.Relation(ctx, me, id)
		if err != nil || single != w {
			t.Fatalf("Relation(%s) 与集合查询不一致: %+v %v", id, single, err)
		}
	}
	// 大批量（超过单条语句的 SQL 变量上限也没关系：ids 作为一个 JSON 参数传入）
	var many []string
	for i := 0; i < 40000; i++ {
		many = append(many, fmt.Sprintf("x-%d", i))
	}
	many = append(many, friend)
	rels, err = s.Relations(ctx, me, many)
	if err != nil || len(rels) != len(many) || !rels[friend].Friends {
		t.Fatalf("大批量关系查询失败: %d %v", len(rels), err)
	}
}

func makeFriendsTx(s *Store, a, b string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error { return makeFriends(context.Background(), tx, a, b, NowMillis()) })
}
