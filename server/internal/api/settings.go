package api

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 注册模式（管理后台 A11）。
const (
	RegOpen   = "open"   // 开放注册（默认，现有客户端行为不变）
	RegInvite = "invite" // 新用户需要邀请码
	RegClosed = "closed" // 不接受新用户
)

// Settings 是可以在线修改的运行参数（管理后台 A13）。启动参数 / Config 提供默认值，
// settings 表里的值覆盖它；修改后对之后的请求立即生效，不需要重启。
type Settings struct {
	RegistrationMode string
	EmailDomains     []string // 新用户邮箱域名白名单（小写）；空表示不限

	ClaimPerMinute           int
	WritesPerMinute          int
	AuthCodePerIPPerMinute   int
	AuthVerifyPerIPPerMinute int
	ReportsPerHour           int
	SendOlmPerMinute         int   // 每用户每分钟可发的单聊信封数（按接收方条目计）
	SendGroupPerMinute       int   // 每用户每分钟可发的群消息数
	SendBytesPerMinute       int64 // 每用户每分钟可发的消息正文字节
	SearchPerMinute          int   // 每用户每分钟按邮箱搜索的次数
	MomentsPerDay            int   // 每用户 24 小时内可发布的朋友圈条数

	QueueMaxItems       int64
	QueueMaxBytes       int64
	SenderQueueMaxItems int64 // 单个发送方在某个接收方队列里最多占多少条
	SenderQueueMaxBytes int64
	DailyUploadBytes    int64

	TokenTTL   time.Duration // 0 = 永不过期
	BackupKeep int           // 在线备份保留最近多少份
}

// SettingField 描述一个设置项（供后台渲染表单与校验）。
type SettingField struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Group string   `json:"group"`
	Kind  string   `json:"kind"` // int | bytes | hours | enum | list
	Min   int64    `json:"min"`
	Max   int64    `json:"max"`
	Enum  []string `json:"enum,omitempty"`
	Help  string   `json:"help,omitempty"`
}

