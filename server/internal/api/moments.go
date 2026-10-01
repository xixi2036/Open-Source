package api

import (
	"errors"
	"hush/server/internal/logx"
	"net/http"
	"unicode/utf8"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

const (
	maxMomentText   = 5000
	maxMomentMedia  = 9
	maxCommentText  = 1000
	maxVisibleUsers = 5000
	// 图片宽高的上限（像素）。0 表示未知；负数与离谱的大数没有意义，只会让客户端按错误的比例排版。
	maxMediaDimension = 20000
	// 同一个人对同一条动态的评论通知，在接收方队列里最多保留最新的这么多条事件
	// （通知本身都在 moment_notifications 表里，客户端可以通过接口拉全）。
	commentEventKeep = 3
)

// hydrateOne 把单条动态组装为查看者视角的 Moment。
func (s *Server) hydrateOne(r *http.Request, viewer string, row store.MomentRow) (store.Moment, error) {
	list, err := s.st.HydrateMoments(r.Context(), viewer, []store.MomentRow{row})
	if err != nil {
		return store.Moment{}, err
	}
	return list[0], nil
}

// POST /v1/moments → Moment
func (s *Server) handleCreateMoment(w http.ResponseWriter, r *http.Request, uid string) {
	var req struct {
		Text       string            `json:"text"`
		Media      []store.Media     `json:"media"`
		Visibility *store.Visibility `json:"visibility"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if req.Text == "" && len(req.Media) == 0 {
		fail(w, codeBadRequest, "文字与图片不能同时为空")
		return
	}
	if utf8.RuneCountInString(req.Text) > maxMomentText {
		fail(w, codeBadRequest, "文字过长")
		return
	}
	if len(req.Media) > maxMomentMedia {
		fail(w, codeBadRequest, "最多 9 张图片")
		return
	}
	for _, m := range req.Media {
		if m.AttachmentID == "" || m.Width < 0 || m.Height < 0 || m.Width > maxMediaDimension || m.Height > maxMediaDimension {
			fail(w, codeBadRequest, "图片的 attachment_id / width / height 无效（宽高须在 0–20000 之间）")
			return
		}
	}
	if !s.allowWrite(w, uid, "moment") {
		return
	}
	vis := store.Visibility{Mode: store.VisAll}
	if req.Visibility != nil {
		vis = *req.Visibility
		if vis.Mode == "" {
			vis.Mode = store.VisAll
		}
	}
	switch vis.Mode {
	case store.VisAll:
		vis.UserIDs = nil
	case store.VisInclude, store.VisExclude:
		if len(vis.UserIDs) > maxVisibleUsers {
			fail(w, codeBadRequest, "可见范围名单过长")
			return
		}
	default:
		fail(w, codeBadRequest, "visibility.mode 须为 all/include/exclude")
		return
	}
	row, err := s.st.CreateMoment(r.Context(), uid, req.Text, req.Media, vis, s.set().MomentsPerDay)
	switch {
	case errors.Is(err, store.ErrRateLimited):
		fail(w, codeRateLimited, "今天发布的动态太多了，请明天再试")
		return
	case errors.Is(err, store.ErrNotFound):
		fail(w, codeNotFound, "图片附件不存在（只能使用自己上传的附件）")
		return
	case err != nil:
		internalErr(w, r, err)
		return
	}
	m, err := s.hydrateOne(r, uid, row)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	logx.Debugf("[moments] user=%s 发布动态 %s（%s，%d 图）", uid, row.ID, vis.Mode, len(req.Media))
	ok(w, m)
}

// GET /v1/moments/feed?before=&limit=20 → [Moment]
func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request, uid string) {
	before, limit, valid := pageParams(w, r, 20, 100)
	if !valid {
		return
	}
	rows, err := s.st.Feed(r.Context(), uid, before, limit)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	list, err := s.st.HydrateMoments(r.Context(), uid, rows)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, list)
}

// GET /v1/moments/user/{uid}?before=&limit=20 → [Moment]（非好友返回空列表）
func (s *Server) handleUserMoments(w http.ResponseWriter, r *http.Request, uid string) {
	author := r.PathValue("uid")
	before, limit, valid := pageParams(w, r, 20, 100)
	if !valid {
		return
	}
	if exists, err := s.st.UserExists(r.Context(), author); err != nil {
		internalErr(w, r, err)
		return
	} else if !exists {
		fail(w, codeNotFound, "用户不存在")
		return
	}
	rows, err := s.st.UserMoments(r.Context(), uid, author, before, limit)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	list, err := s.st.HydrateMoments(r.Context(), uid, rows)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, list)
}

// GET /v1/moments/{id} → Moment（可见性与 feed 相同；不可见或不存在均为 404，不泄露存在性）
func (s *Server) handleGetMoment(w http.ResponseWriter, r *http.Request, uid string) {
	row, err := s.st.VisibleMoment(r.Context(), uid, r.PathValue("id"))
	if err != nil {
		storeErr(w, r, err, "动态不存在")
		return
	}
	m, err := s.hydrateOne(r, uid, row)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, m)
}

// DELETE /v1/moments/{id} → {}（作者）
func (s *Server) handleDeleteMoment(w http.ResponseWriter, r *http.Request, uid string) {
	row, err := s.st.VisibleMoment(r.Context(), uid, r.PathValue("id"))
	if err != nil {
		storeErr(w, r, err, "动态不存在")
		return
	}
	if row.AuthorID != uid {
		fail(w, codeForbidden, "只能删除自己的动态")
		return
	}
	if err := s.st.DeleteMoment(r.Context(), row.ID); err != nil {
		internalErr(w, r, err)
		return
	}
	logx.Debugf("[moments] user=%s 删除动态 %s", uid, row.ID)
	ok(w, obj{})
}

// notifyMoment 在事务 t 内写入一条朋友圈通知及其 moment_notification 事件。
func notifyMoment(t *store.Tx, n store.NotificationInsert) error {
	notif, err := t.CreateNotification(n)
	if err != nil {
		return err
	}
	// 点赞通知按（动态, 点赞者）合并：反复点赞/取消点赞不会在对方队列里堆积事件（取消点赞时通知本身也已删除）。
	if n.Kind == "like" {
		return ws.Emit(t, []string{n.UserID}, "moment_notification", "moment_like:"+n.MomentID+":"+n.FromID, obj{"notification": notif})
	}
	// 评论通知按（动态, 评论者）限量：反复评论不能无限占用对方的队列
	return ws.Emit(t, []string{n.UserID}, "moment_notification", "moment_comment:"+n.MomentID+":"+n.FromID,
		obj{"notification": notif}, ws.EmitOpt{Keep: commentEventKeep})
}

// POST /v1/moments/{id}/like → {}
func (s *Server) handleLike(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "like") {
		return
	}
	row, err := s.st.VisibleMoment(r.Context(), uid, r.PathValue("id"))
	if err != nil {
		storeErr(w, r, err, "动态不存在")
		return
	}
	err = s.st.Update(r.Context(), func(t *store.Tx) error {
		added, err := t.Like(row.ID, uid)
		if err != nil || !added || row.AuthorID == uid {
			return err
		}
		return notifyMoment(t, store.NotificationInsert{UserID: row.AuthorID, Kind: "like", MomentID: row.ID, FromID: uid})
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, obj{})
}

// DELETE /v1/moments/{id}/like → {}
func (s *Server) handleUnlike(w http.ResponseWriter, r *http.Request, uid, momentID string) {
	if !s.allowWrite(w, uid, "like") {
		return
	}
	row, err := s.st.VisibleMoment(r.Context(), uid, momentID)
	if err != nil {
		storeErr(w, r, err, "动态不存在")
		return
	}
	if err := s.st.Unlike(r.Context(), row.ID, uid); err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, obj{})
}

// POST /v1/moments/{id}/comments {text, reply_to_user_id?} → Comment
func (s *Server) handleComment(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "comment") {
		return
	}
	var req struct {
		Text          string  `json:"text"`
		ReplyToUserID *string `json:"reply_to_user_id"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if req.Text == "" || utf8.RuneCountInString(req.Text) > maxCommentText {
		fail(w, codeBadRequest, "评论长度须为 1-1000 个字符")
		return
	}
	row, err := s.st.VisibleMoment(r.Context(), uid, r.PathValue("id"))
	if err != nil {
		storeErr(w, r, err, "动态不存在")
		return
	}
	if req.ReplyToUserID != nil && *req.ReplyToUserID == "" {
		req.ReplyToUserID = nil
	}
	var replyTo *store.Profile
	if req.ReplyToUserID != nil {
		u, err := s.st.GetUser(r.Context(), *req.ReplyToUserID)
		if err != nil {
			storeErr(w, r, err, "被回复的用户不存在")
			return
		}
		replyTo = &u.Profile
	}
	var c store.CommentRow
	var me store.User
	err = s.st.Update(r.Context(), func(t *store.Tx) error {
		var err error
		if c, err = t.CreateComment(row.ID, uid, req.ReplyToUserID, req.Text); err != nil {
			return err
		}
		if me, err = t.GetUser(uid); err != nil {
			return err
		}
		text := req.Text
		cid := c.ID
		if row.AuthorID != uid {
			if err := notifyMoment(t, store.NotificationInsert{UserID: row.AuthorID, Kind: "comment", MomentID: row.ID, FromID: uid, CommentID: &cid, Text: &text}); err != nil {
				return err
			}
		}
		// 回复某人：仅当对方能看到这条动态、且与评论者互为好友（即能看到这条回复）时通知。
		if rt := req.ReplyToUserID; rt != nil && *rt != uid && *rt != row.AuthorID {
			visible, err := replyVisible(t, *rt, uid, row.ID)
			if err != nil || !visible {
				return err
			}
			return notifyMoment(t, store.NotificationInsert{UserID: *rt, Kind: "comment", MomentID: row.ID, FromID: uid, CommentID: &cid, Text: &text})
		}
		return nil
	})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	logx.Debugf("[moments] user=%s 评论动态 %s", uid, row.ID)
	ok(w, store.Comment{ID: c.ID, Author: me.Profile, ReplyTo: replyTo, Text: c.Text, CreatedAt: c.CreatedAt})
}

// replyVisible 判断被回复者 target 能否看到这条回复：能看到动态，且与评论者互为好友。
func replyVisible(t *store.Tx, target, commenter, momentID string) (bool, error) {
	if _, err := t.VisibleMoment(target, momentID); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if blocked, err := t.BlockedEither(target, commenter); err != nil || blocked {
		return false, err
	}
	return t.AreFriends(target, commenter)
}

// DELETE /v1/moments/{a}/{b}：分派到「取消点赞」或「删除评论」。
func (s *Server) handleMomentDelete2(w http.ResponseWriter, r *http.Request, uid string) {
	a, b := r.PathValue("a"), r.PathValue("b")
	switch {
	case a == "comments":
		s.handleDeleteComment(w, r, uid, b)
	case b == "like":
		s.handleUnlike(w, r, uid, a)
	default:
		fail(w, codeNotFound, "接口不存在")
	}
}

// DELETE /v1/moments/comments/{cid} → {}（评论作者或动态作者）
func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request, uid, cid string) {
	c, err := s.st.GetComment(r.Context(), cid)
	if err != nil {
		storeErr(w, r, err, "评论不存在")
		return
	}
	row, err := s.st.GetMomentRow(r.Context(), c.MomentID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalErr(w, r, err)
		return
	}
	if c.AuthorID != uid && row.AuthorID != uid {
		fail(w, codeForbidden, "只有评论作者或动态作者可以删除评论")
		return
	}
	if err := s.st.DeleteComment(r.Context(), cid); err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, obj{})
}

// GET /v1/moments/notifications?before=&limit= → [MomentNotification]
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request, uid string) {
	before, limit, valid := pageParams(w, r, 50, 200)
	if !valid {
		return
	}
	list, err := s.st.Notifications(r.Context(), uid, before, limit)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, list)
}
