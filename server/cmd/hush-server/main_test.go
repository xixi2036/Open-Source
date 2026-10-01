package main

import (
	"testing"

	"hush/server/internal/netx"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"127.0.0.2:1":    true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		"LOCALHOST:80":   true,
		":8080":          false,
		"0.0.0.0:8080":   false,
		"[::]:8080":      false,
		"192.168.1.5:80": false,
		"10.0.2.2:8080":  false,
		"example.com:80": false,
		"garbage":        false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, 期望 %v", addr, got, want)
		}
	}
}

func TestResolveDevCode(t *testing.T) {
	if c, w := resolveDevCode("", true, "127.0.0.1:8080"); c != "" || w != "" {
		t.Fatalf("未设置时应无效且无警告: %q %q", c, w)
	}
	if c, w := resolveDevCode("123456", true, "127.0.0.1:8080"); c != "123456" || w != "" {
		t.Fatalf("--dev + 回环应生效: %q %q", c, w)
	}
	if c, w := resolveDevCode("123456", true, "localhost:8080"); c != "123456" || w != "" {
		t.Fatalf("localhost 应生效: %q %q", c, w)
	}
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.2:8080"} {
		if c, w := resolveDevCode("123456", true, addr); c != "" || w == "" {
			t.Fatalf("非回环地址 %s 不应生效且应有警告: %q %q", addr, c, w)
		}
	}
	if c, w := resolveDevCode("123456", false, "127.0.0.1:8080"); c != "" || w == "" {
		t.Fatalf("非 --dev 不应生效且应有警告: %q %q", c, w)
	}
}

func TestAuthPerIPLimit(t *testing.T) {
	if authPerIPLimit("127.0.0.1:8080", nil) == 0 || authPerIPLimit("localhost:8080", nil) == 0 {
		t.Fatal("回环地址应放开按 IP 限速")
	}
	for _, addr := range []string{":8080", "0.0.0.0:8080", "192.168.1.5:8080"} {
		if authPerIPLimit(addr, nil) != 0 {
			t.Fatalf("%s 应使用默认限速", addr)
		}
	}
	// B1：回环监听 + 可信代理（反向代理部署的典型形态）——能拿到真实客户端 IP，限速照常生效
	proxies, err := netx.ParseProxies("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if authPerIPLimit("127.0.0.1:8080", proxies) != 0 {
		t.Fatal("配置了可信代理时不应放开按 IP 限速")
	}
}
