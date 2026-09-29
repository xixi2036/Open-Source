package api_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hush/server/internal/api"
)

type upResp struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// 接收方队列满：Olm 条目返回 too_large，群消息跳过该成员，其他成员与服务端事件不受影响。
func TestQueueQuota(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.QueueMaxItems = 6 })
	a := e.register("a")
	b := e.register("b")
	c := e.register("c")
	befriend(t, a, b) // b 队列里有 1 个事件（friend_request），a 有 1 个（friend_accepted）
	befriend(t, a, c)

	var sr sendResp
	msgs := []map[string]any{}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, map[string]any{"to": b.ID, "type": 1, "body": "eA==", "client_id": "c"})
	}
	a.must("POST", "/v1/messages", map[string]any{"messages": msgs}, &sr)
	ok, full := 0, 0
	for _, r := range sr.Results {
		switch r.Error {
		case "":
			ok++
		case "too_large":
			full++
		default:
			t.Fatalf("意外错误: %+v", r)
		}
	}
	if ok != 5 || full != 1 {
		t.Fatalf("b 队列上限 6（已有 1 个事件）：应成功 5、拒收 1，得到 %d/%d", ok, full)
	}

	// 群消息：b 已满被跳过，c 正常收到；事件（group_updated）不受配额限制
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID, c.ID}}, &g)
	a.must("POST", "/v1/groups/"+g.ID+"/messages", megolm("to-all"), nil)
	var inbox inboxResp
	c.must("GET", "/v1/messages?limit=500", nil, &inbox)
	if len(inbox.Messages) != 1 {
		t.Fatalf("c 应收到群消息: %+v", inbox.Messages)
	}
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	if len(inbox.Messages) != 5 {
		t.Fatalf("b 不应再收到群消息: %d", len(inbox.Messages))
	}
	gotGroupEvent := false
	for _, ev := range inbox.Events {
		if ev["type"] == "group_updated" {
			gotGroupEvent = true
		}
	}
	if !gotGroupEvent {
		t.Fatal("事件不受队列配额限制")
	}

	// ack 之后腾出空间
	ids := []string{}
	for _, m := range inbox.Messages {
		ids = append(ids, m.ID)
	}
	b.must("POST", "/v1/messages/ack", map[string]any{"ids": ids}, nil)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "room", true), &sr)
	if sr.Results[0].Error != "" {
		t.Fatalf("ack 后应可继续接收: %+v", sr)
	}
}

// 队列字节上限同样生效。
func TestQueueByteQuota(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.QueueMaxBytes = 4096 })
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	var sr sendResp
	body := string(bytes.Repeat([]byte("A"), 3000))
	a.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{
		{"to": b.ID, "type": 1, "body": body, "client_id": "c1"},
		{"to": b.ID, "type": 1, "body": body, "client_id": "c2"},
	}}, &sr)
	if sr.Results[0].Error != "" || sr.Results[1].Error != "too_large" {
		t.Fatalf("超出字节上限应拒收: %+v", sr)
	}
}

// 每用户 24 小时上传总量上限 → 429 rate_limited。
func TestDailyUploadQuota(t *testing.T) {
	e := newEnv(t, true, func(c *api.Config) { c.DailyUploadBytes = 1500 })
	a := e.register("a")
	b := e.register("b")
	a.must("POST", "/v1/attachments", bytes.Repeat([]byte{1}, 1000), nil)
	a.expectErr("POST", "/v1/attachments", bytes.Repeat([]byte{1}, 1000), 429, "rate_limited")
	a.must("POST", "/v1/attachments", bytes.Repeat([]byte{1}, 400), nil)
	b.must("POST", "/v1/attachments", bytes.Repeat([]byte{1}, 1000), nil) // 按用户计
}

// 清理：30 天前的队列项、90 天未访问且未被引用的附件。
func TestCleanup(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	a.must("POST", "/v1/messages", olmMsg(b.ID, "old", true), nil)

	var loose, avatar, groupAvatar, moment, recent upResp
	a.must("POST", "/v1/attachments", []byte("loose"), &loose)
	a.must("POST", "/v1/attachments", []byte("avatar"), &avatar)
	a.must("POST", "/v1/attachments", []byte("gavatar"), &groupAvatar)
	a.must("POST", "/v1/attachments", []byte("moment"), &moment)
	a.must("PUT", "/v1/me/profile", map[string]any{"avatar_id": avatar.ID}, nil)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"avatar_id": groupAvatar.ID}, nil)
	a.must("POST", "/v1/moments", map[string]any{"text": "x", "media": []any{map[string]any{"attachment_id": moment.ID, "width": 1, "height": 1}}}, nil)
	a.must("POST", "/v1/attachments", []byte("recent"), &recent)

	ctx := context.Background()
	// 29 天后：什么都不删
	cs, err := api.Cleanup(ctx, e.st, e.dataDir, time.Now().Add(29*24*time.Hour))
	if err != nil || cs.QueueItems != 0 || cs.Attachments != 0 {
		t.Fatalf("未到期不应清理: %+v %v", cs, err)
	}
	// 31 天后：队列项过期
	cs, _ = api.Cleanup(ctx, e.st, e.dataDir, time.Now().Add(31*24*time.Hour))
	if cs.QueueItems == 0 {
		t.Fatalf("30 天前的队列项应删除: %+v", cs)
	}
	var inbox inboxResp
	b.must("GET", "/v1/messages", nil, &inbox)
	if len(inbox.Messages) != 0 || len(inbox.Events) != 0 {
		t.Fatalf("过期队列项应已删除: %+v", inbox)
	}

	// 91 天后：只有未被引用的附件被删除
	cs, _ = api.Cleanup(ctx, e.st, e.dataDir, time.Now().Add(91*24*time.Hour))
	if cs.Attachments != 2 {
		t.Fatalf("应删除 2 个未引用附件（loose、recent），得到 %d", cs.Attachments)
	}
	for _, id := range []string{loose.ID, recent.ID} {
		if _, err := os.Stat(filepath.Join(e.dataDir, "attachments", id)); !os.IsNotExist(err) {
			t.Fatalf("附件文件 %s 应被删除", id)
		}
		a.expectErr("GET", "/v1/attachments/"+id, nil, 404, "not_found")
	}
	for _, id := range []string{avatar.ID, groupAvatar.ID, moment.ID} {
		if st, _ := a.call("GET", "/v1/attachments/"+id, nil, nil); st != 200 {
			t.Fatalf("被引用的附件 %s 不应删除", id)
		}
	}
}

