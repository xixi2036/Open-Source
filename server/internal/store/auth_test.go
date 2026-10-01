package store

import (
	"context"
	"testing"
)

const testIP = "198.51.100.7"

// 按（邮箱, IP）的滚动失败上限：重发验证码不清零，达到上限后即使验证码正确也拒绝，窗口过后恢复（审查 S9）。
func TestAuthFailureLockout(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const email = "victim@example.com"
	guessRound := func(code string) {
		t.Helper()
		if _, err := s.SaveCode(ctx, email, testIP, code); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < CodeMaxAttempts; i++ {
			if err := s.CheckCode(ctx, email, testIP, "000000"); err != ErrBadCode {
				t.Fatalf("错误验证码应返回 ErrBadCode，得到 %v", err)
			}
		}
		// 模拟 60 秒后重发
		if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET sent_at = 0 WHERE email = ?", email); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < AuthMaxFailures/CodeMaxAttempts; round++ {
		guessRound("123456")
	}
	if _, err := s.SaveCode(ctx, email, testIP, "654321"); err != ErrLocked {
		t.Fatalf("锁定期内不应再发验证码，得到 %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET code = '654321', attempts = 0, expires_at = ? WHERE email = ?", NowMillis()+60000, email); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCode(ctx, email, testIP, "654321"); err != ErrLocked {
		t.Fatalf("锁定期内正确验证码也应被拒，得到 %v", err)
	}
	// 窗口过去之后恢复
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_failures SET since = since - ? WHERE email = ?", AuthLockWindow.Milliseconds(), email); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET sent_at = 0, expires_at = 0 WHERE email = ?", email); err != nil {
		t.Fatal(err)
	}
	if code, err := s.SaveCode(ctx, email, testIP, "111111"); err != nil || code != "111111" {
		t.Fatalf("窗口过后应可重新申请: %v %v", code, err)
	}
	if err := s.CheckCode(ctx, email, testIP, "111111"); err != nil {
		t.Fatalf("窗口过后应可正常登录: %v", err)
	}
}

// B2：别人在另一个 IP 上乱试，锁住的是他自己；邮箱主人手里的验证码仍然有效，也能继续申请验证码。
func TestAuthFailuresBucketedByIP(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const email, attacker, victim = "victim@example.com", "203.0.113.9", "198.51.100.7"
	code, err := s.SaveCode(ctx, email, victim, "424242")
	if err != nil || code != "424242" {
		t.Fatal(code, err)
	}
	// 攻击者把自己的额度全部用完（跨两个「重发」周期）
	for i := 0; i < AuthMaxFailures; i++ {
		if i == CodeMaxAttempts {
			if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET sent_at = 0 WHERE email = ?", email); err != nil {
				t.Fatal(err)
			}
			// 攻击者替受害者重新申请：他自己的 IP 已把这个验证码试满，于是换了一个新的
			if _, err := s.SaveCode(ctx, email, attacker, "777777"); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CheckCode(ctx, email, attacker, "000000"); err != ErrBadCode {
			t.Fatalf("第 %d 次应为 ErrBadCode，得到 %v", i, err)
		}
	}
	if err := s.CheckCode(ctx, email, attacker, "777777"); err != ErrLocked {
		t.Fatalf("攻击者的 IP 应被锁定，得到 %v", err)
	}
	if _, err := s.SaveCode(ctx, email, attacker, "888888"); err != ErrLocked {
		t.Fatalf("被锁定的 IP 不能再申请验证码，得到 %v", err)
	}
	// 受害者不受影响：可以申请，拿到的仍是当前有效的那个验证码，并且能登录
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET sent_at = 0 WHERE email = ?", email); err != nil {
		t.Fatal(err)
	}
	code, err = s.SaveCode(ctx, email, victim, "999999")
	if err != nil {
		t.Fatalf("受害者应能申请验证码: %v", err)
	}
	if code != "777777" {
		t.Fatalf("已有的验证码仍有效时应重发同一个，得到 %s", code)
	}
	if err := s.CheckCode(ctx, email, victim, code); err != nil {
		t.Fatalf("受害者应能登录: %v", err)
	}
}

// B2：重发不会让手里的验证码失效（别人每 60 秒替你申请一次也没用）。
func TestResendKeepsValidCode(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const email = "a@example.com"
	if _, err := s.SaveCode(ctx, email, testIP, "111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveCode(ctx, email, "203.0.113.9", "222222"); err != ErrRateLimited {
		t.Fatalf("60 秒内重复申请应限流，得到 %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET sent_at = 0 WHERE email = ?", email); err != nil {
		t.Fatal(err)
	}
	code, err := s.SaveCode(ctx, email, "203.0.113.9", "222222")
	if err != nil || code != "111111" {
		t.Fatalf("应重发原验证码，得到 %q %v", code, err)
	}
	if err := s.CheckCode(ctx, email, testIP, "111111"); err != nil {
		t.Fatalf("原验证码应仍然有效: %v", err)
	}
}

// 分布式爆破的兜底：全部 IP 合计的失败次数到上限后整个邮箱锁定。
func TestAuthGlobalBackstop(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const email = "a@example.com"
	if _, err := s.SaveCode(ctx, email, testIP, "111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET failures = ?, failures_since = ? WHERE email = ?", AuthMaxFailuresTotal, NowMillis(), email); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCode(ctx, email, "192.0.2.1", "111111"); err != ErrLocked {
		t.Fatalf("合计失败达到上限后应锁定，得到 %v", err)
	}
	// 单个验证码合计试满后作废
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET failures = 0, attempts = ? WHERE email = ?", CodeMaxAttemptsTotal, email); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCode(ctx, email, "192.0.2.1", "111111"); err != ErrBadCode {
		t.Fatalf("验证码合计试满后应作废，得到 %v", err)
	}
}
