package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// 人数上限在加人事务内检查：并发加人不会超过上限，已是成员的人不占新名额。
func TestAddMembersCapConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	owner := newUser(t, s, "owner")
	g, err := s.CreateGroup(ctx, owner, "g", nil)
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for i := 0; i < 10; i++ {
		users = append(users, newUser(t, s, fmt.Sprint("u", i)))
	}
	const max = 5
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.AddMembers(ctx, g.ID, users[i*2:i*2+2], max)
			if err != nil && err != ErrTooManyMembers {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	g, err = s.GetGroup(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range g.Members {
		ids = append(ids, m.UserID)
	}
	if len(ids) != max {
		t.Fatalf("并发加人后应恰好 %d 人，得到 %d", max, len(ids))
	}
	// 重复添加已有成员不算超限
	if err := s.AddMembers(ctx, g.ID, ids[1:], max); err != nil {
		t.Fatalf("已是成员的人不占新名额: %v", err)
	}
}

// 挑出清理候选之后才被设为头像的附件不会被删（条件与删除在同一条语句里）。
func TestDeleteStaleAttachmentsRechecksReferences(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a")
	for _, id := range []string{"x1", "x2"} {
		if err := s.CreateAttachment(ctx, Attachment{ID: id, UploaderID: uid, Size: 1, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := s.StaleAttachments(ctx, 100, 10)
	if err != nil || len(ids) != 2 {
		t.Fatalf("候选: %v %v", ids, err)
	}
	avatar := "x1"
	if _, err := s.UpdateProfile(ctx, uid, ProfileUpdate{AvatarID: &avatar}); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.DeleteStaleAttachments(ctx, ids, 100)
	if err != nil || len(deleted) != 1 || !deleted["x2"] {
		t.Fatalf("只应删除仍未被引用的 x2: %v %v", deleted, err)
	}
	if _, err := s.GetAttachment(ctx, "x1"); err != nil {
		t.Fatalf("刚设为头像的附件不应被删: %v", err)
	}
}
