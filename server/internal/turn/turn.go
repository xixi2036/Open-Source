// Package turn 提供内嵌 TURN/STUN 服务（UDP）与 TURN REST 时限凭证（契约 3.9）。
package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	pionturn "github.com/pion/turn/v4"

	"hush/server/internal/logx"
)

// Realm 是 TURN 认证域。
const Realm = "hush"

// DefaultMaxAllocationsPerUser 是每个用户同时持有的中继分配数的默认上限。
const DefaultMaxAllocationsPerUser = 8

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

// userOf 从 REST 凭证的 username（"<过期秒>:<user_id>"）里取出过期时间与用户 id。
func userOf(username string) (exp int64, userID string, ok bool) {
	expStr, uid, found := strings.Cut(username, ":")
	if !found || uid == "" {
		return 0, "", false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return exp, uid, true
}

// authHandler 校验 REST 凭证：解析过期时间、拒绝过期，确认该用户此刻仍处于登录状态，再按 HMAC 推导长期凭证密钥。
// 被拒绝时日志里不写 username 原文：它来自网络上任何人发来的 UDP 包，可以是任意内容。
func authHandler(cfg Config) pionturn.AuthHandler {
	return func(username, realm string, src net.Addr) ([]byte, bool) {
		exp, uid, ok := userOf(username)
		if !ok || time.Now().Unix() > exp {
			logx.Infof("[turn] 拒绝过期或非法凭证（username 长度 %d）来自 %s", len(username), src)
			return nil, false
		}
		// 凭证与登录态绑定：退出登录、在别处重新登录之前被停用、被强制下线的用户，手里没过期的凭证也立即失效
		if cfg.Validate != nil && !cfg.Validate(uid) {
			logx.Infof("[turn] 拒绝已失去登录态的用户的凭证，来自 %s", src)
			return nil, false
		}
		return pionturn.GenerateAuthKey(username, realm, Sign(cfg.Secret, username)), true
	}
}

// allocations 统计每个用户当前的中继分配数，用于每用户上限。
type allocations struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

func (a *allocations) quota(username, _ string, src net.Addr) bool {
	_, uid, ok := userOf(username)
	if !ok {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.max > 0 && a.n[uid] >= a.max {
		logx.Infof("[turn] 用户的中继分配数已达上限 %d，拒绝来自 %s 的新分配", a.max, src)
		return false
	}
	return true
}

func (a *allocations) created(username string) {
	if _, uid, ok := userOf(username); ok {
		a.mu.Lock()
		a.n[uid]++
		a.mu.Unlock()
	}
}

func (a *allocations) deleted(username string) {
	if _, uid, ok := userOf(username); ok {
		a.mu.Lock()
		if a.n[uid]--; a.n[uid] <= 0 {
			delete(a.n, uid)
		}
		a.mu.Unlock()
	}
}

// Config 是 TURN 服务配置。
type Config struct {
	Port     int
	PublicIP string
	Secret   string
	// Validate 判断用户此刻是否仍处于登录状态（有未作废的令牌、账号未停用）；nil 表示不检查。
	// 每次 TURN 认证（Allocate / Refresh / CreatePermission / ChannelBind）都会调用，调用方应自带短时缓存。
	Validate func(userID string) bool
	// MaxAllocationsPerUser 是每个用户同时持有的中继分配数上限（一次通话用 1–2 个）；0 取默认值 8。
	MaxAllocationsPerUser int
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
		logx.Infof("[turn] 拒绝 %s 中继到 %s（回环/私有/链路本地/组播地址）", client, peer)
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
	if cfg.MaxAllocationsPerUser <= 0 {
		cfg.MaxAllocationsPerUser = DefaultMaxAllocationsPerUser
	}
	allocs := &allocations{max: cfg.MaxAllocationsPerUser, n: map[string]int{}}
	srv, err := pionturn.NewServer(pionturn.ServerConfig{
		Realm:        Realm,
		AuthHandler:  authHandler(cfg),
		QuotaHandler: allocs.quota,
		EventHandler: pionturn.EventHandler{
			OnAllocationCreated: func(_, _ net.Addr, _, username, _ string, _ net.Addr, _ int) { allocs.created(username) },
			OnAllocationDeleted: func(_, _ net.Addr, _, username, _ string) { allocs.deleted(username) },
		},
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
