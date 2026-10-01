// Package store 封装 hush-server 的全部持久化（SQLite, WAL）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动
)

var (
	// ErrNotFound 目标记录不存在。
	ErrNotFound = errors.New("not found")
	// ErrConflict 与已有数据冲突。
	ErrConflict = errors.New("conflict")
	// ErrClosed 数据库已关闭（停机过程中）。
	ErrClosed = errors.New("store closed")
	// ErrNewerSchema 数据库是更新版本的程序写的，本程序不认识它的结构。
	ErrNewerSchema = errors.New("database schema is newer than this binary")
)

// Options 是打开数据库的可选参数。零值即默认。
type Options struct {
	// ReadConns 是只读连接池的大小，默认 min(max(4, CPU 数), 16)。
	ReadConns int
	// SyncFull 为 true 时写连接用 synchronous=FULL（每次提交都落盘，掉电不丢已提交的数据，写入变慢）；
	// 默认 NORMAL：进程崩溃不丢数据，但掉电 / 内核崩溃可能丢最近几次提交。
	SyncFull bool
	// NoMigrationBackup 为 true 时升级数据库结构之前不做备份（测试用）。
	NoMigrationBackup bool
}

// Store 是数据访问入口。
//
// 读写分离：一条写连接由唯一的写协程持有（见 writer），全部写事务在它上面串行执行并按批提交——
// 因此「提交顺序 = 队列 seq 顺序」，也不会出现 SQLITE_BUSY；只读查询走 rd（WAL 下可与写并发，
// 连接带 query_only）。读连接在事务提交之后才看得到它的写入：同一个请求里「先写后读」没有问题，
// 但写事务的回调里要读自己刚写的数据必须用 Tx 的方法。
type Store struct {
	db             *router // 读走只读池，单条写语句经写协程
	rd             *readDB
	wr             *sql.DB
	w              *writer
	closeOnce      sync.Once
	closeErr       error
	ck             *sql.DB // 后台检查点专用连接
	ckStop, ckDone chan struct{}
	path           string
	hook           hookHolder
	auth           authCache
	rel            relationCache
}

// Open 以默认参数打开（必要时创建）dataDir/hush.db 并执行迁移。
func Open(dataDir string) (*Store, error) { return OpenWith(dataDir, Options{}) }

// OpenWith 打开（必要时创建）dataDir/hush.db 并执行迁移。
//
// 数据库版本高于本程序已知的迁移时返回 ErrNewerSchema（旧程序不能打开新库）；需要升级已有的库时，
// 先用 VACUUM INTO 在 dataDir/premigrate/ 下留一份升级前的完整副本。
func OpenWith(dataDir string, opt Options) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "hush.db")
	sync := "NORMAL"
	if opt.SyncFull {
		sync = "FULL"
	}
	common := "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=temp_store(MEMORY)"
	wr, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous("+sync+")&"+common+
		"&_pragma=cache_size(-32768)&_pragma=journal_size_limit(67108864)&_pragma=wal_autocheckpoint(16384)")
	if err != nil {
		return nil, err
	}
	wr.SetMaxOpenConns(1)
	wr.SetConnMaxIdleTime(0)
	if err := migrate(context.Background(), wr, dataDir, opt); err != nil {
		wr.Close()
		if errors.Is(err, ErrNewerSchema) {
			return nil, err
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	n := opt.ReadConns
	if n <= 0 {
		n = min(max(4, runtime.NumCPU()), 16)
	}
	rd, err := sql.Open("sqlite", "file:"+path+"?"+common+"&_pragma=query_only(1)&_pragma=cache_size(-8192)")
	if err != nil {
		wr.Close()
		return nil, err
	}
	rd.SetMaxOpenConns(n)
	rd.SetMaxIdleConns(n)
	rd.SetConnMaxIdleTime(0)
	s := &Store{rd: &readDB{DB: rd}, wr: wr, path: path}
	s.auth.init()
	w, err := newWriter(s, wr)
	if err != nil {
		rd.Close()
		wr.Close()
		return nil, err
	}
	s.w = w
	s.db = &router{s: s}
	if ck, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1000)"); err == nil {
		ck.SetMaxOpenConns(1)
		s.ck, s.ckStop, s.ckDone = ck, make(chan struct{}), make(chan struct{})
		go s.checkpointLoop()
	}
	return s, nil
}

