// Package mail 发送登录验证码邮件。
package mail

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Sender 发送验证码。
type Sender interface {
	SendCode(to, code string) error
}

// MaskEmail 用于日志：只保留本地部分的首字符和完整域名，如 "alice@example.com" → "a***@example.com"。
func MaskEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return "***"
	}
	r := []rune(local)
	return string(r[0]) + "***@" + domain
}

// LogSender 把验证码打印到日志（开发模式）。邮箱打码，验证码原样打印以便本机调试。
type LogSender struct{}

// SendCode 实现 Sender。
func (LogSender) SendCode(to, code string) error {
	log.Printf("[mail] 开发模式验证码 %s -> %s", MaskEmail(to), code)
	return nil
}

// SMTPConfig 是 SMTP 配置。
type SMTPConfig struct {
	Host string
	Port string
	User string
	Pass string
	From string
}

// SMTPSender 通过 SMTP 真实发送邮件。端口 465 走隐式 TLS，其余端口在服务器支持时走 STARTTLS。
// 拨号与整个会话都有超时：SMTP 服务端挂起时不会让请求 goroutine 永久阻塞。
type SMTPSender struct {
	Cfg     SMTPConfig
	Timeout time.Duration // 整个发送过程（含拨号）的超时，默认 30 秒
}

const defaultSMTPTimeout = 30 * time.Second

// SendCode 实现 Sender。
func (s SMTPSender) SendCode(to, code string) error {
	from := s.Cfg.From
	if from == "" {
		from = s.Cfg.User
	}
	msg := buildMessage(from, to, code)
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultSMTPTimeout
	}
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(s.Cfg.Host, s.Cfg.Port)
	dialer := &net.Dialer{Deadline: deadline}
	tlsCfg := &tls.Config{ServerName: s.Cfg.Host}
	var conn net.Conn
	var err error
	if s.Cfg.Port == "465" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return err
	}
	c, err := smtp.NewClient(conn, s.Cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.Cfg.Port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		}
	}
	if s.Cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Cfg.User, s.Cfg.Pass, s.Cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func buildMessage(from, to, code string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	b.WriteString("Subject: =?UTF-8?B?SHVzaCDnmbvlvZXpqozor4HnoIE=?=\r\n") // "Hush 登录验证码"
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	fmt.Fprintf(&b, "你的 Hush 验证码是：%s\r\n\r\n10 分钟内有效。如非本人操作请忽略本邮件。\r\n", code)
	return []byte(b.String())
}

// Multi 依次调用多个 Sender，任一失败即返回错误。
type Multi []Sender

// SendCode 实现 Sender。
func (m Multi) SendCode(to, code string) error {
	for _, s := range m {
		if err := s.SendCode(to, code); err != nil {
			return err
		}
	}
	return nil
}
