package admin_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hush/server/internal/store"
)

func TestGroupManagement(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	a, b, c := e.register("alice"), e.register("bob"), e.register("carol")
	befriend(t, a, b)
	befriend(t, a, c)
	gid := a.createGroup("技术交流", b, c)
	other := b.createGroup("另一个群")
	a.must("POST", "/v1/groups/"+gid+"/messages", map[string]any{"session_id": "sess1", "body": "Q0lQSEVS"}, nil)
	a.must("POST", "/v1/groups/"+gid+"/messages", map[string]any{"session_id": "sess1", "body": "Q0lQSEVS"}, nil)

	var page pageOf[store.AdminGroup]
	mod.must("GET", "/admin/api/groups", nil, &page)
	if page.Total != 2 {
		t.Fatalf("群列表不对: %+v", page)
	}
	mod.must("GET", "/admin/api/groups?q=技术", nil, &page)
	if page.Total != 1 || page.Items[0].ID != gid || page.Items[0].Members != 3 || page.Items[0].Messages != 2 || page.Items[0].OwnerID != a.ID {
		t.Fatalf("群搜索 / 消息计数不对: %+v", page)
	}
	var detail struct {
		Group   store.AdminGroup    `json:"group"`
		Members []store.AdminMember `json:"members"`
	}
	mod.must("GET", "/admin/api/groups/"+gid, nil, &detail)
	if len(detail.Members) != 3 || detail.Members[0].Role != "owner" || detail.Members[0].Email != a.Email || detail.Group.CreatedAt == 0 {
		t.Fatalf("群详情不对: %+v", detail)
	}
	ackAll(b)
	ackAll(c)

	// 转让群主
	mod.expect("POST", "/admin/api/groups/"+gid+"/owner", map[string]any{"user_id": "nobody"}, 404, "not_found")
	mod.expect("POST", "/admin/api/groups/"+gid+"/owner", map[string]any{}, 400, "bad_request")
	mod.must("POST", "/admin/api/groups/"+gid+"/owner", map[string]any{"user_id": b.ID}, nil)
	var g store.Group
	b.must("GET", "/v1/groups/"+gid, nil, &g)
	roles := map[string]string{}
	for _, m := range g.Members {
		roles[m.UserID] = m.Role
	}
	if g.OwnerID != b.ID || roles[b.ID] != "owner" || roles[a.ID] != "admin" || roles[c.ID] != "member" {
		t.Fatalf("转让后角色不对: %+v", g)
	}
	if !c.hasEvent("group_updated", func(ev map[string]any) bool { return ev["group"].(map[string]any)["owner_id"] == b.ID }) {
		t.Fatalf("成员应收到 group_updated: %v", c.events())
	}
	e.audited("group.transfer_owner", "group:"+gid)

	// 移除成员
	mod.must("DELETE", "/admin/api/groups/"+gid+"/members/"+c.ID, nil, nil)
	if !c.hasEvent("group_removed", func(ev map[string]any) bool { return ev["group_id"] == gid }) {
		t.Fatalf("被移除者应收到 group_removed: %v", c.events())
	}
	c.expect("GET", "/v1/groups/"+gid, nil, 403, "forbidden")
	mod.expect("DELETE", "/admin/api/groups/"+gid+"/members/"+c.ID, nil, 404, "not_found")
	e.audited("group.remove_member", "group:"+gid)

	// 解散群
	ackAll(a)
	mod.must("DELETE", "/admin/api/groups/"+gid, nil, nil)
	for _, u := range []*user{a, b} {
		if !u.hasEvent("group_removed", func(ev map[string]any) bool { return ev["group_id"] == gid }) {
			t.Fatalf("原成员应收到 group_removed: %v", u.events())
		}
		u.expect("GET", "/v1/groups/"+gid, nil, 404, "not_found")
	}
	mod.expect("DELETE", "/admin/api/groups/"+gid, nil, 404, "not_found")
	mod.expect("GET", "/admin/api/groups/"+gid, nil, 404, "not_found")
	e.audited("group.dissolve", "group:"+gid)
	mod.must("GET", "/admin/api/groups/"+other, nil, nil)
}

