package admin

import (
	"errors"
	"log"
	"os"
	"path/filepath"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// loggedOutFrame 是踢下线时发给在线连接的最后一帧（不入队列、不带 id，与重新登录时的 logged_out 一致）。
func loggedOutFrame(reason string) any {
	return obj{"t": "event", "d": obj{"type": "logged_out", "reason": reason}}
}

// kick 在事务提交之后调用：令牌已作废，Kick 递增 epoch，使仍在握手中的旧令牌连接也无法登记。
func (s *Server) kick(uid, reason string) {
	s.hub.Kick(uid, loggedOutFrame(reason), ws.CloseLoggedOut, reason)
}

// GET /admin/api/users?q=&sort=created|active&status=&page=&size=
func (c *ctx) listUsers() {
	page, size := c.page()
	list, total, err := c.s.st.AdminListUsers(c.r.Context(), c.query("q"), c.query("sort"), c.query("status"), page, size)
	if err != nil {
		c.internal(err)
		return
	}
	type row struct {
		store.AdminUser
		Online bool `json:"online"`
	}
	out := make([]row, len(list))
	for i, u := range list {
		out[i] = row{AdminUser: u, Online: c.s.hub.IsOnline(u.ID)}
	}
	c.ok(paged(out, total, page, size))
}

// GET /admin/api/users/{id} → 详情（资料与各类数量；队列只有条数与字节数）
func (c *ctx) getUser() {
	d, err := c.s.st.AdminGetUser(c.r.Context(), c.path("id"))
	if err != nil {
		c.storeErr(err, "用户不存在")
		return
	}
	c.ok(struct {
		store.AdminUserDetail
		Online bool `json:"online"`
	}{d, c.s.hub.IsOnline(d.ID)})
}

// txDisable 在事务内停用账号（作废令牌）。
func txDisable(t *store.Tx, uid string) error { return t.SetUserDisabled(uid, true) }

// POST /admin/api/users/{id}/disable {reason?} → {}
// 停用后：令牌全部作废、verify 返回 403 account_disabled、在线连接收到 logged_out{reason:"account_disabled"} 后以 4001 断开。
func (c *ctx) disableUser() {
	id := c.path("id")
	var req struct {
		Reason string `json:"reason"`
	}
	if !c.decode(&req) {
		return
	}
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		if err := txDisable(t, id); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("user.disable", "user:"+id, jsonDetail(obj{"reason": req.Reason})))
	})
	if err != nil {
		c.storeErr(err, "用户不存在")
		return
	}
	c.s.kick(id, "account_disabled")
	log.Printf("[admin] %s 停用账号 %s", c.admin.Username, id)
	c.ok(obj{})
}

// POST /admin/api/users/{id}/enable → {}
func (c *ctx) enableUser() {
	id := c.path("id")
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		if err := t.SetUserDisabled(id, false); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("user.enable", "user:"+id, ""))
	})
	if err != nil {
		c.storeErr(err, "用户不存在")
		return
	}
	c.ok(obj{})
}

// POST /admin/api/users/{id}/logout → {}（强制下线：作废令牌并断开连接；密钥与队列保留，用户可重新登录）
func (c *ctx) logoutUser() {
	id := c.path("id")
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		if _, err := t.AdminGetUser(id); err != nil {
			return err
		}
		if err := t.RevokeTokens(id); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("user.logout", "user:"+id, ""))
	})
	if err != nil {
		c.storeErr(err, "用户不存在")
		return
	}
	c.s.kick(id, "forced_logout")
	c.ok(obj{})
}

