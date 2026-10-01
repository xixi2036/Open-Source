package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tx 是一次业务事务（outbox 模式，接口 v2 S-D）：业务写入与它产生的队列项（事件、信封）经同一个 Tx 写入，
// 一起提交或一起回滚，进程在提交之后崩溃也不会丢事件。提交成功后，本事务写入的队列项交给提交钩子
// （ws.Hub：直接交给在线连接、离线且 urgent 的触发推送）。
//
// 写事务在唯一的写协程里执行：回调里只能通过 Tx 的方法访问数据库。调用 *Store 的读方法看不到本事务尚未提交的写入，
// 调用 *Store 的写方法会死锁（写协程正忙着执行这个回调）。
type Tx struct {
	s      *Store
	ctx    context.Context
	tx     dbtx
	queued []QueueInsert // 本事务内新写入的队列项（带 Seq）
	after  []func()      // 提交成功后按登记顺序执行
}

// Context 返回事务所用的 ctx。
func (t *Tx) Context() context.Context { return t.ctx }

// OnCommit 登记一个提交成功之后执行的回调（事务回滚则不执行）。回调在写协程里、紧跟着本事务的提交钩子执行，
// 早于之后任何事务的提交钩子——例如重新登录在这里踢掉旧连接，之后入队的信封就不会再交给旧设备。
// 回调必须很快返回、不能访问数据库。
func (t *Tx) OnCommit(fn func()) { t.after = append(t.after, fn) }

