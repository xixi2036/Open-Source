// Package netx 提供「可信反向代理」下的客户端 IP 解析。
package netx

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Proxies 是可信代理的地址段。nil / 空表示没有可信代理：永远以 TCP 对端地址为准，忽略 X-Forwarded-For。
type Proxies struct{ nets []*net.IPNet }

// ParseProxies 解析逗号分隔的 CIDR 或单个 IP（如 "127.0.0.1,10.0.0.0/8,::1"）。空串返回 nil。
func ParseProxies(s string) (*Proxies, error) {
	var p Proxies
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("可信代理地址 %q 无效", part)
			}
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			part = fmt.Sprintf("%s/%d", ip, bits)
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("可信代理地址段 %q 无效", part)
		}
		p.nets = append(p.nets, n)
	}
	if len(p.nets) == 0 {
		return nil, nil
	}
	return &p, nil
}

// Enabled 表示配置了可信代理。
func (p *Proxies) Enabled() bool { return p != nil && len(p.nets) > 0 }

func (p *Proxies) trusted(ip net.IP) bool {
	if p == nil || ip == nil {
		return false
	}
	for _, n := range p.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// RemoteHost 返回 TCP 对端地址里的主机部分。
func RemoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ClientIP 返回请求的客户端 IP。
//
// 只有 TCP 对端是可信代理时才看 X-Forwarded-For：从右往左跳过可信代理，取第一个不可信的地址
// （最右边的条目是离我们最近的代理写的，客户端伪造的内容只能出现在左边）。其他情况一律返回 TCP 对端地址——
// 不经代理直连的客户端带什么头都没用。
func (p *Proxies) ClientIP(r *http.Request) string {
	remote := RemoteHost(r)
	if !p.Enabled() || !p.trusted(net.ParseIP(remote)) {
		return remote
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break // 链条里有无法解析的条目：不再往左信任
		}
		if !p.trusted(ip) {
			return ip.String()
		}
	}
	return remote
}
