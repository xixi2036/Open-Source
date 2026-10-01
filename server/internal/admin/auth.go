package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"hush/server/internal/store"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

// NormalizeUsername 规范化管理员用户名（小写、去空白）并校验：3–32 位的小写字母、数字、下划线、点、连字符。
func NormalizeUsername(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !usernameRe.MatchString(s) {
		return "", errors.New("用户名须为 3–32 位的小写字母、数字、下划线、点或连字符，且以字母或数字开头")
	}
	return s, nil
}

// CheckPassword 校验密码强度：至少 12 个字符、至多 72 字节（bcrypt 的上限）。
func CheckPassword(p string) error {
	if len([]rune(p)) < MinPasswordLen {
		return fmt.Errorf("密码至少 %d 位", MinPasswordLen)
	}
	if len(p) > maxPasswordLen {
		return fmt.Errorf("密码不能超过 %d 字节", maxPasswordLen)
	}
	return nil
}

// HashPassword 生成 bcrypt 哈希。
func HashPassword(p string, cost int) (string, error) {
	if cost <= 0 {
		cost = DefaultBcryptCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), cost)
	return string(h), err
}

const loginFailedMsg = "用户名或密码错误"

// POST /admin/api/login {username, password} → {admin, csrf}
//
// 限速：同一 IP 每分钟 5 次；同一用户名连续失败 10 次锁定 15 分钟（锁定期间即使密码正确也拒绝）。
// 用户名不存在、已停用、密码错误一律返回同样的 401，且都会做一次 bcrypt 比较，避免从响应或耗时探测用户名。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now()
	ip := s.clientIP(r)
	if !sameOrigin(r) {
		fail(w, http.StatusForbidden, "csrf", "请求来源不合法")
		return
	}
	if !s.loginIP.allow(ip, now) {
		fail(w, http.StatusTooManyRequests, "rate_limited", "登录尝试过于频繁，请稍后再试")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "无效的 JSON")
		return
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if username == "" || len(username) > 64 || req.Password == "" || len(req.Password) > 1024 {
		fail(w, http.StatusUnauthorized, "unauthorized", loginFailedMsg)
		return
	}
	const lockedMsg = "连续登录失败次数过多，账号已暂时锁定，请稍后再试"
	a, err := s.st.GetAdminByUsername(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		if s.unknown.locked(username, now) {
			fail(w, http.StatusTooManyRequests, "locked", lockedMsg)
			return
		}
		bcrypt.CompareHashAndPassword(s.dummyHash, []byte(req.Password))
		s.unknown.fail(username, s.cfg.MaxLoginFailures, s.cfg.LockDuration, now)
		fail(w, http.StatusUnauthorized, "unauthorized", loginFailedMsg)
		return
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	if a.LockedUntil > now.UnixMilli() {
		fail(w, http.StatusTooManyRequests, "locked", lockedMsg)
		return
	}
	pwOK := len(req.Password) <= maxPasswordLen && bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(req.Password)) == nil
	if !pwOK || a.Disabled {
		if !pwOK {
			lockedNow, err := s.st.AdminLoginFailed(r.Context(), a.ID, s.cfg.MaxLoginFailures, s.cfg.LockDuration, now.UnixMilli())
			if err != nil {
				internal(w, r, err)
				return
			}
			if lockedNow {
				log.Printf("[admin] 管理员 %s 连续登录失败，已锁定 %s（来源 %s）", a.Username, s.cfg.LockDuration, ip)
				s.st.AppendAudit(r.Context(), store.AuditEntry{TS: now.UnixMilli(), AdminID: a.ID, AdminName: a.Username, Action: "auth.locked", Target: "admin:" + a.ID, IP: ip})
			}
		}
		fail(w, http.StatusUnauthorized, "unauthorized", loginFailedMsg)
		return
	}
	token, err := newSessionToken()
	if err != nil {
		internal(w, r, err)
		return
	}
	if err := s.st.AdminLoginOK(r.Context(), a.ID, hashToken(token), ip, now.UnixMilli()); err != nil {
		internal(w, r, err)
		return
	}
	s.st.PruneAdminSessions(r.Context(), now.Add(-s.cfg.SessionIdle).UnixMilli())
	s.st.AppendAudit(r.Context(), store.AuditEntry{TS: now.UnixMilli(), AdminID: a.ID, AdminName: a.Username, Action: "auth.login", Target: "admin:" + a.ID, IP: ip})
	s.setCookie(w, r, token)
	log.Printf("[admin] 管理员 %s 登录（来源 %s）", a.Username, ip)
	a.LastLoginAt = now.UnixMilli()
	writeJSON(w, http.StatusOK, obj{"admin": a, "csrf": csrfToken(token), "version": s.cfg.Version})
}

// GET /admin/api/me → {admin, csrf, version}（页面刷新后用它取回 CSRF 令牌）
func (c *ctx) me() {
	c.ok(obj{"admin": c.admin, "csrf": csrfToken(c.token), "version": c.s.cfg.Version})
}

// POST /admin/api/logout → {}
func (c *ctx) logout() {
	if err := c.s.st.DeleteAdminSession(c.r.Context(), hashToken(c.token)); err != nil {
		c.internal(err)
		return
	}
	c.writeAudit("auth.logout", "admin:"+c.admin.ID, "")
	c.s.clearCookie(c.w, c.r)
	c.ok(obj{})
}