// 下载会刷新访问时间（超过 1 天才写）。
func TestAttachmentAccessRefresh(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	var up upResp
	a.must("POST", "/v1/attachments", []byte("data"), &up)
	if err := e.st.TouchAttachment(context.Background(), up.ID, time.Now().Add(100*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// 访问时间被推到 100 天后：190 天之前都不应清理
	cs, _ := api.Cleanup(context.Background(), e.st, e.dataDir, time.Now().Add(150*24*time.Hour))
	if cs.Attachments != 0 {
		t.Fatalf("最近访问过的附件不应清理: %+v", cs)
	}
}

// 全局读写超时很短时，WebSocket 长连接与慢速大文件上传仍然可用。
func TestTimeoutsRelaxedForWSAndBlobs(t *testing.T) {
	to := api.Timeouts{ReadHeader: 2 * time.Second, Read: 300 * time.Millisecond, Write: 300 * time.Millisecond, Idle: time.Second}
	e := newEnvTimeouts(t, true, &to)
	a := e.register("a")

	w := a.dial()
	w.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	time.Sleep(800 * time.Millisecond)
	w.send(map[string]any{"t": "ping"})
	w.until("pong（超过全局超时后）", func(f frame) bool { return f.T == "pong" })

	pr, pw := io.Pipe()
	go func() {
		pw.Write(bytes.Repeat([]byte{7}, 100))
		time.Sleep(700 * time.Millisecond)
		pw.Write(bytes.Repeat([]byte{7}, 100))
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/attachments", pr)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("慢速上传失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("慢速上传应成功: %d %s", resp.StatusCode, body)
	}
}

// 附件文件无法移动（例如权限问题）时保留元数据，下次再试，而不是先删元数据让文件永远成为孤儿（审查 S10）。
func TestCleanupKeepsMetadataWhenFileStuck(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	var up upResp
	a.must("POST", "/v1/attachments", []byte("stuck"), &up)
	dir := filepath.Join(e.dataDir, "attachments")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	later := time.Now().Add(91 * 24 * time.Hour)
	cs, err := api.Cleanup(context.Background(), e.st, e.dataDir, later)
	if err != nil || cs.Attachments != 0 {
		t.Fatalf("文件无法移动时不应删除: %+v %v", cs, err)
	}
	if st, _ := a.call("GET", "/v1/attachments/"+up.ID, nil, nil); st != 200 {
		t.Fatalf("元数据与文件都应保留: %d", st)
	}
	os.Chmod(dir, 0o700)
	cs, err = api.Cleanup(context.Background(), e.st, e.dataDir, later)
	if err != nil || cs.Attachments != 1 {
		t.Fatalf("恢复后应正常清理: %+v %v", cs, err)
	}
	if _, err := os.Stat(filepath.Join(dir, up.ID)); !os.IsNotExist(err) {
		t.Fatal("文件应已删除")
	}
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		t.Fatalf("附件目录应为空，残留 %s", en.Name())
	}
}

// 进程崩溃残留的上传临时文件与 .trash- 文件过期后被清理，新的不动。
func TestCleanupSweepsTempFiles(t *testing.T) {
	e := newEnv(t, true)
	dir := filepath.Join(e.dataDir, "attachments")
	os.MkdirAll(dir, 0o700)
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{".upload-old", ".trash-old"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o600)
		os.Chtimes(p, old, old)
	}
	os.WriteFile(filepath.Join(dir, ".upload-fresh"), []byte("x"), 0o600)
	cs, err := api.Cleanup(context.Background(), e.st, e.dataDir, time.Now())
	if err != nil || cs.TempFiles != 2 {
		t.Fatalf("应删除 2 个过期临时文件: %+v %v", cs, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".upload-fresh")); err != nil {
		t.Fatal("进行中的上传临时文件不应删除")
	}
}
