// hush-server：Hush 加密社交 App 的单体服务端。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hush/server/internal/api"
	"hush/server/internal/mail"
	"hush/server/internal/push"
	"hush/server/internal/store"
	"hush/server/internal/turn"
	"hush/server/internal/ws"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	dataDir := flag.String("data", "./data", "数据目录（SQLite、附件、TURN 密钥）")
	turnPort := flag.Int("turn-port", 3478, "内嵌 TURN/STUN 的 UDP 端口（0 表示不启动）")
	publicIP := flag.String("public-ip", "127.0.0.1", "TURN 中继地址（客户端可达的本机 IP）")
	dev := flag.Bool("dev", false, "开发模式：验证码打印到日志")
	tokenTTL := flag.Duration("token-ttl", api.DefaultTokenTTL, "登录令牌滑动过期时长：超过这么久未使用即失效（0 表示永不过期，测试用）")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := os.MkdirAll(filepath.Join(*dataDir, "attachments"), 0o700); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	secret, err := turnSecret(*dataDir)
	if err != nil {
		log.Fatalf("加载 TURN 密钥失败: %v", err)
	}

	devCode, warn := resolveDevCode(os.Getenv("HUSH_DEV_CODE"), *dev, *addr)
	if warn != "" {
		log.Printf("[config] 警告：%s", warn)
	}
	if devCode != "" {
		log.Printf("[config] HUSH_DEV_CODE 已生效（--dev 且仅监听回环地址 %s）：该验证码对任意邮箱永远有效，仅限本机开发！", *addr)
	}

	// 先启动 TURN：只有启动成功时才把端口交给 API（否则 /v1/calls/turn 会下发不可用的地址）。
	activeTurnPort := 0
	if *turnPort > 0 {
		ts, err := turn.Start(turn.Config{Port: *turnPort, PublicIP: *publicIP, Secret: secret, AllowPrivatePeers: *dev})
		if err != nil {
			log.Printf("[turn] 警告：TURN 启动失败（通话中继不可用，HTTP 服务继续运行）: %v", err)
		} else {
			defer ts.Close()
			activeTurnPort = *turnPort
			log.Printf("[turn] TURN/STUN 监听 udp 0.0.0.0:%d，中继地址 %s，允许中继到私有地址=%v", *turnPort, *publicIP, *dev)
		}
	}

	authPerIP := authPerIPLimit(*addr)
	if authPerIP > 0 {
		log.Printf("[config] 仅监听回环地址 %s：登录接口不按 IP 限速（本机所有模拟器、机器人都来自 127.0.0.1）", *addr)
	}

	if *tokenTTL > 0 {
		log.Printf("[config] 登录令牌 %s 未使用即失效", *tokenTTL)
	} else {
		log.Printf("[config] 登录令牌永不过期（--token-ttl 0）")
	}

	hub := ws.NewHub(st, push.LogPusher{})
	handler := api.New(api.Config{
		Store:      st,
		Hub:        hub,
		Mailer:     buildMailer(*dev),
		DataDir:    *dataDir,
		DevCode:    devCode,
		TurnSecret: secret,
		TurnPort:   activeTurnPort,
		TokenTTL:   *tokenTTL,

		AuthCodePerIPPerMinute:   authPerIP,
		AuthVerifyPerIPPerMinute: authPerIP,
	})

	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	defer stopJanitor()
	go api.RunJanitor(janitorCtx, st, *dataDir)

	srv := api.NewHTTPServer(*addr, handler, api.DefaultTimeouts)
	go func() {
		log.Printf("[http] hush-server 监听 %s，数据目录 %s，dev=%v", *addr, *dataDir, *dev)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("正在关闭...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// isLoopbackAddr 判断监听地址是否只绑定回环接口（127.0.0.0/8、::1、localhost）。
// ":8080"、"0.0.0.0:8080" 之类监听全部网卡的地址不算回环。
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authPerIPLimit 返回登录接口的按 IP 限速值（0 表示用 api 包默认值）。
// 只监听回环地址时，所有客户端（模拟器经 10.0.2.2、iOS 模拟器、测试机器人）都来自 127.0.0.1，
// 按 IP 限速等于全体共享一个额度，因此放开；监听局域网 / 公网地址时保留默认限速。
func authPerIPLimit(addr string) int {
	if isLoopbackAddr(addr) {
		return 1 << 20
	}
	return 0
}

// resolveDevCode 决定 HUSH_DEV_CODE 是否生效：只有 --dev 且监听地址为回环时才生效，
// 否则返回空串与一条警告（固定验证码暴露在局域网/公网上等于任何人都能登录任何账号）。
func resolveDevCode(code string, dev bool, addr string) (effective, warning string) {
	if code == "" {
		return "", ""
	}
	if !dev {
		return "", "已设置 HUSH_DEV_CODE 但未加 --dev，固定验证码不生效"
	}
	if !isLoopbackAddr(addr) {
		return "", fmt.Sprintf("已设置 HUSH_DEV_CODE 但监听地址 %q 不是回环地址，固定验证码不生效（请用日志里打印的验证码或配置 SMTP）", addr)
	}
	return code, ""
}

// buildMailer 根据环境变量选择验证码发送方式。
func buildMailer(dev bool) mail.Sender {
	host := os.Getenv("HUSH_SMTP_HOST")
	var senders mail.Multi
	if dev {
		senders = append(senders, mail.LogSender{})
	}
	if host != "" {
		port := os.Getenv("HUSH_SMTP_PORT")
		if port == "" {
			port = "587"
		}
		senders = append(senders, mail.SMTPSender{Cfg: mail.SMTPConfig{
			Host: host, Port: port,
			User: os.Getenv("HUSH_SMTP_USER"), Pass: os.Getenv("HUSH_SMTP_PASS"), From: os.Getenv("HUSH_SMTP_FROM"),
		}})
		log.Printf("[config] SMTP 已配置：%s:%s", host, port)
	}
	if len(senders) == 0 {
		log.Printf("[config] 警告：未配置 SMTP 且非 --dev，/v1/auth/code 将返回错误，任何人都无法登录")
		return nil
	}
	return senders
}

// turnSecret 读取 HUSH_TURN_SECRET；未设置时从 data/turn_secret 加载，不存在则随机生成并持久化。
func turnSecret(dataDir string) (string, error) {
	if s := os.Getenv("HUSH_TURN_SECRET"); s != "" {
		return s, nil
	}
	path := filepath.Join(dataDir, "turn_secret")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(s+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Printf("[config] 已生成 TURN 密钥并保存到 %s", path)
	return s, nil
}
