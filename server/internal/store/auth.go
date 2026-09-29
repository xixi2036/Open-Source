package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// 验证码策略参数。
const (
	CodeTTL         = 10 * time.Minute
	CodeResendAfter = 60 * time.Second
	CodeMaxAttempts = 5
	// 按邮箱的滚动失败上限：窗口内累计输错 AuthMaxFailures 次即锁定到窗口结束，重发验证码不清零。
	// 单个验证码最多试 5 次、60 秒可重发一次，没有这一层时每天可试约 7200 次。
	AuthMaxFailures = 10
	AuthLockWindow  = time.Hour
)

var (
	// ErrRateLimited 发送过于频繁。
	ErrRateLimited = errors.New("rate limited")
	// ErrBadCode 验证码错误、过期或已作废。
	ErrBadCode = errors.New("bad code")
	// ErrLocked 该邮箱近期输错次数过多，暂时锁定。
	ErrLocked = errors.New("locked")
)

// locked 判断失败计数是否处于锁定期。
func locked(failures int, since, now int64) bool {
	return failures >= AuthMaxFailures && now-since < AuthLockWindow.Milliseconds()
}

// SaveCode 为邮箱保存新验证码；60 秒内重复请求返回 ErrRateLimited，近期输错过多返回 ErrLocked。
// 重发只重置单个验证码的尝试次数，不清零按邮箱的失败计数。
func (s *Store) SaveCode(ctx context.Context, email, code string) error {
	now := NowMillis()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var sentAt, failSince int64
		var failures int
		err := tx.QueryRowContext(ctx, "SELECT sent_at, failures, failures_since FROM auth_codes WHERE email = ?", email).Scan(&sentAt, &failures, &failSince)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if locked(failures, failSince, now) {
				return ErrLocked
			}
			if now-sentAt < CodeResendAfter.Milliseconds() {
				return ErrRateLimited
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_codes (email, code, expires_at, sent_at, attempts) VALUES (?, ?, ?, ?, 0)
			ON CONFLICT(email) DO UPDATE SET code = excluded.code, expires_at = excluded.expires_at, sent_at = excluded.sent_at, attempts = 0`,
			email, code, now+CodeTTL.Milliseconds(), now)
		return err
	})
}

// ClearCode 删除验证码记录（发送失败时回退，允许立即重试）。
func (s *Store) ClearCode(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM auth_codes WHERE email = ?", email)
	return err
}

// CheckCode 校验验证码：正确则消费；错误累计次数（单码 5 次作废），并累计按邮箱的滚动失败计数，
// 达到上限后锁定到窗口结束（期间即使验证码正确也返回 ErrLocked）。
func (s *Store) CheckCode(ctx context.Context, email, code string) error {
	now := NowMillis()
	result := ErrBadCode
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var stored string
		var expires, failSince int64
		var attempts, failures int
		err := tx.QueryRowContext(ctx, "SELECT code, expires_at, attempts, failures, failures_since FROM auth_codes WHERE email = ?", email).
			Scan(&stored, &expires, &attempts, &failures, &failSince)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if locked(failures, failSince, now) {
			result = ErrLocked
			return nil
		}
		if expires < now || attempts >= CodeMaxAttempts {
			// 过期或已作废：保留 sent_at 以维持限流，但不可再用。
			_, err = tx.ExecContext(ctx, "UPDATE auth_codes SET attempts = ? WHERE email = ?", CodeMaxAttempts, email)
			return err
		}
		if stored == code {
			result = nil
			_, err = tx.ExecContext(ctx, "DELETE FROM auth_codes WHERE email = ?", email)
			return err
		}
		if now-failSince >= AuthLockWindow.Milliseconds() {
			failures, failSince = 0, now // 新窗口
		}
		_, err = tx.ExecContext(ctx, "UPDATE auth_codes SET attempts = attempts + 1, failures = ?, failures_since = ? WHERE email = ?",
			failures+1, failSince, email)
		return err
	})
	if err != nil {
		return err
	}
	return result
}

// TokenTouchInterval 是令牌最近使用时间的最小写库间隔（滑动过期的刷新节流）。
const TokenTouchInterval = time.Hour

// Authenticate 由 token 哈希查询用户并实现滑动过期（接口 v2 S-C）：
// ttl>0 且令牌已有 ttl 未使用 → 删除该令牌并返回 ErrNotFound；否则距上次记录超过 TokenTouchInterval 时
// 把 last_used_at 刷新为 now（每小时最多写一次库）。ttl<=0 表示永不过期（仍会刷新使用时间）。now 为毫秒。
func (s *Store) Authenticate(ctx context.Context, tokenHash string, now int64, ttl time.Duration) (string, error) {
	var uid string
	var last int64
	err := s.db.QueryRowContext(ctx, "SELECT user_id, last_used_at FROM tokens WHERE token_hash = ?", tokenHash).Scan(&uid, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if ttl > 0 && now-last > ttl.Milliseconds() {
		// 条件删除：并发请求里另一方刚刷新过就不删（它已证明令牌在有效期内被使用）。
		if _, err := s.db.ExecContext(ctx, "DELETE FROM tokens WHERE token_hash = ? AND last_used_at = ?", tokenHash, last); err != nil {
			return "", err
		}
		return "", ErrNotFound
	}
	if now-last >= TokenTouchInterval.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, "UPDATE tokens SET last_used_at = ? WHERE token_hash = ? AND last_used_at < ?", now, tokenHash, now); err != nil {
			return "", err
		}
	}
	return uid, nil
}

// TokenValid 判断 token 是否仍属于该用户（未被重新登录或退出登录作废）。
func (s *Store) TokenValid(ctx context.Context, userID, tokenHash string) (bool, error) {
	return exists(ctx, s.db, "SELECT 1 FROM tokens WHERE token_hash = ? AND user_id = ?", tokenHash, userID)
}

// DeleteToken 作废 token。
func (s *Store) DeleteToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM tokens WHERE token_hash = ?", tokenHash)
	return err
}
