package api

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

const maxGroupMembers = 500

func isAdminRole(role string) bool { return role == store.RoleOwner || role == store.RoleAdmin }

// checkFriends 校验 ids 均为 uid 的好友，返回去重后（排除 uid 本人）的列表。
func (s *Server) checkFriends(w http.ResponseWriter, r *http.Request, uid string, ids []string) ([]string, bool) {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id == "" || id == uid || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	friends, err := s.st.FriendsAmong(r.Context(), uid, out)
	if err != nil {
		internalErr(w, r, err)
		return nil, false
	}
	for _, id := range out {
		if !friends[id] {
			fail(w, codeNotFriends, "只能添加你的好友入群: "+id)
			return nil, false
		}
	}
	return out, true
}

// broadcastGroup 在事务 t 内读取群的最新状态，向当前全体成员写入 group_updated，并返回该群。
func broadcastGroup(t *store.Tx, gid string) (store.Group, error) {
	g, err := t.GetGroup(gid)
	if err != nil {
		return g, err
	}
	ids := make([]string, len(g.Members))
	for i, m := range g.Members {
		ids[i] = m.UserID
	}
	return g, ws.Emit(t, ids, "group_updated", "group_updated:"+g.ID, obj{"group": g})
}

// POST /v1/groups {name, member_ids} → Group
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "group") {
		return
	}
	var req struct {
		Name      string   `json:"name"`
		MemberIDs []string `json:"member_ids"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validName(req.Name) {
		fail(w, codeBadRequest, "群名长度须为 1-64 个字符")
		return
	}
	members, valid := s.checkFriends(w, r, uid, req.MemberIDs)
	if !valid {
		return
	}
	if len(members)+1 > maxGroupMembers {
		fail(w, codeBadRequest, "群成员过多")
		return
	}
	var g store.Group
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		created, err := t.CreateGroup(uid, req.Name, members)
		if err != nil {
			return err
		}
		g, err = broadcastGroup(t, created.ID)
		return err
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	log.Printf("[group] %s 创建群 %s（%d 人）", uid, g.ID, len(g.Members))
	ok(w, g)
}

// GET /v1/groups → [Group]
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request, uid string) {
	out, err := s.st.UserGroups(r.Context(), uid)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, out)
}

// GET /v1/groups/{id} → Group
func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request, uid string) {
	gid := r.PathValue("id")
	if _, valid := s.requireMember(w, r, gid, uid); !valid {
		return
	}
	g, err := s.st.GetGroup(r.Context(), gid)
	if err != nil {
		storeErr(w, r, err, "群不存在")
		return
	}
	ok(w, g)
}

// PUT /v1/groups/{id} {name?, avatar_id?, announcement?} → Group（管理员）
func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "group") {
		return
	}
	gid := r.PathValue("id")
	var req struct {
		Name         *string `json:"name"`
		AvatarID     *string `json:"avatar_id"`
		Announcement *string `json:"announcement"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	role, valid := s.requireMember(w, r, gid, uid)
	if !valid {
		return
	}
	if !isAdminRole(role) {
		fail(w, codeForbidden, "仅管理员可修改群资料")
		return
	}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if !validName(n) {
			fail(w, codeBadRequest, "群名长度须为 1-64 个字符")
			return
		}
		req.Name = &n
	}
	if req.Announcement != nil && utf8.RuneCountInString(*req.Announcement) > maxAnnouncement {
		fail(w, codeBadRequest, "群公告过长")
		return
	}
	// 与个人头像一致：avatar_id 必须是已上传的附件（空串表示清除）
	if !s.requireAvatar(w, r, req.AvatarID, "群头像附件不存在") {
		return
	}
	var g store.Group
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		if err := t.UpdateGroup(gid, store.GroupUpdate{Name: req.Name, AvatarID: req.AvatarID, Announcement: req.Announcement}); err != nil {
			return err
		}
		var err error
		g, err = broadcastGroup(t, gid)
		return err
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, g)
}

