package store

import (
	"context"
	"testing"
)

// 同一毫秒的多条动态/通知跨页时不漏条：(created_at, id) 复合游标。
func TestCursorSameMillisecond(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.Login(ctx, "a@example.com", "android", "t", "tokhash")
	if err != nil {
		t.Fatal(err)
	}
	uid := u.User.ID
	const ts = 1_700_000_000_000
	ids := []string{}
	for i := 0; i < 5; i++ {
		row, err := s.CreateMoment(ctx, uid, "x", nil, Visibility{Mode: VisAll}, 0)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.ID)
		if _, err := s.CreateNotification(ctx, NotificationInsert{UserID: uid, Kind: "like", MomentID: row.ID, FromID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	// 全部压到同一毫秒
	if _, err := s.db.ExecContext(ctx, "UPDATE moments SET created_at = ?", ts); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE moment_notifications SET created_at = ?", ts); err != nil {
		t.Fatal(err)
	}

	// 动态流：每页 2 条，用上一页最后一条的 (created_at, id) 翻页
	seen := map[string]bool{}
	cur := Cursor{Before: 1 << 62}
	for page := 0; page < 5; page++ {
		rows, err := s.Feed(ctx, uid, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if seen[r.ID] {
				t.Fatalf("重复返回 %s", r.ID)
			}
			seen[r.ID] = true
		}
		last := rows[len(rows)-1]
		cur = Cursor{Before: last.CreatedAt, BeforeID: last.ID}
	}
	if len(seen) != 5 {
		t.Fatalf("同一毫秒的 5 条动态应全部分页取到，得到 %d", len(seen))
	}
	// 旧语义（不带 before_id）：同一毫秒的其余条目被跳过
	rows, _ := s.Feed(ctx, uid, Cursor{Before: ts}, 10)
	if len(rows) != 0 {
		t.Fatalf("不带 before_id 时保持 created_at < before 语义: %d", len(rows))
	}
	// 作者动态同理
	rows, _ = s.UserMoments(ctx, uid, uid, Cursor{Before: ts, BeforeID: ids[4]}, 10)
	if len(rows) != 4 {
		t.Fatalf("UserMoments 游标应返回同毫秒内更早的 4 条，得到 %d", len(rows))
	}

	// 通知
	got := map[string]bool{}
	ncur := Cursor{Before: 1 << 62}
	for page := 0; page < 5; page++ {
		list, err := s.Notifications(ctx, uid, ncur, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			break
		}
		for _, n := range list {
			got[n.ID] = true
		}
		last := list[len(list)-1]
		ncur = Cursor{Before: last.CreatedAt, BeforeID: last.ID}
	}
	if len(got) != 5 {
		t.Fatalf("同一毫秒的 5 条通知应全部分页取到，得到 %d", len(got))
	}
}