func TestMomentModerationAndMedia(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	a, b := e.register("alice"), e.register("bob")
	befriend(t, a, b)
	img := a.upload(pngBytes)
	html := a.upload([]byte("<html><script>alert(1)</script></html>"))
	chat := a.upload(pngBytes) // 没有被头像或动态引用：相当于聊天附件
	m1 := a.post("第一条", img)
	time.Sleep(3 * time.Millisecond) // 动态按毫秒时间排序，错开时间戳
	m2 := a.post("带 HTML 的", html)
	time.Sleep(3 * time.Millisecond)
	m3 := b.post("b 的动态")
	var cm struct {
		ID string `json:"id"`
	}
	b.must("POST", "/v1/moments/"+m1+"/comments", map[string]any{"text": "违规评论"}, &cm)
	cid := cm.ID
	b.must("POST", "/v1/moments/"+m1+"/like", nil, nil)
	b.must("POST", "/v1/reports", map[string]any{"target_type": "moment", "target_id": m1, "reason": "spam"}, nil)

	var page pageOf[store.AdminMoment]
	mod.must("GET", "/admin/api/moments", nil, &page)
	if page.Total != 3 || page.Items[0].ID != m1 || page.Items[0].PendingReports != 1 || page.Items[0].Likes != 1 || page.Items[0].Comments != 1 ||
		page.Items[0].AuthorEmail != a.Email || len(page.Items[0].Media) != 1 {
		t.Fatalf("被举报的动态应排在最前: %+v", page)
	}
	mod.must("GET", "/admin/api/moments?order=time", nil, &page)
	if page.Items[0].ID != m3 {
		t.Fatalf("按时间排序不对: %+v", page.Items[0])
	}
	mod.must("GET", "/admin/api/moments?author="+b.ID, nil, &page)
	if page.Total != 1 || page.Items[0].ID != m3 {
		t.Fatalf("按作者筛选不对: %+v", page)
	}
	mod.must("GET", "/admin/api/moments?reported=1", nil, &page)
	if page.Total != 1 || page.Items[0].ID != m1 {
		t.Fatalf("只看被举报不对: %+v", page)
	}
	var comments []store.AdminComment
	mod.must("GET", "/admin/api/moments/"+m1+"/comments", nil, &comments)
	if len(comments) != 1 || comments[0].ID != cid || comments[0].Text != "违规评论" {
		t.Fatalf("评论列表不对: %+v", comments)
	}

	// 缩略图：朋友圈图片可以看；聊天附件（未被引用）看不到；不是图片的内容不渲染
	status, body, h := mod.raw("GET", "/admin/api/media/"+img, nil, nil)
	if status != 200 || h.Get("Content-Type") != "image/png" || len(body) != len(pngBytes) || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("朋友圈图片应可预览: %d %v", status, h)
	}
	mod.expect("GET", "/admin/api/media/"+chat, nil, 404, "not_found")
	mod.expect("GET", "/admin/api/media/"+html, nil, 415, "unsupported_media")
	mod.expect("GET", "/admin/api/media/..%2F..%2Fhush.db", nil, 404, "not_found")
	mod.expect("GET", "/admin/api/media/00000000-0000-0000-0000-000000000000", nil, 404, "not_found")

	// 删除评论、删除动态（图片文件一并删除，待处理举报自动结案）
	mod.must("DELETE", "/admin/api/comments/"+cid, nil, nil)
	mod.expect("DELETE", "/admin/api/comments/"+cid, nil, 404, "not_found")
	e.audited("comment.delete", "comment:"+cid)
	mod.must("DELETE", "/admin/api/moments/"+m1, nil, nil)
	mod.expect("DELETE", "/admin/api/moments/"+m1, nil, 404, "not_found")
	e.audited("moment.delete", "moment:"+m1)
	a.expect("GET", "/v1/moments/"+m1, nil, 404, "not_found")
	if _, err := os.Stat(filepath.Join(e.dataDir, "attachments", img)); !os.IsNotExist(err) {
		t.Fatalf("被删动态的图片文件应已删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "attachments", chat)); err != nil {
		t.Fatalf("无关附件不应被删: %v", err)
	}
	var reports pageOf[store.Report]
	mod.must("GET", "/admin/api/reports?status=pending", nil, &reports)
	if reports.Total != 0 {
		t.Fatalf("动态删除后其举报应自动结案: %+v", reports)
	}
	_ = m2
}

