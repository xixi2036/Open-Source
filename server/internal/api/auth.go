package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/mail"
	"strings"
	"time"

	hmail "hush/server/internal/mail"
	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// normalizeEmail 去空白并转小写；非法返回 false。
func normalizeEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return "", false
	}
	return s, true
}

// clientIP 返回客户端 IP：默认是 TCP 对端地址；配置了可信代理（--trusted-proxy）且请求确实来自它们时，
// 取它们写的 X-Forwarded-For（见 netx.Proxies.ClientIP）。
func (s *Server) clientIP(r *http.Request) string { return s.cfg.TrustedProxies.ClientIP(r) }

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// POST /v1/auth/code {email} → {}
func (s *Server) handleRequestCode(w http.ResponseWriter, r *http.Request) {
	if !s.authCodeLimiter.allow(s.clientIP(r), time.Now()) {
		fail(w, codeRateLimited, "请求过于频繁，请稍后再试")
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	email, valid := normalizeEmail(req.Email)
	if !valid {
		fail(w, codeBadRequest, "邮箱格式无效")
		return
	}
	if s.cfg.Mailer == nil {
		fail(w, codeInternal, "服务器未配置邮件发送（请配置 HUSH_SMTP_* 或以 --dev 启动）")
		return
	}
	code, err := randomCode()
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ip := s.clientIP(r)
	fresh := code
	// 已有的验证码还有效时重发同一个（见 store.SaveCode）
	code, err = s.st.SaveCode(r.Context(), email, ip, code)
	if err != nil {
		if errors.Is(err, store.ErrRateLimited) {
			fail(w, codeRateLimited, "发送过于频繁，请 60 秒后再试")
			return
		}
		if errors.Is(err, store.ErrLocked) {
			fail(w, codeRateLimited, "验证码错误次数过多，请稍后再试")
			return
		}
		internalErr(w, r, err)
		return
	}
	if err := s.cfg.Mailer.SendCode(email, code); err != nil {
		if code == fresh { // 重发的旧验证码可能已经在用户手里，不能删
			s.st.ClearCode(r.Context(), email)
		}
		log.Printf("[auth] 验证码发送失败 %s: %s", hmail.MaskEmail(email), strings.ReplaceAll(err.Error(), email, hmail.MaskEmail(email)))
		fail(w, codeInternal, "验证码邮件发送失败")
		return
	}
	log.Printf("[auth] 已发送验证码 -> %s", hmail.MaskEmail(email))
	ok(w, obj{})
}

// policyError 是登录事务里因账号状态或注册策略被拒绝的结果（整个事务回滚）。
type policyError struct{ code, msg string }

func (e *policyError) Error() string { return e.code }

// registrationDenied 判断新用户能否注册（邀请码本身是否可用在事务里另行校验）。
func registrationDenied(set *Settings, email, invite string) *policyError {
	if set.RegistrationMode == RegClosed {
		return &policyError{codeRegistrationClosed, "当前不开放注册"}
	}
	if len(set.EmailDomains) > 0 {
		domain := email[strings.LastIndexByte(email, '@')+1:]
		allowed := false
		for _, d := range set.EmailDomains {
			allowed = allowed || d == domain
		}
		if !allowed {
			return &policyError{codeEmailNotAllowed, "该邮箱域名不在允许注册的范围内"}
		}
	}
	if set.RegistrationMode == RegInvite && invite == "" {
		return &policyError{codeInviteRequired, "注册需要邀请码"}
	}
	return nil
}

// POST /v1/auth/verify {email, code, device_name, platform, invite_code?} → {user_id, token, is_new}
//
// 验证码先校验、在登录事务里才消费：账号被停用（403 account_disabled）或注册策略拒绝
// （403 invite_required / invite_invalid / registration_closed / email_not_allowed）时验证码仍然有效，
// 客户端补上邀请码即可用同一个验证码重试。注册策略只约束新用户，老用户登录不受影响。
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !s.authVerifyLimiter.allow(s.clientIP(r), time.Now()) {
		fail(w, codeRateLimited, "请求过于频繁，请稍后再试")
		return
	}
	var req struct {
		Email      string `json:"email"`
		Code       string `json:"code"`
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
		InviteCode string `json:"invite_code"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	email, valid := normalizeEmail(req.Email)
	if !valid {
		fail(w, codeBadRequest, "邮箱格式无效")
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		fail(w, codeBadRequest, "缺少验证码")
		return
	}
	if len(req.DeviceName) > 128 || len(req.Platform) > 32 || len(req.InviteCode) > 64 {
		fail(w, codeBadRequest, "device_name/platform/invite_code 过长")
		return
	}
	invite := store.NormalizeInvite(req.InviteCode)
	ip := s.clientIP(r)
	devOK := s.cfg.DevCode != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.cfg.DevCode)) == 1
	if !devOK {
		if err := s.st.PeekCode(r.Context(), email, ip, code); err != nil {
			if errors.Is(err, store.ErrBadCode) {
				log.Printf("[auth] 验证码错误 %s", hmail.MaskEmail(email))
				fail(w, codeUnauthorized, "验证码错误或已过期")
				return
			}
			if errors.Is(err, store.ErrLocked) {
				log.Printf("[auth] 验证码错误次数过多，已锁定 %s", hmail.MaskEmail(email))
				fail(w, codeRateLimited, "验证码错误次数过多，请稍后再试")
				return
			}
			internalErr(w, r, err)
			return
		}
	}
	token, err := newToken()
	if err != nil {
		internalErr(w, r, err)
		return
	}
	set := s.set()
	// 账号状态与注册策略检查、验证码消费、登录、发给联系人的 key_changed 都在同一事务内（outbox）。
	// 停用检查必须在 Login 之前：Login 会清空已有用户的密钥与队列。
	var res store.LoginResult
	err = s.st.Update(r.Context(), func(t *store.Tx) error {
		exists, disabled, err := t.AccountState(email)
		if err != nil {
			return err
		}
		if disabled {
			return &policyError{codeAccountDisabled, "该账号已被停用"}
		}
		useInvite := false
		if !exists {
			if denied := registrationDenied(set, email, invite); denied != nil {
				return denied
			}
			if useInvite = set.RegistrationMode == RegInvite; useInvite {
				if err := t.ConsumeInvite(invite); err != nil {
					if errors.Is(err, store.ErrInviteInvalid) {
						return &policyError{codeInviteInvalid, "邀请码无效、已过期或已用完"}
					}
					return err
				}
			}
		}
		if !devOK {
			if err := t.ConsumeCode(email, ip, code); err != nil {
				return err
			}
		}
		if res, err = t.Login(email, req.Platform, req.DeviceName, hashToken(token)); err != nil {
			return err
		}
		if res.IsNew {
			if useInvite {
				return t.RecordInviteUse(invite, res.User.ID, email)
			}
			return nil
		}
		// 单设备：旧连接收到 logged_out 后关闭（不入队列）。放在提交回调里：它紧跟着本事务的提交执行，
		// 早于之后任何事务的投递——重新登录之后入队的信封不会再交给旧设备（B6）。
		uid := res.User.ID
		t.OnCommit(func() {
			s.hub.Kick(uid, obj{"t": "event", "d": obj{"type": "logged_out", "reason": "logged_in_elsewhere"}},
				ws.CloseLoggedOut, "logged_in_elsewhere")
		})
		contacts, err := t.Contacts(res.User.ID)
		if err != nil {
			return err
		}
		return ws.Emit(t, contacts, "key_changed", "key_changed:"+res.User.ID, obj{"user_id": res.User.ID})
	})
	var denied *policyError
	switch {
	case errors.As(err, &denied):
		if denied.code == codeInviteInvalid && !devOK {
			// 一个验证码最多试 5 次邀请码，防止拿着有效验证码枚举邀请码
			if err := s.st.BumpCodeAttempt(r.Context(), email, ip); err != nil {
				log.Printf("[auth] 记录邀请码失败次数出错: %v", err)
			}
		}
		log.Printf("[auth] 登录被拒绝 %s: %s", hmail.MaskEmail(email), denied.code)
		fail(w, denied.code, denied.msg)
		return
	case errors.Is(err, store.ErrBadCode): // 验证码已被并发的另一次登录消费
		fail(w, codeUnauthorized, "验证码错误或已过期")
		return
	case err != nil:
		internalErr(w, r, err)
		return
	}
	uid := res.User.ID
	log.Printf("[auth] 登录成功 user=%s email=%s new=%v platform=%s generation=%d",
		uid, hmail.MaskEmail(email), res.IsNew, req.Platform, res.User.DeviceGeneration)
	ok(w, obj{"user_id": uid, "token": token, "is_new": res.IsNew})
}

// POST /v1/auth/logout → {}
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, uid string) {
	if err := s.st.DeleteToken(r.Context(), tokenHash(r)); err != nil {
		internalErr(w, r, err)
		return
	}
	s.hub.Disconnect(uid, ws.CloseLoggedOut, "logged_out")
	log.Printf("[auth] 退出登录 user=%s", uid)
	ok(w, obj{})
}
