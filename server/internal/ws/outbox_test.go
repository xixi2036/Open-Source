package ws

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hush/server/internal/push"
	"hush/server/internal/store"
)

// updateProfileWithEvent 是一次典型的业务写入：改昵称，并在同一事务里给 to 写 profile_updated。
func updateProfileWithEvent(ctx context.Context, st *store.Store, uid, to, name string, fail bool) error {
	return st.Update(ctx, func(t *store.Tx) error {
		p, err := t.UpdateProfile(uid, store.ProfileUpdate{Name: &name})
		if err != nil {
			return err
		}
		if err := Emit(t, []string{to}, "profile_updated", "profile_updated:"+uid, map[string]any{"profile": p}); err != nil {
			return err
		}
		if fail {
			return errors.New("业务后续步骤失败")
		}
		return nil
	})
}

// S-D：业务提交之后、推送之前进程崩溃（提交钩子没能执行），事件仍在队列里；重启后客户端作为积压收到。
func TestOutboxCrashAfterCommitBeforePush(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{st: st, hub: NewHub(st, push.LogPusher{})}
	a := e.login(t, "a@example.com", "ha")
	b := e.login(t, "b@example.com", "hb")
	srv := e.server(t)
	online := dialWS(t, srv, b, "hb")
	readUntil(t, online, "queue_empty")

	// 模拟崩溃：事务一提交，进程就在通知在线连接之前挂掉。
	st.SetCommitHook(func([]store.QueueInsert) { panic("crash after commit") })
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("应在提交钩子处崩溃")
			}
		}()
		updateProfileWithEvent(ctx, st, a, b, "A-new", false)
	}()
	// 在线连接没有被唤醒（推送没有发生）……
	rctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	if _, data, err := online.Read(rctx); err == nil {
		t.Fatalf("崩溃时推送不应发生，却收到 %s", data)
	}
	cancel()
	srv.CloseClientConnections()
	srv.Close()
	st.Close()

	// ……但重启后业务写入与事件都在：事件与业务写入同一事务提交。
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	u, err := st2.GetUser(ctx, a)
	if err != nil || u.Name != "A-new" {
		t.Fatalf("业务写入应已提交: %+v %v", u, err)
	}
	e2 := &testEnv{st: st2, hub: NewHub(st2, push.LogPusher{})}
	if ids := e2.queueIDs(t, b); len(ids) != 1 {
		t.Fatalf("重启后事件应仍在接收方队列: %v", ids)
	}
	c := dialWS(t, e2.server(t), b, "hb")
	// 积压（事件）先于 queue_empty 推送
	if f, code := readFrame(t, c); code != 0 || f.T != "event" {
		t.Fatalf("应作为积压先收到事件: %+v code=%d", f, code)
	}
	readUntil(t, c, "queue_empty")
}

// S-D：事务回滚时业务写入与事件都不落库，提交钩子也不会被调用。
func TestOutboxRollback(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	a := e.login(t, "a@example.com", "ha")
	b := e.login(t, "b@example.com", "hb")
	called := false
	e.st.SetCommitHook(func([]store.QueueInsert) { called = true })
	if err := updateProfileWithEvent(ctx, e.st, a, b, "A-new", true); err == nil {
		t.Fatal("应返回错误")
	}
	if u, _ := e.st.GetUser(ctx, a); u.Name == "A-new" {
		t.Fatal("回滚后业务写入不应生效")
	}
	if ids := e.queueIDs(t, b); len(ids) != 0 {
		t.Fatalf("回滚后事件不应入队: %v", ids)
	}
	if called {
		t.Fatal("回滚时不应调用提交钩子")
	}
}

// S-D：在线推送在提交之后触发——钩子被调用时事件已经提交、可以从队列读到；在线连接随即实时收到。
func TestOutboxNotifyAfterCommit(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	a := e.login(t, "a@example.com", "ha")
	b := e.login(t, "b@example.com", "hb")
	srv := e.server(t)
	c := dialWS(t, srv, b, "hb")
	readUntil(t, c, "queue_empty")

	var mu sync.Mutex
	var visibleAtHook int
	orig := e.hub.onCommit
	e.st.SetCommitHook(func(items []store.QueueInsert) {
		// 若在提交前调用，这里的读取会因唯一连接被事务占用而死锁
		got, err := e.st.QueueAfter(ctx, b, 0, 100, 0)
		mu.Lock()
		if err == nil {
			visibleAtHook = len(got)
		}
		mu.Unlock()
		orig(items)
	})
	if err := updateProfileWithEvent(ctx, e.st, a, b, "A-new", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := visibleAtHook
	mu.Unlock()
	if n != 1 {
		t.Fatalf("钩子调用时事件应已提交可见，得到 %d 条", n)
	}
	readUntil(t, c, "event")
}
