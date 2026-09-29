package store

import (
	"context"
	"database/sql"
	"sync"
)

// Tx 是一次业务事务（outbox 模式，接口 v2 S-D）：业务写入与它产生的队列项（事件、信封）经同一个 Tx 写入，
// 一起提交或一起回滚，进程在提交之后崩溃也不会丢事件。提交成功后，本事务写入的队列项交给提交钩子
// （ws.Hub：唤醒在线连接、离线且 urgent 的触发推送）。
//
// Store 只有一个数据库连接：在 Update 的回调里只能通过 Tx 的方法访问数据库，调用 *Store 的方法会死锁。
type Tx struct {
	ctx    context.Context
	tx     *sql.Tx
	queued []QueueInsert // 本事务内已接收的队列项
}

// Context 返回事务所用的 ctx。
func (t *Tx) Context() context.Context { return t.ctx }

// CommitHook 在事务提交成功之后被调用，参数是该事务写入（且已接收）的全部队列项，按写入顺序。
type CommitHook func(items []QueueInsert)

type hookHolder struct {
	mu sync.RWMutex
	fn CommitHook
}

// SetCommitHook 设置提交钩子（ws.NewHub 会注册自己）。nil 表示不通知。
func (s *Store) SetCommitHook(fn CommitHook) {
	s.hook.mu.Lock()
	defer s.hook.mu.Unlock()
	s.hook.fn = fn
}

func (s *Store) commitHook() CommitHook {
	s.hook.mu.RLock()
	defer s.hook.mu.RUnlock()
	return s.hook.fn
}

// Update 在一个事务中执行 fn：fn 返回错误则回滚（业务写入与队列项都不落库，也不通知）；
// 否则提交，提交成功后才把写入的队列项交给提交钩子。
func (s *Store) Update(ctx context.Context, fn func(t *Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &Tx{ctx: ctx, tx: sqlTx}
	if err := fn(t); err != nil {
		sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return err
	}
	if len(t.queued) > 0 {
		if hook := s.commitHook(); hook != nil {
			hook(t.queued)
		}
	}
	return nil
}

// update 在事务中执行单个返回值的 Tx 操作（供 *Store 的同名包装方法使用）。
func update[T any](s *Store, ctx context.Context, fn func(t *Tx) (T, error)) (T, error) {
	var out T
	err := s.Update(ctx, func(t *Tx) error {
		var err error
		out, err = fn(t)
		return err
	})
	return out, err
}
