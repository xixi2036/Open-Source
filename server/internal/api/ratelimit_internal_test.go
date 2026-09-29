package api

import (
	"sync"
	"testing"
	"time"
)

func TestWindowLimiter(t *testing.T) {
	l := newWindowLimiter(3, time.Minute)
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !l.allow("a|b", now) {
			t.Fatalf("第 %d 次应允许", i+1)
		}
	}
	if l.allow("a|b", now.Add(59*time.Second)) {
		t.Fatal("窗口内第 4 次应拒绝")
	}
	if !l.allow("a|c", now) {
		t.Fatal("不同目标互不影响")
	}
	if !l.allow("a|b", now.Add(time.Minute)) {
		t.Fatal("新窗口应重新计数")
	}
	// 并发安全（-race）
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.allow("x", now) }()
	}
	wg.Wait()
}
