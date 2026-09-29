package turn

import (
	"fmt"
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
	h := authHandler("s3cret")
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
