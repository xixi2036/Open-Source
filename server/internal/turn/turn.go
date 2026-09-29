// Package turn 提供内嵌 TURN/STUN 服务（UDP）与 TURN REST 时限凭证（契约 3.9）。
package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	pionturn "github.com/pion/turn/v4"
)

// Realm 是 TURN 认证域。
const Realm = "hush"

// Credentials 生成 TURN REST 凭证：username="<expiry unix 秒>:<user_id>"，
// credential=base64(HMAC-SHA1(secret, username))。
func Credentials(secret, userID string, ttl time.Duration, now time.Time) (username, credential string) {
	expiry := now.Add(ttl).Unix()
	username = fmt.Sprintf("%d:%s", expiry, userID)
	return username, Sign(secret, username)
}

// Sign 计算 base64(HMAC-SHA1(secret, username))。
func Sign(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// authHandler 校验 REST 凭证：解析过期时间、拒绝过期，按 HMAC 推导长期凭证密钥。
func authHandler(secret string) pionturn.AuthHandler {
	return func(username, realm string, src net.Addr) ([]byte, bool) {
		expStr, _, ok := strings.Cut(username, ":")
		if !ok {
			return nil, false
		}
		exp, err := strconv.ParseInt(expStr, 10, 64)
		if err != nil || time.Now().Unix() > exp {
			log.Printf("[turn] 拒绝过期或非法凭证 %q 来自 %s", username, src)
			return nil, false
		}
		return pionturn.GenerateAuthKey(username, realm, Sign(secret, username)), true
	}
}

// Config 是 TURN 服务配置。
type Config struct {
	Port     int
	PublicIP string
	Secret   string
	// AllowPrivatePeers 允许把回环 / 私有 / 链路本地地址作为中继目标（仅开发模式：双方都在本机或模拟器上）。
	AllowPrivatePeers bool
}

// cgnat 是运营商级 NAT 共享地址段 100.64.0.0/10（RFC 6598），与私有地址同样不应被公网中继访问。
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// PeerAllowed 判断 TURN 客户端能否把 ip 作为中继目标（CreatePermission / ChannelBind）。
// 始终拒绝：未指定地址、组播、广播、0.0.0.0/8。
// 生产模式另外拒绝：回环、私有（10/8、172.16/12、192.168/16、fc00::/7）、链路本地（169.254/16、fe80::/10）、100.64/10，
// 防止 TURN 被用来访问服务端所在内网或本机服务（SSRF）。开发模式允许这些地址，方便本机/模拟器联调。
func PeerAllowed(ip net.IP, allowPrivate bool) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && (v4[0] == 0 || v4.Equal(net.IPv4bcast)) {
		return false
	}
	if allowPrivate {
		return true
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip))
}

func permissionHandler(allowPrivate bool) pionturn.PermissionHandler {
	return func(client net.Addr, peer net.IP) bool {
		if PeerAllowed(peer, allowPrivate) {
			return true
		}
		log.Printf("[turn] 拒绝 %s 中继到 %s（回环/私有/链路本地/组播地址）", client, peer)
		return false
	}
}

// Server 是运行中的 TURN 服务。
type Server struct {
	srv *pionturn.Server
}

// Start 在 0.0.0.0:Port 上启动 UDP TURN 服务，中继地址为 PublicIP。
func Start(cfg Config) (*Server, error) {
	ip := net.ParseIP(cfg.PublicIP)
	if ip == nil {
		return nil, fmt.Errorf("invalid --public-ip %q", cfg.PublicIP)
	}
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", cfg.Port))
	if err != nil {
		return nil, err
	}
	srv, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm:       Realm,
		AuthHandler: authHandler(cfg.Secret),
		PacketConnConfigs: []pionturn.PacketConnConfig{{
			PacketConn:            pc,
			RelayAddressGenerator: &pionturn.RelayAddressGeneratorStatic{RelayAddress: ip, Address: "0.0.0.0"},
			PermissionHandler:     permissionHandler(cfg.AllowPrivatePeers),
		}},
	})
	if err != nil {
		pc.Close()
		return nil, err
	}
	return &Server{srv: srv}, nil
}

// Close 停止服务。
func (s *Server) Close() error { return s.srv.Close() }