// POST /admin/api/me/password {old_password, new_password} → {}（修改后全部会话失效，需重新登录）
func (c *ctx) changeOwnPassword() {
	var req struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if !c.decode(&req) {
		return
	}
	if len(req.Old) > maxPasswordLen || bcrypt.CompareHashAndPassword([]byte(c.admin.PasswordHash), []byte(req.Old)) != nil {
		fail(c.w, http.StatusForbidden, "forbidden", "原密码不正确")
		return
	}
	if err := CheckPassword(req.New); err != nil {
		c.bad(err.Error())
		return
	}
	hash, err := HashPassword(req.New, c.s.cfg.BcryptCost)
	if err != nil {
		c.internal(err)
		return
	}
	if err := c.s.st.UpdateAdmin(c.r.Context(), c.admin.ID, store.AdminUpdate{PasswordHash: &hash}, c.audit("admin.password", "admin:"+c.admin.ID, `{"self":true}`)); err != nil {
		c.internal(err)
		return
	}
	c.s.clearCookie(c.w, c.r)
	c.ok(obj{})
}

// ---------- A16 管理员管理 ----------

// GET /admin/api/admins → [Admin]
func (c *ctx) listAdmins() {
	list, err := c.s.st.ListAdmins(c.r.Context())
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(list)
}

// POST /admin/api/admins {username, password, role} → Admin
func (c *ctx) createAdmin() {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !c.decode(&req) {
		return
	}
	username, err := NormalizeUsername(req.Username)
	if err != nil {
		c.bad(err.Error())
		return
	}
	if !store.ValidAdminRole(req.Role) {
		c.bad("角色须为 owner / moderator / viewer")
		return
	}
	if err := CheckPassword(req.Password); err != nil {
		c.bad(err.Error())
		return
	}
	hash, err := HashPassword(req.Password, c.s.cfg.BcryptCost)
	if err != nil {
		c.internal(err)
		return
	}
	a, err := c.s.st.CreateAdmin(c.r.Context(), username, hash, req.Role)
	if errors.Is(err, store.ErrConflict) {
		c.conflict("用户名已存在")
		return
	}
	if err != nil {
		c.internal(err)
		return
	}
	c.writeAudit("admin.create", "admin:"+a.ID, jsonDetail(obj{"username": username, "role": req.Role}))
	c.ok(a)
}

// PUT /admin/api/admins/{id} {role?, disabled?, password?} → Admin
// 任何修改都会让该管理员的会话全部失效；不能让系统失去最后一个可用的 owner。
func (c *ctx) updateAdmin() {
	id := c.path("id")
	var req struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if !c.decode(&req) {
		return
	}
	if req.Role == nil && req.Disabled == nil && req.Password == nil {
		c.bad("没有要修改的内容")
		return
	}
	up := store.AdminUpdate{Role: req.Role, Disabled: req.Disabled}
	detail := obj{}
	if req.Role != nil {
		if !store.ValidAdminRole(*req.Role) {
			c.bad("角色须为 owner / moderator / viewer")
			return
		}
		detail["role"] = *req.Role
	}
	if req.Disabled != nil {
		detail["disabled"] = *req.Disabled
	}
	if req.Password != nil {
		if err := CheckPassword(*req.Password); err != nil {
			c.bad(err.Error())
			return
		}
		hash, err := HashPassword(*req.Password, c.s.cfg.BcryptCost)
		if err != nil {
			c.internal(err)
			return
		}
		up.PasswordHash = &hash
		detail["password_reset"] = true // 密码本身不进审计
	}
	err := c.s.st.UpdateAdmin(c.r.Context(), id, up, c.audit("admin.update", "admin:"+id, jsonDetail(detail)))
	if errors.Is(err, store.ErrLastOwner) {
		c.conflict("不能停用或降级最后一个可用的 owner")
		return
	}
	if err != nil {
		c.storeErr(err, "管理员不存在")
		return
	}
	a, err := c.s.st.GetAdmin(c.r.Context(), id)
	if err != nil {
		c.storeErr(err, "管理员不存在")
		return
	}
	c.ok(a)
}

// DELETE /admin/api/admins/{id} → {}
func (c *ctx) deleteAdmin() {
	id := c.path("id")
	if id == c.admin.ID {
		c.bad("不能删除自己，请让另一位 owner 操作")
		return
	}
	target, err := c.s.st.GetAdmin(c.r.Context(), id)
	if err != nil {
		c.storeErr(err, "管理员不存在")
		return
	}
	err = c.s.st.DeleteAdmin(c.r.Context(), id, c.audit("admin.delete", "admin:"+id, jsonDetail(obj{"username": target.Username, "role": target.Role})))
	if errors.Is(err, store.ErrLastOwner) {
		c.conflict("不能删除最后一个可用的 owner")
		return
	}
	if err != nil {
		c.storeErr(err, "管理员不存在")
		return
	}
	c.ok(obj{})
}

// GET /admin/api/audit?admin=&action=&from=&to=&page=&size= → 分页的审计日志（只读，没有删除接口）
func (c *ctx) listAudit() {
	page, size := c.page()
	f := store.AuditFilter{Admin: c.query("admin"), Action: c.query("action")}
	fmt.Sscan(c.query("from"), &f.From)
	fmt.Sscan(c.query("to"), &f.To)
	list, total, err := c.s.st.ListAudit(c.r.Context(), f, page, size)
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(paged(list, total, page, size))
}
