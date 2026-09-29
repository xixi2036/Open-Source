package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newUser(t *testing.T, s *Store, name string) string {
	t.Helper()
	res, err := s.Login(context.Background(), name+"@example.com", "android", "t", "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	return res.User.ID
}

func mustUsage(t *testing.T, s *Store, uid string) (int64, int64) {
	t.Helper()
	n, b, err := s.QueueUsage(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	return n, b
}

// 用量按字节（而不是字符）统计，并由触发器在入队、ack、过期清理、重新登录时同步维护。
func TestQueueUsageCountsBytes(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a")
	payload := []byte(`{"text":"` + strings.Repeat("中", 1000) + `"}`) // 3000 字节 + 外壳，约 1000 个字符
	if _, err := s.Enqueue(ctx, []QueueInsert{
		{UserID: uid, ID: "e1", Kind: KindEvent, Payload: payload},
		{UserID: uid, ID: "m1", Kind: KindEnvelope, Payload: []byte(`{"x":1}`)},
		{UserID: uid, ID: "m1", Kind: KindEnvelope, Payload: []byte(`{"x":1}`)}, // 重复 id 忽略
	}, QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	n, b := mustUsage(t, s, uid)
	if n != 2 || b != int64(len(payload)+7) {
		t.Fatalf("用量应为 2 条 / %d 字节，得到 %d / %d", len(payload)+7, n, b)
	}
	if _, err := s.Ack(ctx, uid, []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if n, b := mustUsage(t, s, uid); n != 1 || b != int64(len(payload)) {
		t.Fatalf("ack 后用量应减少: %d / %d", n, b)
	}
	if _, err := s.PruneQueue(ctx, NowMillis()+1); err != nil {
		t.Fatal(err)
	}
	if n, b := mustUsage(t, s, uid); n != 0 || b != 0 {
		t.Fatalf("清理后用量应归零: %d / %d", n, b)
	}
	if _, err := s.Enqueue(ctx, []QueueInsert{{UserID: uid, ID: "m2", Kind: KindEnvelope, Payload: []byte("{}")}}, QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "a@example.com", "ios", "t2", "tok-a2"); err != nil { // 重新登录清空队列
		t.Fatal(err)
	}
	if n, b := mustUsage(t, s, uid); n != 0 || b != 0 {
		t.Fatalf("重新登录后用量应归零: %d / %d", n, b)
	}
}

// 配额检查在入队事务内：超限的信封被拒收，事件不受限。
func TestEnqueueLimits(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a")
	lim := QueueLimits{MaxItems: 3, MaxBytes: 100}
	items := []QueueInsert{
		{UserID: uid, ID: "m1", Kind: KindEnvelope, Payload: []byte(strings.Repeat("a", 40))},
		{UserID: uid, ID: "m2", Kind: KindEnvelope, Payload: []byte(strings.Repeat("a", 40))},
		{UserID: uid, ID: "m3", Kind: KindEnvelope, Payload: []byte(strings.Repeat("a", 40))}, // 字节超限
		{UserID: uid, ID: "m4", Kind: KindEnvelope, Payload: []byte(strings.Repeat("a", 10))},
		{UserID: uid, ID: "m5", Kind: KindEnvelope, Payload: []byte("a")}, // 条数超限
		{UserID: uid, ID: "e1", Kind: KindEvent, Payload: []byte(strings.Repeat("a", 500))},
	}
	acc, err := s.Enqueue(ctx, items, lim)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, true, false, true, false, true}
	if fmt.Sprint(acc) != fmt.Sprint(want) {
		t.Fatalf("接收结果 %v，期望 %v", acc, want)
	}
}

// 合并键：同键旧事件被替换；但旧事件之后已有信封时保留（不改变事件与信封的相对顺序）。
func TestEnqueueCoalesce(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a")
	ev := func(id string) QueueInsert {
		return QueueInsert{UserID: uid, ID: id, Kind: KindEvent, Payload: []byte(`{"id":"` + id + `"}`), CoalesceKey: "group_updated:g"}
	}
	ids := func() []string {
		items, err := s.QueueAfter(ctx, uid, 0, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.ID)
		}
		return out
	}
	for _, id := range []string{"g1", "g2", "g3"} {
		if _, err := s.Enqueue(ctx, []QueueInsert{ev(id)}, QueueLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(); fmt.Sprint(got) != "[g3]" {
		t.Fatalf("同键事件应只保留最新一条: %v", got)
	}
	if _, err := s.Enqueue(ctx, []QueueInsert{{UserID: uid, ID: "m1", Kind: KindEnvelope, Payload: []byte("{}")}, ev("g4")}, QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	if got := ids(); fmt.Sprint(got) != "[g3 m1 g4]" {
		t.Fatalf("旧事件之后有信封时不能合并: %v", got)
	}
	if _, err := s.Enqueue(ctx, []QueueInsert{ev("g5")}, QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	if got := ids(); fmt.Sprint(got) != "[g3 m1 g5]" {
		t.Fatalf("信封之后的同键事件可以合并: %v", got)
	}
	if n, _ := mustUsage(t, s, uid); n != 3 {
		t.Fatalf("合并删除也应同步用量: %d", n)
	}
}

// 单页按累计字节截断，至少返回 1 条。
func TestQueueAfterMaxBytes(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a")
	var ins []QueueInsert
	for i := 0; i < 5; i++ {
		ins = append(ins, QueueInsert{UserID: uid, ID: fmt.Sprint(i), Kind: KindEnvelope, Payload: []byte(strings.Repeat("a", 100))})
	}
	if _, err := s.Enqueue(ctx, ins, QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		max  int64
		want int
	}{{250, 2}, {50, 1}, {0, 5}, {500, 5}} {
		items, err := s.QueueAfter(ctx, uid, 0, 100, c.max)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != c.want {
			t.Fatalf("maxBytes=%d 应返回 %d 条，得到 %d", c.max, c.want, len(items))
		}
	}
}

// 从 v2 升级：回填字节用量、清理重复的待处理好友申请后建唯一索引。
func TestMigrateFromV2(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "hush.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:2] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO users (id, email, created_at, updated_at) VALUES ('a', 'a@x', 0, 0), ('b', 'b@x', 0, 0)`,
		`INSERT INTO queue (user_id, id, kind, payload, created_at) VALUES ('a', 'q1', 'event', '"中文"', 0), ('a', 'q2', 'envelope', '{}', 0)`,
		`INSERT INTO friend_requests (id, from_id, to_id, status, created_at, updated_at) VALUES
			('r1', 'a', 'b', 'pending', 1, 1), ('r2', 'a', 'b', 'pending', 2, 2), ('r3', 'a', 'b', 'rejected', 0, 0), ('r4', 'b', 'a', 'pending', 3, 3)`,
		`INSERT INTO auth_codes (email, code, expires_at, sent_at) VALUES ('a@x', '1', 0, 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级失败: %v", err)
	}
	defer s.Close()
	if n, b := mustUsage(t, s, "a"); n != 2 || b != int64(len(`"中文"`)+2) {
		t.Fatalf("回填用量错误: %d / %d", n, b)
	}
	in, out, err := s.PendingRequests(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "r2" || len(in) != 1 {
		t.Fatalf("重复的待处理申请应只保留最新一条: in=%v out=%v", in, out)
	}
	if _, err := s.db.Exec(`INSERT INTO friend_requests (id, from_id, to_id, status, created_at, updated_at) VALUES ('r5', 'a', 'b', 'pending', 5, 5)`); err == nil {
		t.Fatal("同一 (from,to) 不应允许第二条待处理申请")
	}
}

// 上传密钥时设备代按 token 在事务内确定：已被重新登录作废的 token 不能以新设备代写入旧身份密钥。
func TestUploadKeysUsesTokenGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a") // token tok-a，generation 1
	if _, err := s.Login(ctx, "a@example.com", "ios", "new", "tok-a2"); err != nil {
		t.Fatal(err)
	}
	up := KeyUpload{IdentityKey: "old-ik", SigningKey: "old-sk", TokenHash: "tok-a"}
	if _, err := s.UploadKeys(ctx, uid, up); err != ErrNotFound {
		t.Fatalf("已作废 token 上传应返回 ErrNotFound，得到 %v", err)
	}
	up = KeyUpload{IdentityKey: "new-ik", SigningKey: "new-sk", TokenHash: "tok-a2"}
	if _, err := s.UploadKeys(ctx, uid, up); err != nil {
		t.Fatal(err)
	}
	id, err := s.GetIdentity(ctx, uid)
	if err != nil || id.IdentityKey != "new-ik" || id.DeviceGeneration != 2 {
		t.Fatalf("应以新设备代写入新身份密钥: %+v %v", id, err)
	}
	if n, err := s.AckAs(ctx, uid, "tok-a", nil); err != ErrNotFound || n != 0 {
		t.Fatalf("已作废 token 的 ack 应被拒: %d %v", n, err)
	}
}