// CommitHook 在事务提交成功之后被调用，参数是该事务新写入的全部队列项，按 seq 顺序。
// 钩子在写协程里按提交顺序依次调用，必须很快返回、不能访问数据库。
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
// 否则提交，提交成功后才把写入的队列项交给提交钩子。Update 在钩子执行完之后才返回。
func (s *Store) Update(ctx context.Context, fn func(t *Tx) error) error {
	return s.w.do(ctx, fn)
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

// ---------- 写协程（组提交） ----------

const (
	writeBatchMax   = 256 // 一次提交最多合并多少个写操作
	writeQueueDepth = 4096
	stmtCacheMax    = 256
)

type writeOp struct {
	ctx      context.Context
	fn       func(t *Tx) error
	enqueued time.Time
	done     chan struct{}
	err      error
	panicked any // fn 或提交钩子里的 panic，转交给调用方协程重新抛出
	t        *Tx
}

// writer 持有唯一的写连接。调用方把写操作（一个回调）交给它；它把此刻已经在排队的操作合并进一个数据库事务
// （每个操作一个 SAVEPOINT，失败的只回滚自己），一次提交，再按顺序执行各操作的提交钩子。
// 负载低时每个操作单独提交、没有额外等待；负载高时自然成批，事务数远少于操作数。
type writer struct {
	s    *Store
	conn *sql.Conn
	tx   *txConn
	ch   chan *writeOp
	quit chan struct{}
	dead chan struct{}
	once sync.Once

	// 统计（管理后台「数据库写队列等待」）
	ops         atomic.Int64
	batches     atomic.Int64
	waitNanos   atomic.Int64
	workNanos   atomic.Int64 // 执行各操作回调的时间
	commitNanos atomic.Int64 // COMMIT 的时间
	hookNanos   atomic.Int64 // 提交钩子与回调的时间
}

func newWriter(s *Store, db *sql.DB) (*writer, error) {
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	w := &writer{s: s, conn: conn, tx: &txConn{c: conn, stmts: map[string]*sql.Stmt{}, relTouch: map[string]bool{}},
		ch: make(chan *writeOp, writeQueueDepth), quit: make(chan struct{}), dead: make(chan struct{})}
	go w.loop()
	return w, nil
}

// do 提交一个写操作并等待结果。
func (w *writer) do(ctx context.Context, fn func(t *Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	op := &writeOp{ctx: ctx, fn: fn, enqueued: time.Now(), done: make(chan struct{})}
	select {
	case <-w.quit:
		return ErrClosed
	default:
	}
	select {
	case w.ch <- op:
	case <-w.quit:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-op.done:
	case <-w.dead:
		// 写协程已退出：操作要么已经完成（done 已关闭），要么没被执行
		select {
		case <-op.done:
		default:
			return ErrClosed
		}
	}
	if op.panicked != nil {
		panic(op.panicked)
	}
	return op.err
}

func (w *writer) close() {
	w.once.Do(func() { close(w.quit) })
	<-w.dead
}

func (w *writer) loop() {
	defer close(w.dead)
	defer func() {
		w.tx.closeStmts()
		w.conn.Close()
	}()
	batch := make([]*writeOp, 0, writeBatchMax)
	for {
		var first *writeOp
		select {
		case first = <-w.ch:
		case <-w.quit:
			// 停机：把已经排队的写完再退出
			select {
			case first = <-w.ch:
			default:
				return
			}
		}
		batch = append(batch[:0], first)
	fill:
		for len(batch) < writeBatchMax {
			select {
			case op := <-w.ch:
				batch = append(batch, op)
			default:
				break fill
			}
		}
		w.run(batch)
	}
}

var bg = context.Background()

func (w *writer) exec(q string) error {
	_, err := w.conn.ExecContext(bg, q)
	return err
}

// call 执行一个操作的回调，把 panic 转成返回值。
func (w *writer) call(op *writeOp) (err error) {
	defer func() {
		if v := recover(); v != nil {
			op.panicked = v
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return op.fn(op.t)
}

// run 在一个数据库事务里执行一批操作。
func (w *writer) run(batch []*writeOp) {
	now := time.Now()
	w.batches.Add(1)
	w.ops.Add(int64(len(batch)))
	w.tx.relDirty = false
	for _, op := range batch {
		w.waitNanos.Add(int64(now.Sub(op.enqueued)))
	}
	finish := func() {
		for _, op := range batch {
			close(op.done)
		}
	}
	if err := w.exec("BEGIN IMMEDIATE"); err != nil {
		for _, op := range batch {
			op.err = err
		}
		finish()
		return
	}
	single := len(batch) == 1
	committed := 0
	t0 := time.Now()
	aborted := false
	for _, op := range batch {
		if err := op.ctx.Err(); err != nil { // 调用方已经放弃（客户端断开）：不执行
			op.err = err
			continue
		}
		if !single {
			if err := w.exec("SAVEPOINT op"); err != nil {
				op.err = err
				aborted = true
				break
			}
		}
		op.t = &Tx{s: w.s, ctx: op.ctx, tx: w.tx}
		if err := w.call(op); err != nil {
			op.err = err
			op.t = nil
			if single {
				break
			}
			if rerr := w.exec("ROLLBACK TO op"); rerr != nil {
				// 事务已被 SQLite 自动回滚（磁盘满、I/O 错误等）：这一批之前的操作也都没了
				aborted = true
				break
			}
			if rerr := w.exec("RELEASE op"); rerr != nil {
				aborted = true
				break
			}
			continue
		}
		if !single {
			if err := w.exec("RELEASE op"); err != nil {
				op.err = err
				op.t = nil
				aborted = true
				break
			}
		}
		committed++
	}
	t1 := time.Now()
	w.workNanos.Add(int64(t1.Sub(t0)))
	var commitErr error
	switch {
	case aborted:
		commitErr = errors.New("写事务被中止，本批写入全部回滚")
		w.exec("ROLLBACK")
	case committed == 0:
		w.exec("ROLLBACK")
	default:
		if commitErr = w.exec("COMMIT"); commitErr != nil {
			w.exec("ROLLBACK")
		}
	}
	if commitErr != nil {
		for _, op := range batch {
			if op.err == nil {
				op.err = commitErr
			}
			op.t = nil
		}
		finish()
		return
	}
	t2 := time.Now()
	w.commitNanos.Add(int64(t2.Sub(t1)))
	if w.tx.relDirty {
		w.s.rel.invalidate()
	}
	defer func() { w.hookNanos.Add(int64(time.Since(t2))) }()
	// 已提交：按操作顺序执行提交钩子与 OnCommit 回调，然后才唤醒调用方
	hook := w.s.commitHook()
	for _, op := range batch {
		if op.t == nil {
			continue
		}
		w.afterCommit(op, hook)
	}
	finish()
}

func (w *writer) afterCommit(op *writeOp, hook CommitHook) {
	defer func() {
		if v := recover(); v != nil {
			op.panicked = v
		}
	}()
	t := op.t
	if len(t.queued) > 0 && hook != nil {
		sort.SliceStable(t.queued, func(i, j int) bool { return t.queued[i].Seq < t.queued[j].Seq })
		hook(t.queued)
	}
	for _, fn := range t.after {
		fn()
	}
}

// txConn 是写连接上的语句执行器。语句一律用不可取消的 ctx 执行：调用方的 ctx 被取消时，
// database/sql 会中断连接上正在执行的语句，而 SQLite 在事务里被中断会回滚整个事务——那会连累同一批里的其他操作。
// 写操作都很短，执行前已经检查过 ctx，不需要中途取消。常用语句在连接上预编译并缓存。
type txConn struct {
	c     *sql.Conn
	stmts map[string]*sql.Stmt
	// relTouch 记住每条语句是否改动「谁能给谁发消息」所依据的表；relDirty 表示当前这批操作里执行过这样的语句，
	// 提交后据此让关系缓存失效（见 relationCache）。按语句文本判断，覆盖全部写入路径（含管理后台与级联删除）。
	relTouch map[string]bool
	relDirty bool
}

// touchesRelations 判断一条写语句是否可能改变用户之间的关系（好友、拉黑、群成员、用户 / 群的删除）。
func touchesRelations(q string) bool {
	for _, table := range []string{"friendships", "blocks", "group_members", "FROM groups", "FROM users"} {
		if strings.Contains(q, table) {
			u := strings.ToUpper(strings.TrimSpace(q))
			return strings.HasPrefix(u, "INSERT") || strings.HasPrefix(u, "DELETE") || strings.HasPrefix(u, "UPDATE")
		}
	}
	return false
}

func (t *txConn) stmt(q string) (*sql.Stmt, error) {
	if st := t.stmts[q]; st != nil {
		if t.relTouch[q] {
			t.relDirty = true
		}
		return st, nil
	}
	st, err := t.c.PrepareContext(bg, q)
	if err != nil {
		return nil, err
	}
	if touchesRelations(q) {
		t.relTouch[q] = true
		t.relDirty = true
	}
	if len(t.stmts) >= stmtCacheMax {
		// 缓存满了：随便淘汰一条（语句种类有限，正常情况下到不了这里）
		for k, old := range t.stmts {
			old.Close()
			delete(t.stmts, k)
			delete(t.relTouch, k)
			break
		}
	}
	t.stmts[q] = st
	return st, nil
}

func (t *txConn) closeStmts() {
	for k, st := range t.stmts {
		st.Close()
		delete(t.stmts, k)
	}
}

func (t *txConn) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	st, err := t.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.ExecContext(bg, args...)
}

func (t *txConn) QueryContext(_ context.Context, q string, args ...any) (*sql.Rows, error) {
	st, err := t.stmt(q)
	if err != nil {
		return nil, err
	}
	return st.QueryContext(bg, args...)
}

func (t *txConn) QueryRowContext(_ context.Context, q string, args ...any) *sql.Row {
	st, err := t.stmt(q)
	if err != nil {
		// *sql.Row 无法直接构造错误：退回到连接上直接执行，让同样的错误从 Scan 里出来
		return t.c.QueryRowContext(bg, q, args...)
	}
	return st.QueryRowContext(bg, args...)
}

// WriterStats 是写协程的累计统计。
type WriterStats struct {
	Ops     int64         // 写操作总数
	Batches int64         // 提交次数（Ops / Batches = 平均每次提交合并的操作数）
	Wait    time.Duration // 写操作排队等待的累计时间
	Work    time.Duration // 执行写操作本身的累计时间
	Commit  time.Duration // COMMIT（写 WAL）的累计时间
	Hooks   time.Duration // 提交钩子（交给在线连接）的累计时间
	Queued  int           // 此刻排队中的写操作数
}

// WriterStats 返回写协程的累计统计。
func (s *Store) WriterStats() WriterStats {
	return WriterStats{Ops: s.w.ops.Load(), Batches: s.w.batches.Load(), Wait: time.Duration(s.w.waitNanos.Load()),
		Work: time.Duration(s.w.workNanos.Load()), Commit: time.Duration(s.w.commitNanos.Load()), Hooks: time.Duration(s.w.hookNanos.Load()),
		Queued: len(s.w.ch)}
}