// SettingFields 是全部设置项，顺序即后台展示顺序。
var SettingFields = []SettingField{
	{Key: "registration_mode", Label: "注册模式", Group: "注册", Kind: "enum", Enum: []string{RegOpen, RegInvite, RegClosed}, Help: "open 开放 / invite 仅邀请码 / closed 关闭；只影响新用户，老用户登录不受影响"},
	{Key: "email_domains", Label: "邮箱域名白名单", Group: "注册", Kind: "list", Help: "逗号分隔，如 example.com,corp.cn；留空表示不限。只限制新用户注册"},
	{Key: "auth_code_per_ip_per_minute", Label: "申请验证码（每 IP 每分钟）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "auth_verify_per_ip_per_minute", Label: "校验验证码（每 IP 每分钟）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "writes_per_minute", Label: "写操作（每用户每类每分钟）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "claim_per_minute", Label: "领取一次性密钥（每对用户每分钟）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "reports_per_hour", Label: "举报（每用户每小时）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "send_olm_per_minute", Label: "单聊发送（每用户每分钟，按接收方条目计）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 30, Help: "一次批量发送（如给 500 人群分发会话密钥）按条目数计"},
	{Key: "send_group_per_minute", Label: "群消息发送（每用户每分钟）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 30},
	{Key: "send_bytes_per_minute", Label: "消息正文（每用户每分钟，字节）", Group: "限速", Kind: "bytes", Min: 1 << 20, Max: 1 << 40},
	{Key: "search_per_minute", Label: "按邮箱搜索用户（每用户每分钟）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "moments_per_day", Label: "发布朋友圈（每用户 24 小时）", Group: "限速", Kind: "int", Min: 1, Max: 1 << 20},
	{Key: "queue_max_items", Label: "离线队列上限（信封条数）", Group: "配额", Kind: "int", Min: 1, Max: 10_000_000},
	{Key: "queue_max_bytes", Label: "离线队列上限（信封字节）", Group: "配额", Kind: "bytes", Min: 1 << 20, Max: 1 << 40},
	{Key: "sender_queue_max_items", Label: "单个发送方在接收方队列里的上限（条）", Group: "配额", Kind: "int", Min: 1, Max: 10_000_000, Help: "一个人灌满了也只影响他自己发来的消息"},
	{Key: "sender_queue_max_bytes", Label: "单个发送方在接收方队列里的上限（字节）", Group: "配额", Kind: "bytes", Min: 1 << 20, Max: 1 << 40},
	{Key: "daily_upload_bytes", Label: "每用户 24 小时上传上限（字节）", Group: "配额", Kind: "bytes", Min: 1 << 20, Max: 1 << 44},
	{Key: "token_ttl_hours", Label: "登录令牌有效期（小时）", Group: "令牌", Kind: "hours", Min: 0, Max: 24 * 3650, Help: "连续这么久未使用即失效；0 表示永不过期"},
	{Key: "backup_keep", Label: "保留最近几份备份", Group: "维护", Kind: "int", Min: 1, Max: 365},
}

// Map 把设置序列化为 settings 表的键值形式。
func (s Settings) Map() map[string]string {
	return map[string]string{
		"registration_mode":             s.RegistrationMode,
		"email_domains":                 strings.Join(s.EmailDomains, ","),
		"claim_per_minute":              strconv.Itoa(s.ClaimPerMinute),
		"writes_per_minute":             strconv.Itoa(s.WritesPerMinute),
		"auth_code_per_ip_per_minute":   strconv.Itoa(s.AuthCodePerIPPerMinute),
		"auth_verify_per_ip_per_minute": strconv.Itoa(s.AuthVerifyPerIPPerMinute),
		"reports_per_hour":              strconv.Itoa(s.ReportsPerHour),
		"send_olm_per_minute":           strconv.Itoa(s.SendOlmPerMinute),
		"send_group_per_minute":         strconv.Itoa(s.SendGroupPerMinute),
		"send_bytes_per_minute":         strconv.FormatInt(s.SendBytesPerMinute, 10),
		"search_per_minute":             strconv.Itoa(s.SearchPerMinute),
		"moments_per_day":               strconv.Itoa(s.MomentsPerDay),
		"sender_queue_max_items":        strconv.FormatInt(s.SenderQueueMaxItems, 10),
		"sender_queue_max_bytes":        strconv.FormatInt(s.SenderQueueMaxBytes, 10),
		"queue_max_items":               strconv.FormatInt(s.QueueMaxItems, 10),
		"queue_max_bytes":               strconv.FormatInt(s.QueueMaxBytes, 10),
		"daily_upload_bytes":            strconv.FormatInt(s.DailyUploadBytes, 10),
		"token_ttl_hours":               strconv.FormatInt(int64(s.TokenTTL/time.Hour), 10),
		"backup_keep":                   strconv.Itoa(s.BackupKeep),
	}
}

// parseDomains 解析逗号 / 空白分隔的域名列表：转小写、去掉前导 @、去重、排序。
func parseDomains(v string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, d := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
		d = strings.TrimPrefix(strings.ToLower(d), "@")
		if d == "" || seen[d] {
			continue
		}
		if len(d) > 253 || !strings.Contains(d, ".") || strings.ContainsAny(d, "@/\\\"'<>") {
			return nil, fmt.Errorf("域名 %q 无效", d)
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// WithOverrides 在 s 的基础上应用键值覆盖并校验；未知的键或越界的值返回错误。
func (s Settings) WithOverrides(kv map[string]string) (Settings, error) {
	fields := map[string]SettingField{}
	for _, f := range SettingFields {
		fields[f.Key] = f
	}
	for k, v := range kv {
		f, known := fields[k]
		if !known {
			return s, fmt.Errorf("未知的设置项 %q", k)
		}
		v = strings.TrimSpace(v)
		switch f.Kind {
		case "enum":
			valid := false
			for _, e := range f.Enum {
				valid = valid || e == v
			}
			if !valid {
				return s, fmt.Errorf("%s 的取值无效：%q", f.Label, v)
			}
			s.RegistrationMode = v
		case "list":
			d, err := parseDomains(v)
			if err != nil {
				return s, err
			}
			s.EmailDomains = d
		default:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < f.Min || n > f.Max {
				return s, fmt.Errorf("%s 须为 %d–%d 之间的整数", f.Label, f.Min, f.Max)
			}
			switch k {
			case "claim_per_minute":
				s.ClaimPerMinute = int(n)
			case "writes_per_minute":
				s.WritesPerMinute = int(n)
			case "auth_code_per_ip_per_minute":
				s.AuthCodePerIPPerMinute = int(n)
			case "auth_verify_per_ip_per_minute":
				s.AuthVerifyPerIPPerMinute = int(n)
			case "reports_per_hour":
				s.ReportsPerHour = int(n)
			case "send_olm_per_minute":
				s.SendOlmPerMinute = int(n)
			case "send_group_per_minute":
				s.SendGroupPerMinute = int(n)
			case "send_bytes_per_minute":
				s.SendBytesPerMinute = n
			case "search_per_minute":
				s.SearchPerMinute = int(n)
			case "moments_per_day":
				s.MomentsPerDay = int(n)
			case "sender_queue_max_items":
				s.SenderQueueMaxItems = n
			case "sender_queue_max_bytes":
				s.SenderQueueMaxBytes = n
			case "queue_max_items":
				s.QueueMaxItems = n
			case "queue_max_bytes":
				s.QueueMaxBytes = n
			case "daily_upload_bytes":
				s.DailyUploadBytes = n
			case "token_ttl_hours":
				s.TokenTTL = time.Duration(n) * time.Hour
			case "backup_keep":
				s.BackupKeep = int(n)
			}
		}
	}
	return s, nil
}

// set 返回当前生效的设置（只读快照）。
func (s *Server) set() *Settings { return s.live.Load() }

// Settings 返回当前生效的设置。
func (s *Server) Settings() Settings { return *s.live.Load() }

// Defaults 返回由启动参数决定的默认设置（后台「恢复默认」用）。
func (s *Server) Defaults() Settings { return s.defaults }

// ApplySettings 让新设置立即生效：之后的请求读到新的配额 / 有效期，限速器换用新的上限。
func (s *Server) ApplySettings(v Settings) {
	s.live.Store(&v)
	s.claimLimiter.setLimit(int64(v.ClaimPerMinute))
	s.writeLimiter.setLimit(int64(v.WritesPerMinute))
	s.authCodeLimiter.setLimit(int64(v.AuthCodePerIPPerMinute))
	s.authVerifyLimiter.setLimit(int64(v.AuthVerifyPerIPPerMinute))
	s.reportLimiter.setLimit(int64(v.ReportsPerHour))
	s.sendOlmLimiter.setLimit(int64(v.SendOlmPerMinute))
	s.sendGroupLimiter.setLimit(int64(v.SendGroupPerMinute))
	s.sendBytesLimiter.setLimit(v.SendBytesPerMinute)
	s.searchLimiter.setLimit(int64(v.SearchPerMinute))
}

// LoadSettings 读取 settings 表并覆盖默认值（启动时调用）。表里有无法识别或越界的值时跳过该项并返回说明。
func (s *Server) LoadSettings(ctx context.Context) (skipped []string, err error) {
	kv, err := s.st.Settings(ctx)
	if err != nil {
		return nil, err
	}
	cur := s.defaults
	for k, v := range kv {
		next, e := cur.WithOverrides(map[string]string{k: v})
		if e != nil {
			skipped = append(skipped, e.Error())
			continue
		}
		cur = next
	}
	sort.Strings(skipped)
	s.ApplySettings(cur)
	return skipped, nil
}
