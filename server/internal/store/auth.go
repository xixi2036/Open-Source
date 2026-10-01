package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// 验证码策略参数。
//
// 失败次数按（邮箱, 来源 IP）分桶：别人在另一个 IP 上乱试，锁住的只是他自己，不会把邮箱的主人锁在外面，
// 也不会让主人手里的验证码作废。按邮箱的总量上限只作为分布式爆破的兜底，阈值高得多。
const (
	CodeTTL         = 10 * time.Minute
	CodeResendAfter = 60 * time.Second
	// 单个验证码：同一个 IP 最多试 CodeMaxAttempts 次；全部 IP 合计 CodeMaxAttemptsTotal 次后作废。
	CodeMaxAttempts      = 5
	CodeMaxAttemptsTotal = 20
	// 滚动窗口内：同一个（邮箱, IP）累计输错 AuthMaxFailures 次即锁定该 IP 到窗口结束（重发验证码不清零）；
	// 同一个邮箱全部 IP 合计 AuthMaxFailuresTotal 次则锁定整个邮箱到窗口结束。
	AuthMaxFailures      = 10
	AuthMaxFailuresTotal = 50
	AuthLockWindow       = time.Hour
)

var (
	// ErrRateLimited 发送过于频繁。
	ErrRateLimited = errors.New("rate limited")
	// ErrBadCode 验证码错误、过期或已作废。
	ErrBadCode = errors.New("bad code")
	// ErrLocked 近期输错次数过多，暂时锁定。
	ErrLocked = errors.New("locked")
)

func lockedAt(failures, limit int, since, now int64) bool {
	return failures >= limit && now-since < AuthLockWindow.Milliseconds()
}

// codeState 是某邮箱当前的验证码与（调用方 IP 的）失败计数。
type codeState struct {
	found                      bool
	code                       string
	expires, sentAt, failSince int64
	attempts, failures         int // 全部 IP 合计

	ipFailures, ipAttempts int
	ipSince                int64
}

