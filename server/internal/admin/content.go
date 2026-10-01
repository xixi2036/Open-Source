package admin

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"hush/server/internal/store"
	"hush/server/internal/ws"
)

// ---------- A8 群组 ----------

// GET /admin/api/groups?q=&page=&size=
func (c *ctx) listGroups() {
	page, size := c.page()
	if err := c.s.api.FlushStats(c.r.Context()); err != nil {
		log.Printf("[admin] 统计落库失败: %v", err)
	}
	list, total, err := c.s.st.AdminListGroups(c.r.Context(), c.query("q"), page, size)
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(paged(list, total, page, size))
}

// GET /admin/api/groups/{id} → {group, members}（messages 是累计条数，不含内容）
func (c *ctx) getGroup() {
	if err := c.s.api.FlushStats(c.r.Context()); err != nil {
		log.Printf("[admin] 统计落库失败: %v", err)
	}
	g, members, err := c.s.st.AdminGetGroup(c.r.Context(), c.path("id"))
	if err != nil {
		c.storeErr(err, "群不存在")
		return
	}
	c.ok(obj{"group": g, "members": members})
}

// txDissolve 在事务内解散群并通知原成员（group_removed，与被移出群时相同）。
func txDissolve(t *store.Tx, gid string) (int, error) {
	members, err := t.DissolveGroup(gid)
	if err != nil {
		return 0, err
	}
	return len(members), ws.Emit(t, members, "group_removed", "group_removed:"+gid, obj{"group_id": gid})
}

// DELETE /admin/api/groups/{id} → {}（解散群）
func (c *ctx) dissolveGroup() {
	gid := c.path("id")
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		g, err := t.GetGroup(gid)
		if err != nil {
			return err
		}
		n, err := txDissolve(t, gid)
		if err != nil {
			return err
		}
		return t.AppendAudit(c.audit("group.dissolve", "group:"+gid, jsonDetail(obj{"name": g.Name, "members": n})))
	})
	if err != nil {
		c.storeErr(err, "群不存在")
		return
	}
	c.ok(obj{})
}

// DELETE /admin/api/groups/{id}/members/{uid} → {}（移除成员；移除群主时自动转让，群内无人则解散）
func (c *ctx) removeMember() {
	gid, uid := c.path("id"), c.path("uid")
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		dissolved, err := t.RemoveMember(gid, uid)
		if err != nil {
			return err
		}
		if err := ws.Emit(t, []string{uid}, "group_removed", "group_removed:"+gid, obj{"group_id": gid}); err != nil {
			return err
		}
		if !dissolved {
			if err := txBroadcastGroup(t, gid); err != nil {
				return err
			}
		}
		return t.AppendAudit(c.audit("group.remove_member", "group:"+gid, jsonDetail(obj{"user_id": uid, "dissolved": dissolved})))
	})
	if err != nil {
		c.storeErr(err, "群不存在或该用户不是群成员")
		return
	}
	c.ok(obj{})
}

// POST /admin/api/groups/{id}/owner {user_id} → {}（转让群主给现有成员；原群主降为管理员）
func (c *ctx) transferOwner() {
	gid := c.path("id")
	var req struct {
		UserID string `json:"user_id"`
	}
	if !c.decode(&req) {
		return
	}
	if req.UserID == "" {
		c.bad("缺少 user_id")
		return
	}
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		if err := t.TransferOwner(gid, req.UserID); err != nil {
			return err
		}
		if err := txBroadcastGroup(t, gid); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("group.transfer_owner", "group:"+gid, jsonDetail(obj{"user_id": req.UserID})))
	})
	if err != nil {
		c.storeErr(err, "群不存在或该用户不是群成员")
		return
	}
	c.ok(obj{})
}

// ---------- A9 朋友圈审核 ----------

// GET /admin/api/moments?author=&order=reported|time&reported=1&page=&size=
// 默认被举报（有待处理举报）的排在前面。
func (c *ctx) listMoments() {
	page, size := c.page()
	list, total, err := c.s.st.AdminListMoments(c.r.Context(), c.query("author"), c.query("order") != "time", c.query("reported") == "1", page, size)
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(paged(list, total, page, size))
}

// GET /admin/api/moments/{id}/comments → [评论]
func (c *ctx) listComments() {
	list, err := c.s.st.AdminMomentComments(c.r.Context(), c.path("id"))
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(list)
}

// DELETE /admin/api/moments/{id} → {}
func (c *ctx) deleteMoment() {
	id := c.path("id")
	var media []string
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		author, m, err := t.AdminDeleteMoment(id)
		if err != nil {
			return err
		}
		media = m
		if _, err := t.ResolveReports(store.ReportMoment, id, "delete_content", "动态已被删除", c.admin.Username); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("moment.delete", "moment:"+id, jsonDetail(obj{"author_id": author})))
	})
	if err != nil {
		c.storeErr(err, "动态不存在")
		return
	}
	c.s.purgeMedia(c, media)
	c.ok(obj{})
}

// DELETE /admin/api/comments/{id} → {}
func (c *ctx) deleteComment() {
	id := c.path("id")
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		author, err := t.AdminDeleteComment(id)
		if err != nil {
			return err
		}
		if _, err := t.ResolveReports(store.ReportComment, id, "delete_content", "评论已被删除", c.admin.Username); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("comment.delete", "comment:"+id, jsonDetail(obj{"author_id": author})))
	})
	if err != nil {
		c.storeErr(err, "评论不存在")
		return
	}
	c.ok(obj{})
}

