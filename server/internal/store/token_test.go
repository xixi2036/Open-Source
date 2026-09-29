package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func tokenLastUsed(t *testing.T, s *Store, hash string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow("SELECT last_used_at FROM tokens WHERE token_hash = ?", hash).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// 令牌滑动过期（接口 v2 S-C）：过期失效、刷新节流、ttl=0 永不过期。
func TestTokenSlidingExpiry(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	res, err := s.Login(ctx, "a@example.com", "android", "t", "h1")
	if err != nil {
		t.Fatal(err)
	}
	uid := res.User.ID
	t0 := tokenLastUsed(t, s, "h1")
	if t0 == 0 {
		t.Fatal("登录时应写入 last_used_at")
	}
	const ttl = 90 * 24 * time.Hour
	hour := time.Hour.Milliseconds()

	// 30 分钟后使用：有效，但不写库（节流）
	if got, err := s.Authenticate(ctx, "h1", t0+hour/2, ttl); err != nil || got != uid {
		t.Fatalf("应有效: %v %v", got, err)
	}
	if v := tokenLastUsed(t, s, "h1"); v != t0 {
		t.Fatalf("1 小时内不应刷新 last_used_at: %d != %d", v, t0)
	}
	// 2 小时后使用：刷新
	if _, err := s.Authenticate(ctx, "h1", t0+2*hour, ttl); err != nil {
		t.Fatal(err)
	}
	if v := tokenLastUsed(t, s, "h1"); v != t0+2*hour {
		t.Fatalf("超过 1 小时应刷新 last_used_at: %d", v)
	}
	// 从最近一次使用算起 ttl 内有效（滑动）：距登录已超过 ttl，但距刷新未超过
	if _, err := s.Authenticate(ctx, "h1", t0+ttl.Milliseconds()+hour, ttl); err != nil {
		t.Fatalf("滑动窗口内应有效: %v", err)
	}
	last := tokenLastUsed(t, s, "h1")
	// 超过 ttl 未使用：失效并删除
	if _, err := s.Authenticate(ctx, "h1", last+ttl.Milliseconds()+1, ttl); err != ErrNotFound {
		t.Fatalf("过期应返回 ErrNotFound，得到 %v", err)
	}
	if ok, _ := s.TokenValid(ctx, uid, "h1"); ok {
		t.Fatal("过期令牌应被删除")
	}
	if _, err := s.Authenticate(ctx, "h1", last, ttl); err != ErrNotFound {
		t.Fatalf("已删除的令牌不能再用: %v", err)
	}

	// ttl=0：永不过期
	if _, err := s.Login(ctx, "a@example.com", "android", "t", "h2"); err != nil {
		t.Fatal(err)
	}
	t2 := tokenLastUsed(t, s, "h2")
	if got, err := s.Authenticate(ctx, "h2", t2+10*ttl.Milliseconds(), 0); err != nil || got != uid {
		t.Fatalf("ttl=0 应永不过期: %v %v", got, err)
	}
	// 未知令牌
	if _, err := s.Authenticate(ctx, "nope", t2, ttl); err != ErrNotFound {
		t.Fatalf("未知令牌: %v", err)
	}
}

// openAtVersion 创建只执行了前 n 个迁移的数据库（模拟旧版本服务端留下的库）。
func openAtVersion(t *testing.T, dir string, n int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "hush.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(n)); err != nil {
		t.Fatal(err)
	}
	return db
}

// v4 迁移：已有令牌的 last_used_at 取迁移时刻，升级后仍然有效。
func TestMigrationTokenLastUsed(t *testing.T) {
	dir := t.TempDir()
	db := openAtVersion(t, dir, 3)
	if _, err := db.Exec(`INSERT INTO users (id, email, created_at, updated_at) VALUES ('u1', 'a@example.com', 1, 1);
		INSERT INTO tokens (token_hash, user_id, created_at) VALUES ('old', 'u1', 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := NowMillis()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v := tokenLastUsed(t, s, "old")
	if v < before-1000 || v > NowMillis()+1000 {
		t.Fatalf("迁移后 last_used_at 应为迁移时刻，得到 %d（迁移前 %d）", v, before)
	}
	if uid, err := s.Authenticate(context.Background(), "old", NowMillis(), 90*24*time.Hour); err != nil || uid != "u1" {
		t.Fatalf("迁移后旧令牌应仍有效: %v %v", uid, err)
	}
}
