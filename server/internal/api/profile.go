package api

import (
	"errors"
	"net/http"
	"time"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// GET /v1/me → Profile（含 email）
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, uid string) {
	u, err := s.st.GetUser(r.Context(), uid)
	if err != nil {
		storeErr(w, r, err, "用户不存在")
		return
	}
	ok(w, u.WithEmail())
}

// PUT /v1/me/profile {name?, about?, avatar_id?} → Profile
func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "profile") {
		return
	}
	var req struct {
		Name     *string `json:"name"`
		About    *string `json:"about"`
		AvatarID *string `json:"avatar_id"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if req.Name != nil && !validName(*req.Name) {
		fail(w, codeBadRequest, "昵称长度须为 1-64 个字符")
		return
	}
	if req.About != nil && !validShortText(*req.About) {
		fail(w, codeBadRequest, "签名不能超过 256 个字符")
		return
	}
	var p store.Profile
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		// 头像必须是自己上传的附件；检查与写入在同一事务里（B4）
		if req.AvatarID != nil && *req.AvatarID != "" {
			if owns, err := t.OwnsAttachment(*req.AvatarID, uid); err != nil {
				return err
			} else if !owns {
				return errAvatar
			}
		}
		if p, err = t.UpdateProfile(uid, store.ProfileUpdate{Name: req.Name, About: req.About, AvatarID: req.AvatarID}); err != nil {
			return err
		}
		contacts, err := t.Contacts(uid)
		if err != nil {
			return err
		}
		return ws.Emit(t, contacts, "profile_updated", "profile_updated:"+uid, obj{"profile": p})
	})
	if errors.Is(err, errAvatar) {
		fail(w, codeNotFound, "头像附件不存在")
		return
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, p)
}

// GET /v1/users/{id} → Profile（查看者是本人或互为好友时带 email）
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request, uid string) {
	u, err := s.st.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		storeErr(w, r, err, "用户不存在")
		return
	}
	if u.ID == uid {
		ok(w, u.WithEmail())
		return
	}
	friends, err := s.st.AreFriends(r.Context(), uid, u.ID)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if friends {
		ok(w, u.WithEmail())
		return
	}
	ok(w, u.Profile)
}

// GET /v1/users/search?email= → Profile 或 404
func (s *Server) handleSearchUser(w http.ResponseWriter, r *http.Request, uid string) {
	// 按用户限速：否则可以用这个接口批量探测哪些邮箱注册过
	if !s.searchLimiter.allow(uid, time.Now()) {
		fail(w, codeRateLimited, "搜索过于频繁，请稍后再试")
		return
	}
	email, valid := normalizeEmail(r.URL.Query().Get("email"))
	if !valid {
		fail(w, codeBadRequest, "邮箱格式无效")
		return
	}
	u, err := s.st.GetUserByEmail(r.Context(), email)
	if err != nil {
		storeErr(w, r, err, "未找到该邮箱的用户")
		return
	}
	ok(w, u.Profile)
}
