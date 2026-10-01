package logx

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestLevels(t *testing.T) {
	var buf lockedBuf
	old := log.Writer()
	log.SetOutput(&buf)
	defer func() { log.SetOutput(old); SetLevel(Info) }()
	SetLevel(Info)
	Debugf("debug-line")
	Infof("info-line")
	Warnf("warn-line")
	if s := buf.String(); strings.Contains(s, "debug-line") || !strings.Contains(s, "info-line") || !strings.Contains(s, "warn-line") {
		t.Fatalf("info 级别下的输出不对: %q", s)
	}
	SetLevel(Debug)
	Debugf("debug-line")
	if !strings.Contains(buf.String(), "debug-line") {
		t.Fatal("debug 级别应输出 debug 日志")
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("未知级别应报错")
	}
	if l, _ := ParseLevel("WARN"); l != Warn {
		t.Fatal("级别解析不区分大小写")
	}
}

func TestAsyncWriterFlushesOnTickAndClose(t *testing.T) {
	var buf lockedBuf
	a := NewAsyncWriter(&buf, 1<<16, 10*time.Millisecond)
	a.Write([]byte("one\n"))
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "one") {
		if time.Now().After(deadline) {
			t.Fatal("定时刷新没有发生")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.Write([]byte("two\n"))
	a.Close()
	if !strings.Contains(buf.String(), "two") {
		t.Fatal("Close 应刷出剩余内容")
	}
}