// checkpointInterval 是后台检查点的间隔。
const checkpointInterval = 200 * time.Millisecond

// checkpointLoop 在独立连接上定时做被动检查点（PASSIVE：不阻塞读写），把 WAL 里已提交的页搬回主库并落盘。
// 写连接自己的自动检查点阈值设得很高（16384 页），轮到它时大部分页已经搬完，只剩最近一小段——
// 这样检查点的磁盘 I/O 基本不占写协程的时间，写入不会每隔几十次提交就停顿一下。
func (s *Store) checkpointLoop() {
	defer close(s.ckDone)
	tick := time.NewTicker(checkpointInterval)
	defer tick.Stop()
	var lastWrites int64 = -1
	for {
		select {
		case <-s.ckStop:
			return
		case <-tick.C:
		}
		if n := s.w.batches.Load(); n != lastWrites { // 没有新提交就不必做
			lastWrites = n
			var busy, logFrames, done int
			s.ck.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &logFrames, &done)
		}
	}
}

// Close 等写协程处理完已经排队的写入后关闭数据库。之后的写入返回 ErrClosed。
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.ck != nil {
			close(s.ckStop)
			<-s.ckDone
			s.ck.Close()
		}
		s.w.close()
		s.rd.closeStmts()
		s.closeErr = s.rd.Close()
		if werr := s.wr.Close(); s.closeErr == nil {
			s.closeErr = werr
		}
	})
	return s.closeErr
}

// Ping 做一次最小的读查询，确认数据库可用（/readyz）。
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.rd.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// SchemaVersion 返回本程序的数据库结构版本（迁移条数）。
func SchemaVersion() int { return len(migrations) }

const premigrateKeep = 3 // 升级前备份保留最近几份

func migrate(ctx context.Context, db *sql.DB, dataDir string, opt Options) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("%w：库版本 %d，本程序最高支持 %d——请使用更新的 hush-server，不要用旧程序打开新库", ErrNewerSchema, version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}
	if version > 0 && !opt.NoMigrationBackup {
		// 升级已有的库之前留一份完整副本。它含离线队列里的信封，所以不放进 backups/（后台可以下载那里的文件）。
		dir := filepath.Join(dataDir, "premigrate")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("升级前备份: %w", err)
		}
		name := fmt.Sprintf("hush-v%d-%s.db", version, time.Now().Format("20060102-150405"))
		dst := filepath.Join(dir, name)
		os.Remove(dst)
		if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
			return fmt.Errorf("升级前备份: %w", err)
		}
		os.Chmod(dst, 0o600)
		log.Printf("[store] 数据库将从版本 %d 升级到 %d，升级前的完整副本保存在 %s", version, len(migrations), dst)
		prunePremigrate(dir)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func prunePremigrate(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hush-v") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	// 按修改时间从新到旧
	sort.Slice(names, func(i, j int) bool {
		a, _ := os.Stat(filepath.Join(dir, names[i]))
		b, _ := os.Stat(filepath.Join(dir, names[j]))
		if a == nil || b == nil {
			return names[i] > names[j]
		}
		return a.ModTime().After(b.ModTime())
	})
	for i := premigrateKeep; i < len(names); i++ {
		os.Remove(filepath.Join(dir, names[i]))
	}
}

