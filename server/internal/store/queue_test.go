package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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

// 合并键：同键旧事件被替换；旧事件之后已有信封时，最早那条原地换成最新状态（不改变事件与信封的相对顺序）。
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
	// 旧事件之后有信封：它不能挪到后面，于是原地换成最新状态（新 id），队尾再放一条——同键最多两条
	got := ids()
	if len(got) != 3 || got[0] == "g3" || got[1] != "m1" || got[2] != "g4" {
		t.Fatalf("应为 [<原地替换的新 id> m1 g4]: %v", got)
	}
	items, _ := s.QueueAfter(ctx, uid, 0, 100, 0)
	if want := `{"id":"` + got[0] + `"}`; string(items[0].Payload) != want {
		t.Fatalf("原地替换的事件应带最新状态与新 id: %s", items[0].Payload)
	}
	if _, err := s.Enqueue(ctx, []QueueInsert{ev("g5")}, QueueLimits{}); err != nil {
		t.Fatal(err)
	}
	got2 := ids()
	if len(got2) != 3 || got2[0] == got[0] || got2[1] != "m1" || got2[2] != "g5" {
		t.Fatalf("信封之后的同键事件合并，最早那条再次换成最新状态: %v", got2)
	}
	if n, _ := mustUsage(t, s, uid); n != 3 {
		t.Fatalf("合并删除也应同步用量: %d", n)
	}
	// A3：事件与信封交替也不能让同键事件堆积
	for i := 0; i < 30; i++ {
		if _, err := s.Enqueue(ctx, []QueueInsert{{UserID: uid, ID: fmt.Sprint("x", i), Kind: KindEnvelope, Payload: []byte("{}")}, ev(fmt.Sprint("h", i))}, QueueLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	var events int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM queue WHERE user_id = ? AND kind = 'event'", uid).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 2 {
		t.Fatalf("交替 30 次后同键事件应只有 2 条，得到 %d", events)
	}
	all := ids()
	if all[len(all)-1] != "h29" || all[1] != "m1" {
		t.Fatalf("最早的位置与队尾各一条: %v", all)
	}
	n, b := mustUsage(t, s, uid)
	var cn, cb int64
	if err := s.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM queue WHERE user_id = ?", uid).Scan(&cn, &cb); err != nil {
		t.Fatal(err)
	}
	if n != cn || b != cb {
		t.Fatalf("用量计数器应与队列一致: %d/%d vs %d/%d", n, b, cn, cb)
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

// A11：库版本高于程序已知的迁移时拒绝打开（旧程序不能打开新库）。
func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "hush.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(dir); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("版本 99 的库应拒绝打开，得到 %v", err)
	}
}