// 允许后台预览的图片类型（按文件内容嗅探，不信任任何声明）。
var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true, "image/bmp": true}

// GET /admin/api/media/{id} → 图片字节
//
// 只能读取**头像或朋友圈图片**（服务端本来就以明文持有的内容）；聊天附件是端到端加密的密文，一律 404，
// 后台没有任何读取它们的途径。内容必须嗅探为图片，否则 415：用户上传的 HTML/SVG 不会在后台的源下被渲染。
func (c *ctx) media() {
	id := c.path("id")
	if _, err := uuid.Parse(id); err != nil {
		c.notFound("图片不存在")
		return
	}
	public, err := c.s.st.PublicMedia(c.r.Context(), id)
	if err != nil {
		c.internal(err)
		return
	}
	if !public {
		c.notFound("图片不存在")
		return
	}
	f, err := os.Open(filepath.Join(c.s.cfg.DataDir, "attachments", id))
	if err != nil {
		c.notFound("图片不存在")
		return
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := http.DetectContentType(head[:n])
	if !imageTypes[ctype] {
		fail(c.w, http.StatusUnsupportedMediaType, "unsupported_media", "该附件不是可预览的图片")
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		c.internal(err)
		return
	}
	h := c.w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=300")
	http.ServeContent(c.w, c.r, "", time.Time{}, f)
}

// ---------- A10 举报 ----------

// GET /admin/api/reports?status=pending|resolved&page=&size= → 分页的举报（带被举报对象的摘要）
func (c *ctx) listReports() {
	page, size := c.page()
	status := c.query("status")
	if status != "" && status != "pending" && status != "resolved" {
		c.bad("status 须为 pending / resolved")
		return
	}
	list, total, err := c.s.st.AdminListReports(c.r.Context(), status, page, size)
	if err != nil {
		c.internal(err)
		return
	}
	type row struct {
		store.Report
		Target store.ReportTarget `json:"target"`
	}
	targets, err := c.s.st.ReportTargets(c.r.Context(), list) // 每种对象类型一条查询，而不是每行举报一条
	if err != nil {
		c.internal(err)
		return
	}
	out := make([]row, len(list))
	for i, rp := range list {
		t := targets[rp.TargetType+"\x00"+rp.TargetID]
		if utf8.RuneCountInString(t.Summary) > 200 {
			t.Summary = string([]rune(t.Summary)[:200]) + "…"
		}
		out[i] = row{Report: rp, Target: t}
	}
	c.ok(paged(out, total, page, size))
}

// POST /admin/api/reports/{id}/resolve {action: ignore|delete_content|disable_account, note?} → {resolved}
//
// delete_content：动态 / 评论直接删除；用户 → 清空昵称、签名、头像；群 → 解散。
// disable_account：停用责任账号（用户本人、动态或评论的作者、群主）。
// 针对同一对象的其他待处理举报一并标记为已处理。
func (c *ctx) resolveReport() {
	id := c.path("id")
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if !c.decode(&req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.Action != "ignore" && req.Action != "delete_content" && req.Action != "disable_account" {
		c.bad("action 须为 ignore / delete_content / disable_account")
		return
	}
	if utf8.RuneCountInString(req.Note) > 500 {
		c.bad("备注不能超过 500 个字符")
		return
	}
	rctx := c.r.Context()
	rp, err := c.s.st.GetReport(rctx, id)
	if err != nil {
		c.storeErr(err, "举报不存在")
		return
	}
	if rp.Status != "pending" {
		c.conflict("该举报已处理")
		return
	}
	target, err := c.s.st.ReportTargetInfo(rctx, rp.TargetType, rp.TargetID)
	if err != nil {
		c.internal(err)
		return
	}
	var media []string
	var kickUID string
	var resolved int64
	gone := func(err error) error { // 对象在此期间已被删除：照常把举报标记为已处理
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	err = c.s.st.Update(rctx, func(t *store.Tx) error {
		media, kickUID = nil, ""
		switch {
		case req.Action == "delete_content" && target.Exists:
			var err error
			switch rp.TargetType {
			case store.ReportMoment:
				_, media, err = t.AdminDeleteMoment(rp.TargetID)
			case store.ReportComment:
				_, err = t.AdminDeleteComment(rp.TargetID)
			case store.ReportUser:
				var u store.AdminUser
				if u, err = t.AdminGetUser(rp.TargetID); err == nil {
					if u.AvatarID != nil {
						media = []string{*u.AvatarID}
					}
					err = txResetProfile(t, rp.TargetID, true, true, true)
				}
			case store.ReportGroup:
				_, err = txDissolve(t, rp.TargetID)
			}
			if err = gone(err); err != nil {
				return err
			}
		case req.Action == "disable_account" && target.OwnerID != "":
			if err := gone(txDisable(t, target.OwnerID)); err != nil {
				return err
			}
			kickUID = target.OwnerID
		}
		var err error
		if resolved, err = t.ResolveReports(rp.TargetType, rp.TargetID, req.Action, req.Note, c.admin.Username); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("report.resolve", "report:"+id, jsonDetail(obj{
			"action": req.Action, "target_type": rp.TargetType, "target_id": rp.TargetID, "account": target.OwnerID, "note": req.Note})))
	})
	if err != nil {
		c.internal(err)
		return
	}
	if kickUID != "" {
		c.s.kick(kickUID, "account_disabled")
	}
	c.s.purgeMedia(c, media)
	c.ok(obj{"resolved": resolved})
}