// txResetProfile 在事务内清空违规资料并通知联系人（与用户自己改资料时相同的 profile_updated 事件，另外也发给本人）。
func txResetProfile(t *store.Tx, uid string, name, about, avatar bool) error {
	var up store.ProfileUpdate
	empty := ""
	if name {
		n := "用户" + shortID(uid)
		up.Name = &n
	}
	if about {
		up.About = &empty
	}
	if avatar {
		up.AvatarID = &empty
	}
	p, err := t.UpdateProfile(uid, up)
	if err != nil {
		return err
	}
	contacts, err := t.Contacts(uid)
	if err != nil {
		return err
	}
	return ws.Emit(t, append(contacts, uid), "profile_updated", "profile_updated:"+uid, obj{"profile": p})
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// POST /admin/api/users/{id}/reset-profile {name?, about?, avatar?: bool} → {}
func (c *ctx) resetProfile() {
	id := c.path("id")
	var req struct {
		Name   bool `json:"name"`
		About  bool `json:"about"`
		Avatar bool `json:"avatar"`
	}
	if !c.decode(&req) {
		return
	}
	if !req.Name && !req.About && !req.Avatar {
		c.bad("请至少选择一项要重置的资料")
		return
	}
	var oldAvatar *string
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		u, err := t.AdminGetUser(id)
		if err != nil {
			return err
		}
		oldAvatar = u.AvatarID
		if err := txResetProfile(t, id, req.Name, req.About, req.Avatar); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("user.reset_profile", "user:"+id, jsonDetail(obj{"name": req.Name, "about": req.About, "avatar": req.Avatar})))
	})
	if err != nil {
		c.storeErr(err, "用户不存在")
		return
	}
	if req.Avatar && oldAvatar != nil {
		c.s.purgeMedia(c, []string{*oldAvatar})
	}
	c.ok(obj{})
}

var errConfirm = errors.New("confirm mismatch")

// DELETE /admin/api/users/{id} {confirm_email} → {}
// 一个事务里级联清理并通知：好友收到 friend_removed，所在群的其余成员收到 group_updated（群主自动转让，空群解散）。
func (c *ctx) deleteUser() {
	id := c.path("id")
	var req struct {
		ConfirmEmail string `json:"confirm_email"`
	}
	if !c.decode(&req) {
		return
	}
	var d store.DeletedUser
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		var err error
		if d, err = t.AdminDeleteUser(id); err != nil {
			return err
		}
		if req.ConfirmEmail == "" || req.ConfirmEmail != d.User.Email {
			return errConfirm
		}
		if err := ws.Emit(t, d.Friends, "friend_removed", "friend_removed:"+id, obj{"user_id": id}); err != nil {
			return err
		}
		for _, gid := range d.Groups {
			if err := txBroadcastGroup(t, gid); err != nil {
				return err
			}
		}
		return t.AppendAudit(c.audit("user.delete", "user:"+id, jsonDetail(obj{"email": d.User.Email, "friends": len(d.Friends), "groups": len(d.Groups)})))
	})
	if errors.Is(err, errConfirm) {
		c.bad("确认邮箱与该账号的邮箱不一致")
		return
	}
	if err != nil {
		c.storeErr(err, "用户不存在")
		return
	}
	c.s.kick(id, "account_deleted")
	c.s.purgeMedia(c, d.MediaIDs)
	log.Printf("[admin] %s 删除账号 %s", c.admin.Username, id)
	c.ok(obj{})
}

// txBroadcastGroup 向群的当前全体成员写入 group_updated（与 api 包的 broadcastGroup 相同的事件类型、合并键与负载）。
func txBroadcastGroup(t *store.Tx, gid string) error {
	g, err := t.GetGroup(gid)
	if err != nil {
		return err
	}
	ids := make([]string, len(g.Members))
	for i, m := range g.Members {
		ids[i] = m.UserID
	}
	return ws.Emit(t, ids, "group_updated", "group_updated:"+g.ID, obj{"group": g})
}

// purgeMedia 删除已不再被任何头像或动态引用的明文图片附件（元数据与文件）。失败只记日志：留给定时清理兜底。
func (s *Server) purgeMedia(c *ctx, ids []string) {
	if len(ids) == 0 {
		return
	}
	deleted, err := s.st.DeleteUnreferencedAttachments(c.r.Context(), ids)
	if err != nil {
		log.Printf("[admin] 清理附件元数据失败: %v", err)
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "attachments")
	for _, id := range deleted {
		if err := os.Remove(filepath.Join(dir, filepath.Base(id))); err != nil && !os.IsNotExist(err) {
			log.Printf("[admin] 删除附件文件失败: %v", err)
		}
	}
}
