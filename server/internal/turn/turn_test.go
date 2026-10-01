package turn

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	pionturn "github.com/pion/turn/v4"
)

func TestCredentialsFormat(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	user, cred := Credentials("s3cret", "user-1", time.Hour, now)
	if user != fmt.Sprintf("%d:user-1", now.Add(time.Hour).Unix()) {
		t.Fatalf("username 格式错误: %s", user)
	}
	if cred != Sign("s3cret", user) || !strings.HasSuffix(cred, "=") {
		t.Fatalf("credential 应为标准 base64（带填充）的 HMAC-SHA1: %s", cred)
	}
}

func TestAuthHandler(t *testing.T) {
	h := authHandler(Config{Secret: "s3cret"})
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}

	user, cred := Credentials("s3cret", "u", time.Hour, time.Now())
	key, ok := h(user, Realm, src)
	if !ok || string(key) != string(pionturn.GenerateAuthKey(user, Realm, cred)) {
		t.Fatal("有效凭证应通过，且密钥由 credential 推导")
	}
	expired, _ := Credentials("s3cret", "u", -time.Minute, time.Now())
	if _, ok := h(expired, Realm, src); ok {
		t.Fatal("过期凭证应被拒绝")
	}
	if _, ok := h("garbage", Realm, src); ok {
		t.Fatal("非法 username 应被拒绝")
	}
}

func TestPeerAllowed(t *testing.T) {
	cases := []struct {
		ip        string
		prod, dev bool
	}{
		{"8.8.8.8", true, true},
		{"2001:4860:4860::8888", true, true},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.0.2.15", false, true},
		{"172.16.3.4", false, true},
		{"192.168.1.20", false, true},
		{"169.254.169.254", false, true}, // 云厂商元数据地址
		{"fe80::1", false, true},
		{"fd00::1", false, true},
		{"100.64.1.1", false, true},
		{"::ffff:127.0.0.1", false, true},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"0.1.2.3", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
		{"255.255.255.255", false, false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := PeerAllowed(ip, false); got != c.prod {
			t.Errorf("生产模式 %s: 得到 %v，期望 %v", c.ip, got, c.prod)
		}
		if got := PeerAllowed(ip, true); got != c.dev {
			t.Errorf("开发模式 %s: 得到 %v，期望 %v", c.ip, got, c.dev)
		}
	}
	if PeerAllowed(nil, true) {
		t.Error("nil 应拒绝")
	}
}

// relayTo 经真实 TURN 服务分配中继，向 peer 发一个包，返回 peer 是否收到。
func relayTo(t *testing.T, allowPrivate bool) bool {
	t.Helper()
	probe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	srv, err := Start(Config{Port: port, PublicIP: "127.0.0.1", Secret: "s", AllowPrivatePeers: allowPrivate})
	if err != nil {
		t.Skipf("无法启动 TURN: %v", err)
	}
	defer srv.Close()

	peer, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer peer.Close()
	conn, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer conn.Close()
	user, cred := Credentials("s", "u", time.Hour, time.Now())
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	c, err := pionturn.NewClient(&pionturn.ClientConfig{STUNServerAddr: addr, TURNServerAddr: addr, Username: user, Password: cred, Realm: Realm, Conn: conn})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	relay, err := c.Allocate()
	if err != nil {
		t.Fatalf("分配中继失败: %v", err)
	}
	defer relay.Close()
	_, _ = relay.WriteTo([]byte("hello"), peer.LocalAddr())
	peer.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 64)
	n, _, err := peer.ReadFrom(buf)
	return err == nil && string(buf[:n]) == "hello"
}

func TestRelayToLoopbackOnlyInDev(t *testing.T) {
	if !relayTo(t, true) {
		t.Fatal("开发模式应允许中继到回环地址")
	}
	if relayTo(t, false) {
		t.Fatal("生产模式应拒绝中继到回环地址")
	}
}

// B10：凭证与登录态绑定——用户失去登录态后，没过期的凭证也被拒绝；非法 username 不原样写进日志。
func TestAuthBoundToLoginState(t *testing.T) {
	loggedIn := map[string]bool{"alice": true}
	h := authHandler(Config{Secret: "s3cret", Validate: func(uid string) bool { return loggedIn[uid] }})
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	alice, _ := Credentials("s3cret", "alice", time.Hour, time.Now())
	if _, ok := h(alice, Realm, src); !ok {
		t.Fatal("登录中的用户应通过")
	}
	loggedIn["alice"] = false
	if _, ok := h(alice, Realm, src); ok {
		t.Fatal("退出登录后凭证应立即失效")
	}
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	evil := "evil\n2026/01/01 [auth] 登录成功 user=admin"
	if _, ok := h(evil, Realm, src); ok {
		t.Fatal("非法 username 应被拒绝")
	}
	if strings.Contains(buf.String(), "登录成功") || strings.Contains(buf.String(), "evil") {
		t.Fatalf("日志里不应出现 username 原文: %q", buf.String())
	}
}

// B10：每用户中继分配数上限。
func TestAllocationQuotaPerUser(t *testing.T) {
	a := &allocations{max: 2, n: map[string]int{}}
	src := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	u1, _ := Credentials("s", "u1", time.Hour, time.Now())
	u1b, _ := Credentials("s", "u1", 2*time.Hour, time.Now()) // 同一用户的另一份凭证
	u2, _ := Credentials("s", "u2", time.Hour, time.Now())
	for i, name := range []string{u1, u1b} {
		if !a.quota(name, Realm, src) {
			t.Fatalf("第 %d 个分配应允许", i+1)
		}
		a.created(name)
	}
	if a.quota(u1, Realm, src) {
		t.Fatal("同一用户（不论用哪份凭证）超过上限应拒绝")
	}
	if !a.quota(u2, Realm, src) {
		t.Fatal("其他用户不受影响")
	}
	a.deleted(u1)
	if !a.quota(u1b, Realm, src) {
		t.Fatal("释放后应可再分配")
	}
	if a.quota("garbage", Realm, src) {
		t.Fatal("非法 username 不给分配")
	}
}
