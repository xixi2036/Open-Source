package mail

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"alice@example.com": "a***@example.com",
		"b@x.io":            "b***@x.io",
		"张三@例子.中国":          "张***@例子.中国",
		"no-at-sign":        "***",
		"@domain.com":       "***",
		"":                  "***",
	}
	for in, want := range cases {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestLogSenderMasksEmailKeepsCode(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	LogSender{}.SendCode("secret.person@example.com", "123456")
	out := buf.String()
	if strings.Contains(out, "secret.person") || !strings.Contains(out, "s***@example.com") || !strings.Contains(out, "123456") {
		t.Fatalf("日志应打码邮箱、保留验证码: %s", out)
	}
}

// SMTP 服务端接受连接后不响应：非 465 端口也必须在超时后返回错误，而不是永久阻塞（审查 S13）。
func TestSMTPSenderTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // 接受后一言不发
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s := SMTPSender{Cfg: SMTPConfig{Host: "127.0.0.1", Port: port, From: "a@example.com"}, Timeout: 300 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- s.SendCode("b@example.com", "123456") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("挂起的 SMTP 服务端应导致发送失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP 发送没有超时")
	}
}

// 最简 SMTP 服务端：走完整个会话（不支持 STARTTLS 时明文发送）。
func TestSMTPSenderPlainSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		fmt.Fprint(c, "220 test\r\n")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					fmt.Fprint(c, "250 ok\r\n")
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				fmt.Fprint(c, "250 test\r\n")
			case cmd == "DATA":
				inData = true
				fmt.Fprint(c, "354 go\r\n")
			case cmd == "QUIT":
				fmt.Fprint(c, "221 bye\r\n")
				got <- data.String()
				return
			default:
				fmt.Fprint(c, "250 ok\r\n")
			}
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	s := SMTPSender{Cfg: SMTPConfig{Host: "127.0.0.1", Port: port, From: "a@example.com"}, Timeout: 5 * time.Second}
	if err := s.SendCode("b@example.com", "654321"); err != nil {
		t.Fatal(err)
	}
	if body := <-got; !strings.Contains(body, "654321") {
		t.Fatalf("邮件正文应包含验证码: %q", body)
	}
}
