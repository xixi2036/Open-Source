package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
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

// remoteIP 返回对端 IP（不信任 X-Forwarded-For：服务端直接对外，没有反向代理）。
func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

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
	if !s.authCodeLimiter.allow(remoteIP(r), time.Now()) {
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
	if err := s.st.SaveCode(r.Context(), email, code); err != nil {
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
		s.st.ClearCode(r.Context(), email)
		log.Printf("[auth] 验证码发送失败 %s: %s", hmail.MaskEmail(email), strings.ReplaceAll(err.Error(), email, hmail.MaskEmail(email)))
		fail(w, codeInternal, "验证码邮件发送失败")
		return
	}
	log.Printf("[auth] 已发送验证码 -> %s", hmail.MaskEmail(email))
	ok(w, obj{})
}

// POST /v1/auth/verify {email, code, device_name, platform} → {user_id, token, is_new}
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if !s.authVerifyLimiter.allow(remoteIP(r), time.Now()) {
		fail(w, codeRateLimited, "请求过于频繁，请稍后再试")
		return
	}
	var req struct {
		Email      string `json:"email"`
		Code       string `json:"code"`
		DeviceName string `json:"device_name"`
		Platform   string `json:"platform"`
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
	if len(req.DeviceName) > 128 || len(req.Platform) > 32 {
		fail(w, codeBadRequest, "device_name/platform 过长")
		return
	}
	devOK := s.cfg.DevCode != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.cfg.DevCode)) == 1
	if !devOK {
		if err := s.st.CheckCode(r.Context(), email, code); err != nil {
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
	// 登录与发给联系人的 key_changed 在同一事务内写入（outbox）。
	var res store.LoginResult
	err = s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		if res, err = t.Login(email, req.Platform, req.DeviceName, hashToken(token)); err != nil || res.IsNew {
			return err
		}
		contacts, err := t.Contacts(res.User.ID)
		if err != nil {
			return err
		}
		return ws.Emit(t, contacts, "key_changed", "key_changed:"+res.User.ID, obj{"user_id": res.User.ID})
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	uid := res.User.ID
	if !res.IsNew {
		// 单设备：旧连接收到 logged_out 后关闭（不入队列）。
		s.hub.Kick(uid, obj{"t": "event", "d": obj{"type": "logged_out", "reason": "logged_in_elsewhere"}},
			ws.CloseLoggedOut, "logged_in_elsewhere")
	}
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
