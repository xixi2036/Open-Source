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
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"hush/server/internal/admin"
	"hush/server/internal/api"
	"hush/server/internal/logx"
	"hush/server/internal/mail"
	"hush/server/internal/netx"
	"hush/server/internal/push"
	"hush/server/internal/store"
	"hush/server/internal/turn"
	"hush/server/internal/ws"
)

// version 可在构建时用 -ldflags "-X main.version=1.2.3" 指定；缺省时取 VCS 提交号。
var version = ""

func buildVersion() string {
	v := version
	if v == "" {
		v = "dev"
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v += "+" + s.Value[:7]
			}
		}
	}
	return v
}

func main() {
	// 子命令：hush-server admin create|passwd|list …（管理后台账号，见 internal/admin/cli.go）
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(admin.RunCLI(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	addr := flag.String("addr", ":8080", "HTTP 监听地址")
	adminAddr := flag.String("admin-addr", "127.0.0.1:8081", "管理后台监听地址（空字符串表示不启用）；默认只监听回环，对外暴露请放在 TLS 反向代理之后")
	dataDir := flag.String("data", "./data", "数据目录（SQLite、附件、TURN 密钥）")
	turnPort := flag.Int("turn-port", 3478, "内嵌 TURN/STUN 的 UDP 端口（0 表示不启动）")
	publicIP := flag.String("public-ip", "127.0.0.1", "TURN 中继地址（客户端可达的本机 IP）")
	turnTTL := flag.Duration("turn-ttl", api.DefaultTurnTTL, "TURN 凭证有效期（凭证同时与登录态绑定：退出登录或被停用后立即失效）")
	dev := flag.Bool("dev", false, "开发模式：验证码打印到日志")
	tokenTTL := flag.Duration("token-ttl", api.DefaultTokenTTL, "登录令牌滑动过期时长：超过这么久未使用即失效（0 表示永不过期，测试用）")
	tlsCert := flag.String("tls-cert", "", "TLS 证书文件（PEM，含证书链）；与 --tls-key 同时指定后 --addr 直接提供 HTTPS / WSS")
	tlsKey := flag.String("tls-key", "", "TLS 私钥文件（PEM）")
	trustedProxy := flag.String("trusted-proxy", "", "可信反向代理的地址段（逗号分隔的 CIDR 或 IP，如 127.0.0.1,10.0.0.0/8）；配置后才采信它们写的 X-Forwarded-For")
	logLevel := flag.String("log-level", "info", "日志级别：debug（含逐请求、逐条投递与社交关系明细）| info | warn | error")
	syncFull := flag.Bool("sync-full", false, "SQLite synchronous=FULL：每次提交都落盘，掉电不丢已提交的数据（写入变慢）；默认 NORMAL")
	dbReaders := flag.Int("db-readers", 0, "只读数据库连接数（0 表示自动：CPU 数，介于 4 与 16 之间）")
	maxConns := flag.Int("max-conns", 20000, "WebSocket 连接数上限（0 表示不限），超出返回 503")
	maxConnsPerIP := flag.Int("max-conns-per-ip", -1, "每个客户端 IP 的 WebSocket 连接数上限（0 表示不限；-1 自动：200，只监听回环且没有可信代理时不限）")
	maxInflight := flag.Int("max-inflight", api.DefaultMaxInflight, "同时处理的请求数上限（不含 WebSocket 与附件），超出返回 503")
	maxUploads := flag.Int("max-uploads", api.DefaultMaxUploads, "同时进行的附件上传数上限，超出返回 503")
	shutdownTimeout := flag.Duration("shutdown-timeout", 15*time.Second, "优雅停机的最长等待时间")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	lvl, err := logx.ParseLevel(*logLevel)
	if err != nil {
		log.Fatalf("%v", err)
	}
	logx.SetLevel(lvl)
	// 日志异步缓冲输出：写日志的协程不等终端 / 磁盘。退出前（含 Fatal）刷出。
	logOut := logx.NewAsyncWriter(os.Stderr, 256<<10, 200*time.Millisecond)
	log.SetOutput(logOut)
	defer logOut.Close()
	fatalf := func(format string, args ...any) {
		log.Printf(format, args...)
		logOut.Close()
		os.Exit(1)
	}

	proxies, err := netx.ParseProxies(*trustedProxy)
	if err != nil {
		fatalf("--trusted-proxy: %v", err)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fatalf("--tls-cert 与 --tls-key 必须同时指定")
	}

	if err := os.MkdirAll(filepath.Join(*dataDir, "attachments"), 0o700); err != nil {
		fatalf("创建数据目录失败: %v", err)
	}
	st, err := store.OpenWith(*dataDir, store.Options{ReadConns: *dbReaders, SyncFull: *syncFull})
	if err != nil {
		fatalf("打开数据库失败: %v", err)
	}

	secret, err := turnSecret(*dataDir)
	if err != nil {
		fatalf("加载 TURN 密钥失败: %v", err)
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
	var turnSrv *turn.Server
	if *turnPort > 0 {
		ts, err := turn.Start(turn.Config{Port: *turnPort, PublicIP: *publicIP, Secret: secret, AllowPrivatePeers: *dev,
			Validate: loginStateChecker(st)})
		if err != nil {
			log.Printf("[turn] 警告：TURN 启动失败（通话中继不可用，HTTP 服务继续运行）: %v", err)
		} else {
			turnSrv = ts
			activeTurnPort = *turnPort
			log.Printf("[turn] TURN/STUN 监听 udp 0.0.0.0:%d，中继地址 %s，允许中继到私有地址=%v，凭证有效期 %s", *turnPort, *publicIP, *dev, *turnTTL)
		}
	}

	// 只监听回环且没有可信代理时，所有客户端都来自 127.0.0.1：按 IP 的限制等于全体共用一个额度，因此放开。
	// 配了可信代理就能拿到真实的客户端 IP，照常按 IP 限制。
	authPerIP := authPerIPLimit(*addr, proxies)
	loopbackOnly := authPerIP > 0
	if loopbackOnly {
		log.Printf("[config] 仅监听回环地址 %s：登录接口与 WebSocket 连接数不按 IP 限制（本机所有模拟器、机器人都来自 127.0.0.1）", *addr)
	}
	if proxies.Enabled() {
		log.Printf("[config] 可信代理 %s：来自它们的请求按 X-Forwarded-For 识别客户端 IP", *trustedProxy)
	}
	perIP := *maxConnsPerIP
	if perIP < 0 {
		perIP = 200
		if loopbackOnly {
			perIP = 0
		}
	}

	if *tokenTTL > 0 {
		log.Printf("[config] 登录令牌 %s 未使用即失效", *tokenTTL)
	} else {
		log.Printf("[config] 登录令牌永不过期（--token-ttl 0）")
	}

	hub := ws.NewHub(st, push.LogPusher{})
	hub.SetLimits(*maxConns, perIP)
	apiSrv := api.NewServer(api.Config{
		Store:      st,
		Hub:        hub,
		Mailer:     buildMailer(*dev),
		DataDir:    *dataDir,
		DevCode:    devCode,
		TurnSecret: secret,
		TurnPort:   activeTurnPort,
		TurnTTL:    *turnTTL,
		TokenTTL:   *tokenTTL,

		AuthCodePerIPPerMinute:   authPerIP,
		AuthVerifyPerIPPerMinute: authPerIP,
		MaxInflight:              *maxInflight,
		MaxUploads:               *maxUploads,
		TrustedProxies:           proxies,
	})
	handler := apiSrv.Handler()
	// 后台在线修改过的设置（settings 表）覆盖启动参数
	if skipped, err := apiSrv.LoadSettings(context.Background()); err != nil {
		fatalf("读取系统设置失败: %v", err)
	} else if len(skipped) > 0 {
		log.Printf("[config] 警告：settings 表里有无效的设置项，已忽略：%s", strings.Join(skipped, "；"))
	}
	if mode := apiSrv.Settings().RegistrationMode; mode != api.RegOpen {
		log.Printf("[config] 注册模式：%s", mode)
	}

	bgCtx, stopBackground := context.WithCancel(context.Background())
	var background sync.WaitGroup
	background.Add(3)
	go func() {
		defer background.Done()
		api.RunJanitor(bgCtx, st, *dataDir, func(now time.Time) { hub.PruneEpochs(now) })
	}()
	go func() { defer background.Done(); apiSrv.RunStats(bgCtx) }()
	go func() {
		defer background.Done()
		// 上次进程退出时没扇出完的公告：续发
		if n, err := st.ResumeAnnouncementFanouts(bgCtx); err != nil && bgCtx.Err() == nil {
			log.Printf("[admin] 续发公告失败: %v", err)
		} else if n > 0 {
			log.Printf("[admin] 已续发 %d 条上次未发完的公告", n)
		}
	}()

	var adminSrv *http.Server
	if *adminAddr != "" {
		adminSrv = startAdmin(*adminAddr, admin.New(admin.Config{
			Store: st, Hub: hub, API: apiSrv, DataDir: *dataDir, Version: buildVersion(),
			SecureCookie:   os.Getenv("HUSH_ADMIN_SECURE_COOKIE") == "1",
			MetricsToken:   os.Getenv("HUSH_METRICS_TOKEN"),
			TrustedProxies: proxies,
		}), st)
	}

	srv := api.NewHTTPServer(*addr, handler, api.DefaultTimeouts)
	serveErr := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			log.Printf("[http] hush-server 监听 https://%s（TLS），数据目录 %s，dev=%v", *addr, *dataDir, *dev)
			serveErr <- srv.ListenAndServeTLS(*tlsCert, *tlsKey)
			return
		}
		log.Printf("[http] hush-server 监听 %s，数据目录 %s，dev=%v", *addr, *dataDir, *dev)
		serveErr <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	exitCode := 0
	select {
	case <-sig:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP 服务异常退出: %v", err)
			exitCode = 1
		}
	}
	shutdown(shutdownParams{
		timeout: *shutdownTimeout, api: apiSrv, srv: srv, adminSrv: adminSrv, hub: hub, st: st, turn: turnSrv,
		stopBackground: func() { stopBackground(); background.Wait() },
	})
	logOut.Close()
	os.Exit(exitCode)
}

type shutdownParams struct {
	timeout        time.Duration
	api            *api.Server
	srv, adminSrv  *http.Server
	hub            *ws.Hub
	st             *store.Store
	turn           *turn.Server
	stopBackground func()
}

// shutdown 是优雅停机的顺序（A10 / P5）：
//  1. /readyz 开始返回 503，停止接受新连接与新请求，等在途请求处理完；
//  2. 给每条 WebSocket 发关闭帧（1001），等它们的读写协程退出——此后不再有连接协程访问数据库；
//  3. 停掉后台任务（清理、统计落库）；
//  4. 写协程把已经排队的写入提交完，最后关库。
//
// 已被服务端接受的消息都在队列里（提交后才返回成功），客户端重连后照常收到：停机不丢数据。
func shutdown(p shutdownParams) {
	log.Printf("正在关闭（最长等待 %s）...", p.timeout)
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	p.api.SetDraining()
	if p.adminSrv != nil {
		p.adminSrv.Shutdown(ctx)
	}
	if err := p.srv.Shutdown(ctx); err != nil {
		log.Printf("[http] 等待在途请求超时，强制关闭: %v", err)
		p.srv.Close()
	}
	if err := p.hub.Shutdown(ctx); err != nil {
		log.Printf("[ws] 部分连接未在期限内关闭: %v", err)
	}
	p.stopBackground() // 内存里累计的统计增量落库后再关库
	if p.turn != nil {
		p.turn.Close()
	}
	if err := p.st.Close(); err != nil {
		log.Printf("[store] 关闭数据库: %v", err)
	}
	log.Printf("已关闭")
}

// loginStateChecker 返回 TURN 用的「该用户是否仍处于登录状态」判断，结果缓存 5 秒
// （TURN 的每个认证请求都会调用；5 秒是退出登录到凭证失效的最长延迟）。
func loginStateChecker(st *store.Store) func(userID string) bool {
	type entry struct {
		ok bool
		at time.Time
	}
	var mu sync.Mutex
	cache := map[string]entry{}
	return func(userID string) bool {
		now := time.Now()
		mu.Lock()
		e, hit := cache[userID]
		mu.Unlock()
		if hit && now.Sub(e.at) < 5*time.Second {
			return e.ok
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ok, err := st.HasValidToken(ctx, userID)
		cancel()
		if err != nil {
			return hit && e.ok // 查询失败（如正在停机）：沿用上次的结论，没有结论就拒绝
		}
		mu.Lock()
		if len(cache) > 10000 {
			for k, v := range cache {
				if now.Sub(v.at) >= 5*time.Second {
					delete(cache, k)
				}
			}
		}
		cache[userID] = entry{ok: ok, at: now}
		mu.Unlock()
		return ok
	}
}

// startAdmin 启动管理后台监听。端口被占用等启动失败只告警，不影响主服务。
func startAdmin(addr string, a *admin.Server, st *store.Store) *http.Server {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[admin] 警告：管理后台启动失败（主服务继续运行）: %v", err)
		return nil
	}
	if !isLoopbackAddr(addr) {
		log.Printf("[admin] 警告：管理后台监听在非回环地址 %s。请务必放在 TLS 反向代理之后并设置 HUSH_ADMIN_SECURE_COOKIE=1，不要直接暴露到公网", addr)
	}
	if n, err := st.CountAdmins(context.Background()); err == nil && n == 0 {
		log.Printf("[admin] 还没有管理员账号，后台无法登录。创建方法：hush-server admin create --data <dir> --username <u>")
	}
	srv := api.NewHTTPServer(addr, a.Handler(), api.DefaultTimeouts)
	go func() {
		log.Printf("[admin] 管理后台监听 http://%s", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[admin] 管理后台异常退出: %v", err)
		}
	}()
	return srv
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
// 只监听回环地址且没有可信代理时，所有客户端（模拟器经 10.0.2.2、iOS 模拟器、测试机器人）都来自 127.0.0.1，
// 按 IP 限速等于全体共享一个额度，因此放开；监听局域网 / 公网地址，或配置了可信代理（能拿到真实客户端 IP）时保留默认限速。
func authPerIPLimit(addr string, proxies *netx.Proxies) int {
	if isLoopbackAddr(addr) && !proxies.Enabled() {
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
