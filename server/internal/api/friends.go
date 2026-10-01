package api

import (
	"context"
	"errors"
	"hush/server/internal/logx"
	"net/http"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// FriendRequestView 是好友申请的对外表示（契约 3.5）。
type FriendRequestView struct {
	ID        string        `json:"id"`
	From      store.Profile `json:"from"`
	To        store.Profile `json:"to"`
	Message   string        `json:"message"`
	Status    string        `json:"status"`
	CreatedAt int64         `json:"created_at"`
}

// profileLookup 批量查询资料（*store.Store 或 *store.Tx 的 ProfilesWithEmail）。
type profileLookup func(ids []string) (map[string]store.Profile, error)

func (s *Server) profilesWithEmail(ctx context.Context) profileLookup {
	return func(ids []string) (map[string]store.Profile, error) { return s.st.ProfilesWithEmail(ctx, ids) }
}

// requestViews 组装好友申请视图。申请双方可见彼此邮箱（发起方本来就是按邮箱搜索到对方的）；视图只会发给申请双方。
func requestViews(lookup profileLookup, reqs []store.FriendRequest) ([]FriendRequestView, error) {
	ids := make([]string, 0, len(reqs)*2)
	for _, q := range reqs {
		ids = append(ids, q.FromID, q.ToID)
	}
	profiles, err := lookup(ids)
	if err != nil {
		return nil, err
	}
	out := make([]FriendRequestView, 0, len(reqs))
	for _, q := range reqs {
		out = append(out, FriendRequestView{ID: q.ID, From: profiles[q.FromID], To: profiles[q.ToID], Message: q.Message, Status: q.Status, CreatedAt: q.CreatedAt})
	}
	return out, nil
}

// POST /v1/friends/requests {to_user_id, message} → FriendRequest
func (s *Server) handleSendFriendRequest(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "friend_request") {
		return
	}
	var req struct {
		ToUserID string `json:"to_user_id"`
		Message  string `json:"message"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if req.ToUserID == "" || req.ToUserID == uid {
		fail(w, codeBadRequest, "to_user_id 无效")
		return
	}
	if !validShortText(req.Message) {
		fail(w, codeBadRequest, "留言不能超过 256 个字符")
		return
	}
	var res store.SendRequestResult
	var view FriendRequestView
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		if res, err = t.SendFriendRequest(uid, req.ToUserID, req.Message); err != nil {
			return err
		}
		views, err := requestViews(t.ProfilesWithEmail, []store.FriendRequest{res.Request})
		if err != nil {
			return err
		}
		view = views[0]
		if res.BecameFriends {
			// 对方已向我发出待处理申请：直接成为好友，返回该申请（status=accepted）。
			return ws.Emit(t, []string{req.ToUserID}, "friend_accepted", "friend_accepted:"+uid, obj{"user_id": uid})
		}
		return ws.Emit(t, []string{req.ToUserID}, "friend_request", "friend_request:"+res.Request.ID, obj{"request": view})
	})
	if !friendErr(w, r, err, "用户不存在", "你们已经是好友") {
		return
	}
	if res.BecameFriends {
		logx.Debugf("[friends] %s 与 %s 互发申请，直接成为好友", uid, req.ToUserID)
	} else {
		logx.Debugf("[friends] %s -> %s 好友申请 %s", uid, req.ToUserID, res.Request.ID)
	}
	ok(w, view)
}

// friendErr 把好友相关的存储错误映射为错误码；err 为 nil 时返回 true。
func friendErr(w http.ResponseWriter, r *http.Request, err error, notFoundMsg, conflictMsg string) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		fail(w, codeNotFound, notFoundMsg)
	case errors.Is(err, store.ErrBlocked):
		fail(w, codeBlocked, "你们之间存在拉黑关系")
	case errors.Is(err, store.ErrConflict):
		fail(w, codeConflict, conflictMsg)
	default:
		internalErr(w, r, err)
	}
	return false
}

// GET /v1/friends/requests → {incoming, outgoing}
func (s *Server) handleListFriendRequests(w http.ResponseWriter, r *http.Request, uid string) {
	in, out, err := s.st.PendingRequests(r.Context(), uid)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	inV, err := requestViews(s.profilesWithEmail(r.Context()), in)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	outV, err := requestViews(s.profilesWithEmail(r.Context()), out)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, obj{"incoming": inV, "outgoing": outV})
}

// POST /v1/friends/requests/{id}/accept → {}
func (s *Server) handleAcceptFriendRequest(w http.ResponseWriter, r *http.Request, uid string) {
	var fr store.FriendRequest
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		if fr, err = t.AcceptFriendRequest(r.PathValue("id"), uid); err != nil {
			return err
		}
		return ws.Emit(t, []string{fr.FromID}, "friend_accepted", "friend_accepted:"+uid, obj{"user_id": uid})
	})
	if !friendErr(w, r, err, "好友申请不存在", "该申请已处理") {
		return
	}
	logx.Debugf("[friends] %s 接受了 %s 的申请", uid, fr.FromID)
	ok(w, obj{})
}

// POST /v1/friends/requests/{id}/reject → {}
func (s *Server) handleRejectFriendRequest(w http.ResponseWriter, r *http.Request, uid string) {
	fr, err := s.st.RejectFriendRequest(r.Context(), r.PathValue("id"), uid)
	if !friendErr(w, r, err, "好友申请不存在", "该申请已处理") {
		return
	}
	logx.Debugf("[friends] %s 拒绝了 %s 的申请", uid, fr.FromID)
	ok(w, obj{})
}

// GET /v1/friends → [Profile]
func (s *Server) handleListFriends(w http.ResponseWriter, r *http.Request, uid string) {
	list, err := s.st.Friends(r.Context(), uid)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, list)
}

// DELETE /v1/friends/{user_id} → {}
func (s *Server) handleRemoveFriend(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "friend_remove") {
		return
	}
	other := r.PathValue("user_id")
	var was bool
	err := s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		if was, err = t.RemoveFriend(uid, other); err != nil || !was {
			return err
		}
		return ws.Emit(t, []string{other}, "friend_removed", "friend_removed:"+uid, obj{"user_id": uid})
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if was {
		logx.Debugf("[friends] %s 删除好友 %s", uid, other)
	}
	ok(w, obj{})
}

// GET /v1/blocks → [user_id]
func (s *Server) handleListBlocks(w http.ResponseWriter, r *http.Request, uid string) {
	ids, err := s.st.Blocked(r.Context(), uid)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, ids)
}

// POST /v1/blocks/{user_id} → {}
func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request, uid string) {
	target := r.PathValue("user_id")
	if target == uid {
		fail(w, codeBadRequest, "不能拉黑自己")
		return
	}
	if exists, err := s.st.UserExists(r.Context(), target); err != nil {
		internalErr(w, r, err)
		return
	} else if !exists {
		fail(w, codeNotFound, "用户不存在")
		return
	}
	if err := s.st.Block(r.Context(), uid, target); err != nil {
		internalErr(w, r, err)
		return
	}
	logx.Debugf("[friends] %s 拉黑 %s", uid, target)
	ok(w, obj{})
}

// DELETE /v1/blocks/{user_id} → {}
func (s *Server) handleUnblock(w http.ResponseWriter, r *http.Request, uid string) {
	if err := s.st.Unblock(r.Context(), uid, r.PathValue("user_id")); err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, obj{})
}