// readDB 是只读连接池。查询语句预编译后缓存（database/sql 的 Stmt 会在池里的每条连接上各编译一次并复用）：
// 热路径上的查询不必每次都重新解析 SQL——解析既费 CPU，又要在 SQLite 的内存分配器上争锁。
type readDB struct {
	*sql.DB
	stmts sync.Map // SQL → *sql.Stmt
	n     atomic.Int64
}

const readStmtCacheMax = 1024 // 带可变个数占位符的查询会产生很多不同的 SQL：缓存满了之后的就不预编译

func (r *readDB) stmt(ctx context.Context, q string) *sql.Stmt {
	if v, ok := r.stmts.Load(q); ok {
		return v.(*sql.Stmt)
	}
	if r.n.Load() >= readStmtCacheMax {
		return nil
	}
	st, err := r.DB.PrepareContext(ctx, q)
	if err != nil {
		return nil // 让调用方走非预编译路径，错误从那里报出来
	}
	if v, loaded := r.stmts.LoadOrStore(q, st); loaded {
		st.Close()
		return v.(*sql.Stmt)
	}
	r.n.Add(1)
	return st
}

func (r *readDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if st := r.stmt(ctx, q); st != nil {
		return st.QueryContext(ctx, args...)
	}
	return r.DB.QueryContext(ctx, q, args...)
}

func (r *readDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if st := r.stmt(ctx, q); st != nil {
		return st.QueryRowContext(ctx, args...)
	}
	return r.DB.QueryRowContext(ctx, q, args...)
}

// ExecContext 在只读连接上必然失败（query_only）；实现它只是为了满足 execer。
func (r *readDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return r.DB.ExecContext(ctx, q, args...)
}

func (r *readDB) closeStmts() {
	r.stmts.Range(func(k, v any) bool {
		v.(*sql.Stmt).Close()
		r.stmts.Delete(k)
		return true
	})
}

// withTx 在写事务中执行 fn，出错回滚。
func (s *Store) withTx(ctx context.Context, fn func(tx dbtx) error) error {
	return s.w.do(ctx, func(t *Tx) error { return fn(t.tx) })
}

// NowMillis 返回当前 Unix 毫秒。
func NowMillis() int64 { return time.Now().UnixMilli() }

// NewID 生成 UUIDv4 字符串。
func NewID() string { return uuid.NewString() }

// placeholders 生成 "?,?,?"。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// execer 同时被只读池、写事务满足。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbtx 是写事务里可用的语句接口。
type dbtx = execer

// router 是 *Store 方法里用的 s.db：查询走只读池；单条写语句交给写协程，作为一个独立的小事务执行。
// 注意：带 RETURNING 的写语句要用 withTx（这里的 Query* 只读）。
type router struct{ s *Store }

func (r *router) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := r.s.w.do(ctx, func(t *Tx) error {
		var err error
		res, err = t.tx.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

func (r *router) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return r.s.rd.QueryContext(ctx, query, args...)
}

func (r *router) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.s.rd.QueryRowContext(ctx, query, args...)
}

// QueryRow / Exec 是不带 ctx 的便捷形式（测试用）。
func (r *router) QueryRow(query string, args ...any) *sql.Row {
	return r.s.rd.QueryRowContext(context.Background(), query, args...)
}

func (r *router) Query(query string, args ...any) (*sql.Rows, error) {
	return r.s.rd.QueryContext(context.Background(), query, args...)
}

func (r *router) Exec(query string, args ...any) (sql.Result, error) {
	return r.ExecContext(context.Background(), query, args...)
}

// exists 判断查询是否至少返回一行（q 应是 "SELECT 1 FROM ... WHERE ..."）。
func exists(ctx context.Context, ex execer, q string, args ...any) (bool, error) {
	var one int
	err := ex.QueryRowContext(ctx, q+" LIMIT 1", args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// nullIfEmpty 把空串映射为 SQL NULL（例如清除头像）。
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// prefixed 给逗号分隔的列名加表别名前缀，如 prefixed("u", "id, name") = "u.id, u.name"。
func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}