// A11：升级已有的库之前自动备份；新建的库不备份。v7 迁移把多人共享的正文搬进 envelopes、回填发送方与信封用量。
func TestMigrationBackupAndV7DataMove(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openAtVersion(t, dir, 6)
	megolm := `{"id":"g1","kind":"megolm","from":"alice","type":0,"group_id":"grp","session_id":"s","body":"QUJD","server_ts":5}`
	olm := `{"id":"o1","kind":"olm","from":"alice","type":1,"group_id":null,"session_id":null,"body":"eA==","server_ts":6}`
	event := `{"id":"e1","type":"friend_accepted","user_id":"alice"}`
	for _, q := range []string{
		`INSERT INTO users (id, email, created_at, updated_at) VALUES ('alice', 'a@x', 0, 0), ('bob', 'b@x', 0, 0), ('carol', 'c@x', 0, 0)`,
		`INSERT INTO queue (user_id, id, kind, payload, created_at, size) VALUES
			('bob', 'g1', 'envelope', '` + megolm + `', 5, ` + fmt.Sprint(len(megolm)) + `),
			('carol', 'g1', 'envelope', '` + megolm + `', 5, ` + fmt.Sprint(len(megolm)) + `),
			('bob', 'o1', 'envelope', '` + olm + `', 6, ` + fmt.Sprint(len(olm)) + `),
			('bob', 'e1', 'event', '` + event + `', 7, ` + fmt.Sprint(len(event)) + `)`,
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
	backups, _ := filepath.Glob(filepath.Join(dir, "premigrate", "hush-v6-*.db"))
	if len(backups) != 1 {
		t.Fatalf("升级前应留一份备份，得到 %v", backups)
	}
	bdb, err := sql.Open("sqlite", "file:"+backups[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var ver, rows int
	bdb.QueryRow("PRAGMA user_version").Scan(&ver)
	bdb.QueryRow("SELECT COUNT(*) FROM queue").Scan(&rows)
	bdb.Close()
	if ver != 6 || rows != 4 {
		t.Fatalf("备份应是升级前的完整副本: 版本 %d，队列 %d 行", ver, rows)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "backups", "*")); len(entries) != 0 {
		t.Fatalf("升级前备份含信封，不能放进后台可下载的 backups/ 目录: %v", entries)
	}
	// 数据搬迁：内容不变、顺序不变
	items, err := s.QueueAfter(ctx, "bob", 0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || string(items[0].Payload) != megolm || string(items[1].Payload) != olm || string(items[2].Payload) != event {
		t.Fatalf("迁移后 bob 的队列内容不对: %+v", items)
	}
	if items, _ := s.QueueAfter(ctx, "carol", 0, 100, 0); len(items) != 1 || string(items[0].Payload) != megolm {
		t.Fatalf("迁移后 carol 的队列内容不对: %+v", items)
	}
	var shared, copies, inline int
	s.db.QueryRow("SELECT COUNT(*) FROM queue WHERE shared = 1 AND payload = ''").Scan(&shared)
	s.db.QueryRow("SELECT COUNT(*) FROM envelopes").Scan(&copies)
	s.db.QueryRow("SELECT COUNT(*) FROM queue WHERE shared = 0 AND payload <> ''").Scan(&inline)
	if shared != 2 || copies != 1 || inline != 2 {
		t.Fatalf("群消息正文应搬进 envelopes（2 个引用 / 1 份），其余保持内联: %d / %d / %d", shared, copies, inline)
	}
	var sender sql.NullString
	s.db.QueryRow("SELECT sender_id FROM queue WHERE user_id = 'bob' AND id = 'o1'").Scan(&sender)
	if sender.String != "alice" {
		t.Fatalf("应从信封的 from 回填 sender_id: %v", sender)
	}
	if n, b, _ := s.EnvelopeUsage(ctx, "bob"); n != 2 || b != int64(len(megolm)+len(olm)) {
		t.Fatalf("信封用量回填不对: %d / %d", n, b)
	}
	if n, _, _ := s.QueueUsage(ctx, "bob"); n != 3 {
		t.Fatalf("总用量应保持: %d", n)
	}
	// 共享正文在最后一个引用被删后清除
	s.Ack(ctx, "bob", []string{"g1"})
	s.db.QueryRow("SELECT COUNT(*) FROM envelopes").Scan(&copies)
	if copies != 1 {
		t.Fatal("还有引用时不能删正文")
	}
	s.Ack(ctx, "carol", []string{"g1"})
	s.db.QueryRow("SELECT COUNT(*) FROM envelopes").Scan(&copies)
	if copies != 0 {
		t.Fatal("最后一个引用删除后应清除正文")
	}

	// 新建的库不产生备份
	fresh := t.TempDir()
	s2, err := Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if entries, _ := filepath.Glob(filepath.Join(fresh, "premigrate", "*")); len(entries) != 0 {
		t.Fatalf("新建的库不应有升级前备份: %v", entries)
	}
}

// P3：并发的写操作被合并提交，结果各自独立——失败的只回滚自己，别的照常提交；提交钩子按 seq 顺序。
func TestWriterGroupCommitIsolation(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	uid := newUser(t, s, "a")
	var mu sync.Mutex
	var seqs []int64
	s.SetCommitHook(func(items []QueueInsert) {
		mu.Lock()
		for _, it := range items {
			seqs = append(seqs, it.Seq)
		}
		mu.Unlock()
	})
	const n = 400
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Update(ctx, func(t *Tx) error {
				if _, err := t.Enqueue([]QueueInsert{{UserID: uid, ID: fmt.Sprint("m", i), Kind: KindEnvelope, Payload: []byte("{}")}}, QueueLimits{}); err != nil {
					return err
				}
				if i%4 == 0 {
					return errors.New("业务失败")
				}
				return nil
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if (i%4 == 0) != (err != nil) {
			t.Fatalf("操作 %d 的结果不对: %v", i, err)
		}
	}
	items, err := s.QueueAfter(ctx, uid, 0, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n*3/4 {
		t.Fatalf("应只有成功的 %d 条落库，得到 %d", n*3/4, len(items))
	}
	for _, it := range items {
		var i int
		fmt.Sscanf(it.ID, "m%d", &i)
		if i%4 == 0 {
			t.Fatalf("失败的操作不应落库: %s", it.ID)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seqs) != len(items) {
		t.Fatalf("提交钩子应恰好收到落库的队列项: %d vs %d", len(seqs), len(items))
	}
	for i := range seqs {
		if seqs[i] != items[i].Seq {
			t.Fatalf("提交钩子的顺序应与 seq 顺序一致（第 %d 个：%d vs %d）", i, seqs[i], items[i].Seq)
		}
	}
	if st := s.WriterStats(); st.Batches >= st.Ops {
		t.Logf("提示：本次没有发生合并提交（ops=%d batches=%d）", st.Ops, st.Batches)
	}
}

// 关库之后的写入返回 ErrClosed，已经排队的写入在关库前完成。
func TestWriteAfterCloseReturnsErrClosed(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "a@example.com", "android", "t", "h"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := s.Login(ctx, "b@example.com", "android", "t", "h2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("关库后写入应返回 ErrClosed，得到 %v", err)
	}
}