func TestReports(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	a, b, c := e.register("alice"), e.register("bob"), e.register("carol")
	befriend(t, a, b)
	gid := a.createGroup("问题群", b)
	mid := a.post("违规动态")
	var cm struct {
		ID string `json:"id"`
	}
	b.must("POST", "/v1/moments/"+mid+"/comments", map[string]any{"text": "违规评论"}, &cm)
	cid := cm.ID

	// 客户端接口：参数校验、可见性、去重
	b.expect("POST", "/v1/reports", map[string]any{"target_type": "message", "target_id": "x", "reason": "spam"}, 400, "bad_request")
	b.expect("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": a.ID}, 400, "bad_request")
	b.expect("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": b.ID, "reason": "spam"}, 400, "bad_request")
	b.expect("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": "nobody", "reason": "spam"}, 404, "not_found")
	b.expect("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": a.ID, "reason": "spam", "detail": strings.Repeat("长", 1001)}, 400, "bad_request")
	// c 不是 a 的好友：看不到 a 的动态 / 评论，也不在群里 → 一律 404
	c.expect("POST", "/v1/reports", map[string]any{"target_type": "moment", "target_id": mid, "reason": "spam"}, 404, "not_found")
	c.expect("POST", "/v1/reports", map[string]any{"target_type": "comment", "target_id": cid, "reason": "spam"}, 404, "not_found")
	c.expect("POST", "/v1/reports", map[string]any{"target_type": "group", "target_id": gid, "reason": "spam"}, 404, "not_found")
	e.apiDo("POST", "/v1/reports", "", map[string]any{})
	if st, _ := e.apiDo("POST", "/v1/reports", "", map[string]any{"target_type": "user", "target_id": a.ID, "reason": "spam"}); st != 401 {
		t.Fatalf("未登录举报应为 401: %d", st)
	}
	var r1, r2 struct {
		ID string `json:"id"`
	}
	b.must("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": a.ID, "reason": "harassment", "detail": "一直骚扰我"}, &r1)
	b.must("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": a.ID, "reason": "harassment"}, &r2)
	if r1.ID == "" || r1.ID != r2.ID {
		t.Fatalf("重复举报应返回同一条: %s %s", r1.ID, r2.ID)
	}
	var rm, rc, rg, rcUser struct {
		ID string `json:"id"`
	}
	b.must("POST", "/v1/reports", map[string]any{"target_type": "moment", "target_id": mid, "reason": "porn"}, &rm)
	a.must("POST", "/v1/reports", map[string]any{"target_type": "comment", "target_id": cid, "reason": "abuse"}, &rc)
	b.must("POST", "/v1/reports", map[string]any{"target_type": "group", "target_id": gid, "reason": "fraud"}, &rg)
	c.must("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": a.ID, "reason": "spam"}, &rcUser)

	type row struct {
		store.Report
		Target store.ReportTarget `json:"target"`
	}
	var page pageOf[row]
	mod.must("GET", "/admin/api/reports?status=pending", nil, &page)
	if page.Total != 5 || page.Items[0].ID != r1.ID || page.Items[0].ReporterName == "" || !page.Items[0].Target.Exists || page.Items[0].Detail != "一直骚扰我" {
		t.Fatalf("举报队列不对: %+v", page)
	}
	byID := map[string]row{}
	for _, it := range page.Items {
		byID[it.ID] = it
	}
	if byID[rm.ID].Target.Summary != "违规动态" || byID[rm.ID].Target.OwnerID != a.ID || byID[rc.ID].Target.Summary != "违规评论" ||
		byID[rc.ID].Target.OwnerID != b.ID || byID[rc.ID].Target.MomentID != mid || byID[rg.ID].Target.Summary != "问题群" {
		t.Fatalf("举报对象摘要不对: %+v", byID)
	}
	mod.expect("GET", "/admin/api/reports?status=bogus", nil, 400, "bad_request")
	var dash struct {
		Totals store.Totals `json:"totals"`
	}
	mod.must("GET", "/admin/api/dashboard", nil, &dash)
	if dash.Totals.PendingReports != 5 {
		t.Fatalf("仪表盘待处理举报数不对: %d", dash.Totals.PendingReports)
	}

	// 处理：参数校验
	mod.expect("POST", "/admin/api/reports/"+rc.ID+"/resolve", map[string]any{"action": "nuke"}, 400, "bad_request")
	mod.expect("POST", "/admin/api/reports/nope/resolve", map[string]any{"action": "ignore"}, 404, "not_found")
	// 忽略群举报
	mod.must("POST", "/admin/api/reports/"+rg.ID+"/resolve", map[string]any{"action": "ignore", "note": "查无实据"}, nil)
	mod.expect("POST", "/admin/api/reports/"+rg.ID+"/resolve", map[string]any{"action": "ignore"}, 409, "conflict")
	b.must("GET", "/v1/groups/"+gid, nil, nil)
	// 删除内容：评论
	mod.must("POST", "/admin/api/reports/"+rc.ID+"/resolve", map[string]any{"action": "delete_content"}, nil)
	var comments []store.AdminComment
	mod.must("GET", "/admin/api/moments/"+mid+"/comments", nil, &comments)
	if len(comments) != 0 {
		t.Fatalf("评论应已删除: %+v", comments)
	}
	// 删除内容：动态
	mod.must("POST", "/admin/api/reports/"+rm.ID+"/resolve", map[string]any{"action": "delete_content"}, nil)
	a.expect("GET", "/v1/moments/"+mid, nil, 404, "not_found")
	// 停用账号：同一对象的两条举报一起结案，账号被停用并踢下线
	conn := a.dial()
	var res struct {
		Resolved int `json:"resolved"`
	}
	mod.must("POST", "/admin/api/reports/"+r1.ID+"/resolve", map[string]any{"action": "disable_account", "note": "多人举报"}, &res)
	if res.Resolved != 2 {
		t.Fatalf("同一对象的举报应一并结案: %d", res.Resolved)
	}
	conn.expectKicked("account_disabled")
	if status, code, _ := e.verify(a.Email, ""); status != 403 || code != "account_disabled" {
		t.Fatalf("被举报账号应已停用: %d %s", status, code)
	}
	mod.must("GET", "/admin/api/reports?status=pending", nil, &page)
	if page.Total != 0 {
		t.Fatalf("应已全部处理: %+v", page)
	}
	mod.must("GET", "/admin/api/reports?status=resolved", nil, &page)
	if page.Total != 5 {
		t.Fatalf("已处理列表不对: %d", page.Total)
	}
	for _, it := range page.Items {
		if it.HandledBy != "mod1" || it.HandledAt == 0 || it.Action == "" {
			t.Fatalf("处理信息不完整: %+v", it.Report)
		}
		if it.ID == rg.ID && it.Note != "查无实据" {
			t.Fatalf("处理备注丢失: %+v", it.Report)
		}
	}
	au := e.audited("report.resolve", "report:"+r1.ID)
	if !strings.Contains(au.Detail, "disable_account") || !strings.Contains(au.Detail, a.ID) {
		t.Fatalf("审计不对: %+v", au)
	}
	// 举报「用户」并选择删除内容 = 清空资料
	d := e.register("dave")
	d.must("PUT", "/v1/me/profile", map[string]any{"name": "广告", "about": "加我"}, nil)
	var rd struct {
		ID string `json:"id"`
	}
	b.must("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": d.ID, "reason": "spam"}, &rd)
	mod.must("POST", "/admin/api/reports/"+rd.ID+"/resolve", map[string]any{"action": "delete_content"}, nil)
	var me map[string]any
	d.must("GET", "/v1/me", nil, &me)
	if me["about"] != "" || me["name"] == "广告" {
		t.Fatalf("资料应已清空: %v", me)
	}
}

func TestReportRateLimit(t *testing.T) {
	e := newEnv(t)
	owner := e.mkAdmin("owner1", store.AdminOwner)
	a := e.register("alice")
	targets := []*user{e.register("u"), e.register("u"), e.register("u")}
	owner.must("PUT", "/admin/api/settings", map[string]any{"values": map[string]string{"reports_per_hour": "2"}}, nil)
	for i, tg := range targets {
		status, body := a.call("POST", "/v1/reports", map[string]any{"target_type": "user", "target_id": tg.ID, "reason": "spam"}, nil)
		want := 200
		if i == 2 {
			want = 429
		}
		if status != want {
			t.Fatalf("第 %d 次举报: 期望 %d，得到 %d %s", i+1, want, status, body)
		}
	}
}
