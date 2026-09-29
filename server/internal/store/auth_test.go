package store

import (
	"context"
	"testing"
)

// 按邮箱的滚动失败上限：重发验证码不清零，达到上限后即使验证码正确也拒绝，窗口过后恢复（审查 S9）。
func TestAuthFailureLockout(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const email = "victim@example.com"
	guessRound := func(code string) {
		t.Helper()
		if err := s.SaveCode(ctx, email, code); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < CodeMaxAttempts; i++ {
			if err := s.CheckCode(ctx, email, "000000"); err != ErrBadCode {
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
	if err := s.SaveCode(ctx, email, "654321"); err != ErrLocked {
		t.Fatalf("锁定期内不应再发验证码，得到 %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET code = '654321', attempts = 0, expires_at = ? WHERE email = ?", NowMillis()+60000, email); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCode(ctx, email, "654321"); err != ErrLocked {
		t.Fatalf("锁定期内正确验证码也应被拒，得到 %v", err)
	}
	// 窗口过去之后恢复
	if _, err := s.db.ExecContext(ctx, "UPDATE auth_codes SET failures_since = failures_since - ? WHERE email = ?", AuthLockWindow.Milliseconds(), email); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCode(ctx, email, "111111"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckCode(ctx, email, "111111"); err != nil {
		t.Fatalf("窗口过后应可正常登录: %v", err)
	}
}