func loadCodeState(ctx context.Context, tx dbtx, email, ip string) (codeState, error) {
	var st codeState
	err := tx.QueryRowContext(ctx, "SELECT code, expires_at, sent_at, attempts, failures, failures_since FROM auth_codes WHERE email = ?", email).
		Scan(&st.code, &st.expires, &st.sentAt, &st.attempts, &st.failures, &st.failSince)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	st.found = err == nil
	err = tx.QueryRowContext(ctx, "SELECT failures, since, code_attempts FROM auth_failures WHERE email = ? AND ip = ?", email, ip).
		Scan(&st.ipFailures, &st.ipSince, &st.ipAttempts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	return st, nil
}

func (st *codeState) locked(now int64) bool {
	return lockedAt(st.failures, AuthMaxFailuresTotal, st.failSince, now) || lockedAt(st.ipFailures, AuthMaxFailures, st.ipSince, now)
}

// usable 判断当前验证码对这个 IP 是否还能用。
func (st *codeState) usable(now int64) bool {
	return st.found && st.expires >= now && st.attempts < CodeMaxAttemptsTotal && st.ipAttempts < CodeMaxAttempts
}

// SaveCode 为邮箱准备验证码并返回**应当发送的那一个**：60 秒内重复请求返回 ErrRateLimited，该 IP（或整个邮箱）
// 近期输错过多返回 ErrLocked。
//
// 已有的验证码还有效时重发同一个（不换新的、不清零尝试次数）：别人反复替受害者申请验证码，不会让受害者手里那个失效。
// 只有过期、已作废，或者申请者自己这个 IP 已经把它试满时才换成 code。
func (s *Store) SaveCode(ctx context.Context, email, ip, code string) (string, error) {
	now := NowMillis()
	send := code
	err := s.withTx(ctx, func(tx dbtx) error {
		st, err := loadCodeState(ctx, tx, email, ip)
		if err != nil {
			return err
		}
		if st.locked(now) {
			return ErrLocked
		}
		if st.found && now-st.sentAt < CodeResendAfter.Milliseconds() {
			return ErrRateLimited
		}
		if st.usable(now) {
			send = st.code
			_, err = tx.ExecContext(ctx, "UPDATE auth_codes SET sent_at = ? WHERE email = ?", now, email)
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO auth_codes (email, code, expires_at, sent_at, attempts) VALUES (?, ?, ?, ?, 0)
			ON CONFLICT(email) DO UPDATE SET code = excluded.code, expires_at = excluded.expires_at, sent_at = excluded.sent_at, attempts = 0`,
			email, code, now+CodeTTL.Milliseconds(), now)
		if err != nil {
			return err
		}
		// 换了新验证码：各 IP「对当前验证码的尝试次数」清零（滚动失败计数不清）
		_, err = tx.ExecContext(ctx, "UPDATE auth_failures SET code_attempts = 0 WHERE email = ? AND code_attempts > 0", email)
		return err
	})
	return send, err
}

// ClearCode 删除验证码记录（发送失败时回退，允许立即重试）。
func (s *Store) ClearCode(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM auth_codes WHERE email = ?", email)
	return err
}

// CheckCode 校验验证码：正确则消费；错误则累计次数（见本文件开头的策略参数）。锁定期间即使验证码正确也返回 ErrLocked。
func (s *Store) CheckCode(ctx context.Context, email, ip, code string) error {
	return s.checkCode(ctx, email, ip, code, true)
}

// PeekCode 与 CheckCode 的校验、计数完全相同，但验证码正确时**不消费**：由登录事务里的 Tx.ConsumeCode 消费。
// 这样注册策略（邀请码、停用账号等）拒绝登录时验证码仍然有效，用户补上邀请码即可重试，不必重新收一封邮件。
func (s *Store) PeekCode(ctx context.Context, email, ip, code string) error {
	return s.checkCode(ctx, email, ip, code, false)
}

// ConsumeCode 在登录事务内消费验证码，并清掉这个 IP 的失败计数；已被并发的另一次登录消费返回 ErrBadCode。
func (t *Tx) ConsumeCode(email, ip, code string) error {
	res, err := t.tx.ExecContext(t.ctx, "DELETE FROM auth_codes WHERE email = ? AND code = ?", email, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadCode
	}
	_, err = t.tx.ExecContext(t.ctx, "DELETE FROM auth_failures WHERE email = ? AND ip = ?", email, ip)
	return err
}

// recordFailure 记一次失败：全局计数与该 IP 的计数都加一。countWindow=false 时只记「对当前验证码的尝试」，不计入滚动失败窗口。
func recordFailure(ctx context.Context, tx dbtx, email, ip string, st codeState, now int64, countWindow bool) error {
	failures, since := st.failures, st.failSince
	ipFailures, ipSince := st.ipFailures, st.ipSince
	if countWindow {
		if now-since >= AuthLockWindow.Milliseconds() {
			failures, since = 0, now
		}
		if now-ipSince >= AuthLockWindow.Milliseconds() {
			ipFailures, ipSince = 0, now
		}
		failures++
		ipFailures++
	}
	if _, err := tx.ExecContext(ctx, "UPDATE auth_codes SET attempts = attempts + 1, failures = ?, failures_since = ? WHERE email = ?",
		failures, since, email); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO auth_failures (email, ip, failures, since, code_attempts, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(email, ip) DO UPDATE SET failures = excluded.failures, since = excluded.since,
			code_attempts = excluded.code_attempts, updated_at = excluded.updated_at`,
		email, ip, ipFailures, ipSince, st.ipAttempts+1, now)
	return err
}

// BumpCodeAttempt 给当前验证码记一次尝试（邀请码无效时调用：一个验证码最多试 CodeMaxAttempts 次邀请码）。
func (s *Store) BumpCodeAttempt(ctx context.Context, email, ip string) error {
	now := NowMillis()
	return s.withTx(ctx, func(tx dbtx) error {
		st, err := loadCodeState(ctx, tx, email, ip)
		if err != nil || !st.found {
			return err
		}
		return recordFailure(ctx, tx, email, ip, st, now, false)
	})
}

func (s *Store) checkCode(ctx context.Context, email, ip, code string, consume bool) error {
	now := NowMillis()
	result := ErrBadCode
	err := s.withTx(ctx, func(tx dbtx) error {
		st, err := loadCodeState(ctx, tx, email, ip)
		if err != nil || !st.found {
			return err
		}
		if st.locked(now) {
			result = ErrLocked
			return nil
		}
		if !st.usable(now) {
			return nil // 过期或已作废：保留记录以维持重发限流，但不可再用
		}
		if st.code == code {
			result = nil
			if !consume {
				return nil
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM auth_codes WHERE email = ?", email); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "DELETE FROM auth_failures WHERE email = ? AND ip = ?", email, ip)
			return err
		}
		return recordFailure(ctx, tx, email, ip, st, now, true)
	})
	if err != nil {
		return err
	}
	return result
}

// PruneAuth 删除早已过期且不在锁定窗口内的验证码记录与失败计数（这两张表否则只增不减）。
func (s *Store) PruneAuth(ctx context.Context, now int64) (int64, error) {
	cut := now - AuthLockWindow.Milliseconds() - CodeTTL.Milliseconds()
	n1, err := s.deleteBatched(ctx, "DELETE FROM auth_codes WHERE email IN (SELECT email FROM auth_codes WHERE expires_at < ? AND failures_since < ? LIMIT ?)", cut, cut)
	if err != nil {
		return n1, err
	}
	n2, err := s.deleteBatched(ctx, "DELETE FROM auth_failures WHERE (email, ip) IN (SELECT email, ip FROM auth_failures WHERE updated_at < ? LIMIT ?)", cut)
	return n1 + n2, err
}

// ---------- 令牌 ----------

// TokenTouchInterval 是令牌最近使用时间的最小写库间隔（滑动过期的刷新节流）。
// ttl 很小时取 ttl/4，保证在有效期内持续使用的令牌一定会被刷新（否则滑动过期会退化成绝对过期）。
const TokenTouchInterval = time.Hour

func tokenTouchEvery(ttl time.Duration) int64 {
	iv := TokenTouchInterval
	if ttl > 0 && ttl/4 < iv {
		iv = ttl / 4
	}
	return iv.Milliseconds()
}

// authCacheTTL 是鉴权结果在内存里的缓存时间。令牌被作废（重新登录、退出、停用、强制下线、删除账号、过期）时立即失效。
const authCacheTTL = 30 * time.Second

type authEntry struct {
	uid  string
	last int64 // 已知的 last_used_at
	at   int64 // 写入缓存的时刻（毫秒，墙上时钟）
}

// authCache 把「令牌哈希 → 用户」缓存一小段时间，省掉每个请求一次数据库查询。
// 只用于请求入口的鉴权；ack、上传密钥、WS 登记这些安全不变量仍然在写事务里直接查 tokens 表。
type authCache struct {
	m   sync.Map // tokenHash → *authEntry
	gen atomic.Uint64
	n   atomic.Int64
}

const authCacheMax = 200_000

func (c *authCache) init() {}

func (c *authCache) get(hash string) (*authEntry, bool) {
	v, ok := c.m.Load(hash)
	if !ok {
		return nil, false
	}
	e := v.(*authEntry)
	if time.Now().UnixMilli()-e.at > authCacheTTL.Milliseconds() {
		return nil, false
	}
	return e, true
}

// put 写入缓存；gen 是读库之前记下的代数——期间发生过任何作废就不缓存（读到的可能已是旧数据）。
func (c *authCache) put(hash string, e *authEntry, gen uint64) {
	if c.n.Load() > authCacheMax {
		c.clear()
	}
	if _, loaded := c.m.Swap(hash, e); !loaded {
		c.n.Add(1)
	}
	if c.gen.Load() != gen {
		c.dropToken(hash)
	}
}

func (c *authCache) dropToken(hash string) {
	c.gen.Add(1)
	if _, loaded := c.m.LoadAndDelete(hash); loaded {
		c.n.Add(-1)
	}
}

func (c *authCache) dropUser(uid string) {
	c.gen.Add(1)
	c.m.Range(func(k, v any) bool {
		if v.(*authEntry).uid == uid {
			if _, loaded := c.m.LoadAndDelete(k); loaded {
				c.n.Add(-1)
			}
		}
		return true
	})
}

func (c *authCache) clear() {
	c.gen.Add(1)
	c.m.Range(func(k, _ any) bool {
		if _, loaded := c.m.LoadAndDelete(k); loaded {
			c.n.Add(-1)
		}
		return true
	})
}

// InvalidateAuthCache 清空鉴权缓存（绕过本程序直接改了 tokens 表之后调用；测试用）。
func (s *Store) InvalidateAuthCache() { s.auth.clear() }

// InvalidateUserTokens 让某用户的鉴权缓存立即失效（本事务作废了他的令牌时调用；提交之后生效）。
func (t *Tx) InvalidateUserTokens(uid string) {
	t.OnCommit(func() { t.s.auth.dropUser(uid) })
}

// Authenticate 由 token 哈希查询用户并实现滑动过期（接口 v2 S-C）：
// ttl>0 且令牌已有 ttl 未使用 → 删除该令牌并返回 ErrNotFound；否则距上次记录超过刷新间隔（1 小时与 ttl/4 中较小者）时
// 把 last_used_at 刷新为 now。ttl<=0 表示永不过期（仍会刷新使用时间）。now 为毫秒。
// 查询结果在内存里缓存 authCacheTTL，令牌被作废时立即失效。
func (s *Store) Authenticate(ctx context.Context, tokenHash string, now int64, ttl time.Duration) (string, error) {
	var uid string
	var last int64
	if e, ok := s.auth.get(tokenHash); ok {
		uid, last = e.uid, e.last
	} else {
		gen := s.auth.gen.Load()
		err := s.rd.QueryRowContext(ctx, "SELECT user_id, last_used_at FROM tokens WHERE token_hash = ?", tokenHash).Scan(&uid, &last)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		s.auth.put(tokenHash, &authEntry{uid: uid, last: last, at: time.Now().UnixMilli()}, gen)
	}
	if ttl > 0 && now-last > ttl.Milliseconds() {
		// 条件删除：并发请求里另一方刚刷新过就不删（它已证明令牌在有效期内被使用）。
		s.auth.dropToken(tokenHash)
		if _, err := s.db.ExecContext(ctx, "DELETE FROM tokens WHERE token_hash = ? AND last_used_at = ?", tokenHash, last); err != nil {
			return "", err
		}
		return "", ErrNotFound
	}
	if now-last >= tokenTouchEvery(ttl) {
		gen := s.auth.gen.Load()
		res, err := s.db.ExecContext(ctx, "UPDATE tokens SET last_used_at = ? WHERE token_hash = ? AND last_used_at < ?", now, tokenHash, now)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.auth.put(tokenHash, &authEntry{uid: uid, last: now, at: time.Now().UnixMilli()}, gen)
		} else {
			s.auth.dropToken(tokenHash) // 令牌刚被作废，或别的请求已经刷新：下次重新读库
		}
	}
	return uid, nil
}

// TokenValid 判断 token 是否仍属于该用户（未被重新登录或退出登录作废）。不走缓存。
func (s *Store) TokenValid(ctx context.Context, userID, tokenHash string) (bool, error) {
	return exists(ctx, s.rd, "SELECT 1 FROM tokens WHERE token_hash = ? AND user_id = ?", tokenHash, userID)
}

// DeleteToken 作废 token。
func (s *Store) DeleteToken(ctx context.Context, tokenHash string) error {
	return s.Update(ctx, func(t *Tx) error {
		t.OnCommit(func() { s.auth.dropToken(tokenHash) })
		_, err := t.tx.ExecContext(ctx, "DELETE FROM tokens WHERE token_hash = ?", tokenHash)
		return err
	})
}

// HasValidToken 判断用户此刻是否处于登录状态（有未作废的令牌且账号未停用）。TURN 凭证据此与登录态绑定。
func (s *Store) HasValidToken(ctx context.Context, userID string) (bool, error) {
	return exists(ctx, s.rd, "SELECT 1 FROM tokens t JOIN users u ON u.id = t.user_id WHERE t.user_id = ? AND u.disabled_at = 0", userID)
}
