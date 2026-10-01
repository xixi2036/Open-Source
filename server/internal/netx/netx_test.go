package netx

import (
	"net/http"
	"testing"
)

func req(remote string, xff ...string) *http.Request {
	r, _ := http.NewRequest("GET", "http://x/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	p, err := ParseProxies("127.0.0.1, 10.0.0.0/8,::1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		p    *Proxies
		r    *http.Request
		want string
	}{
		{"没有配置可信代理时忽略 XFF", nil, req("203.0.113.5:1234", "1.2.3.4"), "203.0.113.5"},
		{"对端不是可信代理时忽略 XFF（直连的客户端伪造头）", p, req("203.0.113.5:1234", "1.2.3.4"), "203.0.113.5"},
		{"经可信代理：取 XFF", p, req("127.0.0.1:1234", "198.51.100.7"), "198.51.100.7"},
		{"多级代理：从右往左跳过可信代理", p, req("10.0.0.2:1234", "198.51.100.7, 10.0.0.9"), "198.51.100.7"},
		{"客户端伪造的左侧条目不被采信", p, req("127.0.0.1:1", "6.6.6.6, 198.51.100.7"), "198.51.100.7"},
		{"多个 XFF 头按顺序拼接", p, req("127.0.0.1:1", "6.6.6.6", "198.51.100.7, 10.1.1.1"), "198.51.100.7"},
		{"XFF 全是可信代理：退回对端地址", p, req("127.0.0.1:1", "10.0.0.1"), "127.0.0.1"},
		{"没有 XFF：对端地址", p, req("[::1]:99"), "::1"},
		{"无法解析的条目截断信任链", p, req("127.0.0.1:1", "198.51.100.7, garbage"), "127.0.0.1"},
	} {
		if got := c.p.ClientIP(c.r); got != c.want {
			t.Errorf("%s: 得到 %q，期望 %q", c.name, got, c.want)
		}
	}
	if _, err := ParseProxies("10.0.0.0/33"); err == nil {
		t.Error("无效的 CIDR 应报错")
	}
	if p, _ := ParseProxies("  "); p.Enabled() {
		t.Error("空串不应启用")
	}
}
