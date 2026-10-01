package api

import (
	"errors"
	"hush/server/internal/logx"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"hush/server/internal/store"
)

const (
	maxReportReason = 64
	maxReportDetail = 1000
)

// POST /v1/reports {target_type: user|moment|comment|group, target_id, reason, detail?} → {id}
//
// 举报只记录「谁举报了哪个对象、理由是什么」，不携带任何聊天内容。举报人必须本来就能看到该对象：
// 动态 / 评论须对举报人可见，群须是其所在的群——否则一律 404，避免用举报接口探测对象是否存在。
// 同一用户对同一对象已有待处理举报时不重复记录，返回原来那条的 id。
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.reportLimiter.allow(uid, time.Now()) {
		logx.Debugf("[ratelimit] user=%s report 过于频繁", uid)
		fail(w, codeRateLimited, "举报过于频繁，请稍后再试")
		return
	}
	var req struct {
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Reason     string `json:"reason"`
		Detail     string `json:"detail"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	req.Reason, req.Detail = strings.TrimSpace(req.Reason), strings.TrimSpace(req.Detail)
	if req.TargetID == "" || len(req.TargetID) > 64 {
		fail(w, codeBadRequest, "target_id 无效")
		return
	}
	if req.Reason == "" || utf8.RuneCountInString(req.Reason) > maxReportReason {
		fail(w, codeBadRequest, "reason 须为 1-64 个字符")
		return
	}
	if utf8.RuneCountInString(req.Detail) > maxReportDetail {
		fail(w, codeBadRequest, "detail 不能超过 1000 个字符")
		return
	}
	ctx := r.Context()
	var err error
	switch req.TargetType {
	case store.ReportUser:
		if req.TargetID == uid {
			fail(w, codeBadRequest, "不能举报自己")
			return
		}
		var exists bool
		if exists, err = s.st.UserExists(ctx, req.TargetID); err == nil && !exists {
			err = store.ErrNotFound
		}
	case store.ReportMoment:
		_, err = s.st.VisibleMoment(ctx, uid, req.TargetID)
	case store.ReportComment:
		var c store.CommentRow
		if c, err = s.st.GetComment(ctx, req.TargetID); err == nil {
			_, err = s.st.VisibleMoment(ctx, uid, c.MomentID)
		}
	case store.ReportGroup:
		_, err = s.st.MemberRole(ctx, req.TargetID, uid)
	default:
		fail(w, codeBadRequest, "target_type 须为 user / moment / comment / group")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		fail(w, codeNotFound, "举报对象不存在")
		return
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	id, created, err := s.st.CreateReport(ctx, store.Report{ReporterID: uid, TargetType: req.TargetType, TargetID: req.TargetID, Reason: req.Reason, Detail: req.Detail})
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if created {
		logx.Debugf("[report] user=%s 举报 %s %s", uid, req.TargetType, req.TargetID)
	}
	ok(w, obj{"id": id})
}

// GET /v1/announcements?before=&limit= → [Announcement]（新的在前；已过截止时间或被撤下的不返回）
func (s *Server) handleAnnouncements(w http.ResponseWriter, r *http.Request, uid string) {
	before, limit, valid := pageBefore(w, r, 20, 100)
	if !valid {
		return
	}
	list, err := s.st.ListAnnouncements(r.Context(), before, limit, true)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, list)
}
