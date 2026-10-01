package store

import (
	"context"
	"sort"
	"strings"
	"testing"
)

func momentMedia(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT moment_id || ':' || attachment_id FROM moment_media")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		rows.Scan(&v)
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// v5 迁移：从已有动态的 media JSON 回填 moment_media（重复图片只记一行，无 attachment_id 的项跳过）。
func TestMigrationMomentMediaBackfill(t *testing.T) {
	dir := t.TempDir()
	db := openAtVersion(t, dir, 4)
	if _, err := db.Exec(`INSERT INTO users (id, email, created_at, updated_at) VALUES ('u1', 'a@example.com', 1, 1);
		INSERT INTO attachments (id, uploader_id, size, created_at, last_access) VALUES ('att1', 'u1', 1, 1, 1), ('att2', 'u1', 1, 1, 1), ('att3', 'u1', 1, 1, 1);
		INSERT INTO moments (id, author_id, text, media, created_at) VALUES
			('m1', 'u1', '', '[{"attachment_id":"att1","width":1,"height":1},{"attachment_id":"att2","width":1,"height":1},{"attachment_id":"att1","width":1,"height":1}]', 1),
			('m2', 'u1', 'text only', '[]', 2),
			('m3', 'u1', '', '[{"width":1,"height":1}]', 3);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := momentMedia(t, s)
	if strings.Join(got, ",") != "m1:att1,m1:att2" {
		t.Fatalf("回填结果不对: %v", got)
	}
	// 回填后引用关系生效：att1/att2 被动态引用，att3 未被引用
	stale, err := s.StaleAttachments(context.Background(), 1<<62, 100)
	if err != nil || strings.Join(stale, ",") != "att3" {
		t.Fatalf("只有 att3 应可清理: %v %v", stale, err)
	}
}

// 发动态写关联表；删除动态时级联删除，附件随之变为可清理。
func TestMomentMediaLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	u, err := s.Login(ctx, "a@example.com", "android", "t", "h")
	if err != nil {
		t.Fatal(err)
	}
	uid := u.User.ID
	if err := s.CreateAttachment(ctx, Attachment{ID: "img", UploaderID: uid, Size: 1, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	row, err := s.CreateMoment(ctx, uid, "", []Media{{AttachmentID: "img"}, {AttachmentID: "img"}}, Visibility{Mode: VisAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := momentMedia(t, s); len(got) != 1 || got[0] != row.ID+":img" {
		t.Fatalf("关联表应有一行: %v", got)
	}
	if stale, _ := s.StaleAttachments(ctx, 1<<62, 100); len(stale) != 0 {
		t.Fatalf("被动态引用的附件不应可清理: %v", stale)
	}
	if err := s.DeleteMoment(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if got := momentMedia(t, s); len(got) != 0 {
		t.Fatalf("删除动态应级联删除关联: %v", got)
	}
	deleted, err := s.DeleteStaleAttachments(ctx, []string{"img"}, 1<<62)
	if err != nil || !deleted["img"] {
		t.Fatalf("动态删除后附件应可清理: %v %v", deleted, err)
	}
}

// 清理条件不再扫描 moments 表（走 moment_media 与头像索引）。
func TestStaleAttachmentsPlanAvoidsMoments(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN SELECT id FROM attachments WHERE "+staleCond+" LIMIT 10", int64(0))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	for _, line := range plan {
		// 期望全部是 SEARCH（索引查找），不应出现全表扫描或对 moments / json_each 的访问
		if strings.HasPrefix(line, "SCAN") || strings.Contains(line, "moments") || strings.Contains(line, "json_each") {
			t.Fatalf("查询计划不应有全表扫描或扫描动态:\n%s", joined)
		}
	}
	if !strings.Contains(joined, "moment_media_attachment") {
		t.Fatalf("应使用 moment_media 索引:\n%s", joined)
	}
}
