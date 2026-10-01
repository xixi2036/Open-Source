package api_test

import (
	"bytes"
	"hush/server/internal/logx"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
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

// 登录相关日志不打印邮箱全文。
func TestAuthLogsMaskEmail(t *testing.T) {
	var buf lockedBuf
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)

	e := newEnv(t, true)
	email := "private.person@example.com"
	anon := &client{e: e}
	anon.must("POST", "/v1/auth/code", map[string]any{"email": email}, nil)
	anon.expectErr("POST", "/v1/auth/verify", map[string]any{"email": email, "code": "000000"}, 401, "unauthorized")
	e.login(email)

	out := buf.String()
	if strings.Contains(out, "private.person") {
		t.Fatalf("日志中出现了邮箱全文:\n%s", out)
	}
	if !strings.Contains(out, "p***@example.com") {
		t.Fatalf("日志应包含打码后的邮箱:\n%s", out)
	}
}

// 日志里不出现完整附件 id（凭 id 即可下载）。
func TestLogsMaskAttachmentID(t *testing.T) {
	var buf lockedBuf
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)
	logx.SetLevel(logx.Debug) // 请求日志是 debug 级
	defer logx.SetLevel(logx.Info)

	e := newEnv(t, true)
	a := e.register("a")
	var up upResp
	a.must("POST", "/v1/attachments", []byte("secret-photo"), &up)
	a.must("GET", "/v1/attachments/"+up.ID, nil, nil)
	out := buf.String()
	if strings.Contains(out, up.ID) {
		t.Fatalf("日志中出现了完整附件 id:\n%s", out)
	}
	if !strings.Contains(out, "/v1/attachments/"+up.ID[:8]+"…") {
		t.Fatalf("日志应包含截断后的附件路径:\n%s", out)
	}
}

// ?token= 只在 WebSocket 上接受；其他接口必须用 Authorization 头。
func TestQueryTokenOnlyForWS(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	st, body := e.do("GET", "/v1/me?token="+a.Token, "", nil)
	assertErr(t, st, body, 401, "unauthorized")
	w := a.dial() // dial 使用 ?token=
	w.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
}