// POST /v1/groups/{id}/members {user_ids} → Group（管理员；须为操作者好友）
func (s *Server) handleAddMembers(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "group") {
		return
	}
	gid := r.PathValue("id")
	var req struct {
		UserIDs []string `json:"user_ids"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	role, valid := s.requireMember(w, r, gid, uid)
	if !valid {
		return
	}
	if !isAdminRole(role) {
		fail(w, codeForbidden, "仅管理员可添加成员")
		return
	}
	ids, valid := s.checkFriends(w, r, uid, req.UserIDs)
	if !valid {
		return
	}
	if len(ids) == 0 {
		fail(w, codeBadRequest, "user_ids 不能为空")
		return
	}
	var g store.Group
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		if err := t.AddMembers(gid, ids, maxGroupMembers); err != nil {
			return err
		}
		var err error
		g, err = broadcastGroup(t, gid)
		return err
	})
	if errors.Is(err, store.ErrTooManyMembers) {
		fail(w, codeBadRequest, "群成员过多")
		return
	} else if err != nil {
		internalErr(w, r, err)
		return
	}
	log.Printf("[group] %s 向群 %s 添加 %d 人", uid, gid, len(ids))
	ok(w, g)
}

// DELETE /v1/groups/{id}/members/{uid} → {}（管理员踢人，或 uid=自己即退群）
func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "group") {
		return
	}
	gid, target := r.PathValue("id"), r.PathValue("uid")
	role, valid := s.requireMember(w, r, gid, uid)
	if !valid {
		return
	}
	if target != uid {
		targetRole, err := s.st.MemberRole(r.Context(), gid, target)
		if err != nil {
			storeErr(w, r, err, "该用户不是群成员")
			return
		}
		// 群主可移除任何人；管理员只能移除普通成员。
		allowed := role == store.RoleOwner || (role == store.RoleAdmin && targetRole == store.RoleMember)
		if !allowed {
			fail(w, codeForbidden, "无权移除该成员")
			return
		}
	}
	var dissolved bool
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		if dissolved, err = t.RemoveMember(gid, target); err != nil {
			return err
		}
		if err := ws.Emit(t, []string{target}, "group_removed", "", obj{"group_id": gid}); err != nil {
			return err
		}
		if dissolved {
			return nil
		}
		_, err = broadcastGroup(t, gid)
		return err
	})
	if err != nil {
		storeErr(w, r, err, "该用户不是群成员")
		return
	}
	log.Printf("[group] 群 %s 移除 %s（操作者 %s，解散=%v）", gid, target, uid, dissolved)
	ok(w, obj{})
}

// PUT /v1/groups/{id}/admins/{uid} {admin} → Group（仅群主）
func (s *Server) handleSetAdmin(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "group") {
		return
	}
	gid, target := r.PathValue("id"), r.PathValue("uid")
	var req struct {
		Admin *bool `json:"admin"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if req.Admin == nil {
		fail(w, codeBadRequest, "缺少 admin 字段")
		return
	}
	role, valid := s.requireMember(w, r, gid, uid)
	if !valid {
		return
	}
	if role != store.RoleOwner {
		fail(w, codeForbidden, "仅群主可设置管理员")
		return
	}
	targetRole, err := s.st.MemberRole(r.Context(), gid, target)
	if err != nil {
		storeErr(w, r, err, "该用户不是群成员")
		return
	}
	if targetRole == store.RoleOwner {
		fail(w, codeBadRequest, "不能修改群主的角色")
		return
	}
	newRole := store.RoleMember
	if *req.Admin {
		newRole = store.RoleAdmin
	}
	var g store.Group
	err = s.st.Update(r.Context(), func(t *store.Tx) error {
		if err := t.SetRole(gid, target, newRole); err != nil {
			return err
		}
		var err error
		g, err = broadcastGroup(t, gid)
		return err
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, g)
}

// requireMember 校验 uid 是群成员并返回角色；群不存在 → not_found，非成员 → forbidden。
func (s *Server) requireMember(w http.ResponseWriter, r *http.Request, gid, uid string) (string, bool) {
	role, err := s.st.MemberRole(r.Context(), gid, uid)
	if err == nil {
		return role, true
	}
	if !errors.Is(err, store.ErrNotFound) {
		internalErr(w, r, err)
		return "", false
	}
	if _, gerr := s.st.GetGroup(r.Context(), gid); errors.Is(gerr, store.ErrNotFound) {
		fail(w, codeNotFound, "群不存在")
	} else {
		fail(w, codeForbidden, "你不是该群成员")
	}
	return "", false
}
