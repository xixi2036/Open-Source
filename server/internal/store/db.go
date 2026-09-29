// Package store 封装 hush-server 的全部持久化（SQLite, WAL）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动
)

var (
	// ErrNotFound 目标记录不存在。
	ErrNotFound = errors.New("not found")
	// ErrConflict 与已有数据冲突。
	ErrConflict = errors.New("conflict")
)

// Store 是数据访问入口。内部只使用一个连接，所有读写串行化，避免 SQLITE_BUSY。
// 因此实现上绝不在遍历 rows 时发起新的查询，事务内也只使用 tx。
type Store struct {
	db   *sql.DB
	hook hookHolder
}

// Open 打开（必要时创建）dataDir/hush.db 并执行迁移。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "hush.db")
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
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

// withTx 在事务中执行 fn，出错回滚。
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
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

// execer 同时被 *sql.DB 与 *sql.Tx 满足。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
