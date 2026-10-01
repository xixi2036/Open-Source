package admin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hush/server/internal/store"
)

type pageOf[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

func TestUserListAndDetail(t *testing.T) {
	e := newEnv(t)
	v := e.mkAdmin("viewer1", store.AdminViewer)
	a, b, c := e.register("alice"), e.register("bob"), e.register("carol")
	befriend(t, a, b)
	gid := a.createGroup("小组", b)
	a.post("hello")
	a.upload([]byte("some-bytes"))
	b.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": a.ID, "type": 0, "body": "QUJD"}}}, nil)

	var page pageOf[map[string]any]
	v.must("GET", "/admin/api/users", nil, &page)
	if page.Total != 3 || len(page.Items) != 3 || page.Items[0]["id"] != c.ID {
		t.Fatalf("列表不对（应按注册时间倒序）: %+v", page)
	}
	v.must("GET", "/admin/api/users?size=2&page=2", nil, &page)
	if page.Total != 3 || len(page.Items) != 1 {
		t.Fatalf("分页不对: %+v", page)
	}
	for _, q := range []string{"alice", strings.ToUpper(a.Email[:5]), a.ID[:8]} {
		v.must("GET", "/admin/api/users?q="+q, nil, &page)
		if page.Total != 1 || page.Items[0]["id"] != a.ID {
			t.Fatalf("搜索 %q 不对: %+v", q, page)
		}
	}
	v.must("GET", "/admin/api/users?q=%25", nil, &page) // LIKE 通配符被转义
	if page.Total != 0 {
		t.Fatalf("%% 不应匹配所有用户: %d", page.Total)
	}
	v.must("GET", "/admin/api/users?sort=active", nil, &page)
	if page.Total != 3 {
		t.Fatal("按活跃排序失败")
	}
	var d struct {
		store.AdminUserDetail
		Online bool `json:"online"`
	}
	v.must("GET", "/admin/api/users/"+a.ID, nil, &d)
	if d.Email != a.Email || d.Friends != 1 || d.Moments != 1 || d.Attachments != 1 || d.AttachmentBytes != 10 ||
		d.QueueItems < 1 || d.QueueBytes <= 0 || len(d.Groups) != 1 || d.Groups[0].ID != gid || d.Groups[0].Role != "owner" ||
		d.Platform != "android" || d.DeviceName != "test" || d.Sessions != 1 || d.Online {
		t.Fatalf("详情不对: %+v", d)
	}
	a.dial()
	v.must("GET", "/admin/api/users/"+a.ID, nil, &d)
	if !d.Online {
		t.Fatal("连上 WS 后应显示在线")
	}
	v.expect("GET", "/admin/api/users/nope", nil, 404, "not_found")
}

func TestDisableEnableUser(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	a, b := e.register("alice"), e.register("bob")
	befriend(t, a, b)
	b.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": a.ID, "type": 0, "body": "QUJD"}}}, nil)
	conn := a.dial()

	mod.expect("POST", "/admin/api/users/nope/disable", nil, 404, "not_found")
	mod.must("POST", "/admin/api/users/"+a.ID+"/disable", map[string]any{"reason": "spam"}, nil)
	// 在线连接：收到 logged_out{reason:"account_disabled"} 后以 4001 断开
	conn.expectKicked("account_disabled")
	// 令牌失效（HTTP 与 WS 握手都是 401）
	a.expect("GET", "/v1/me", nil, 401, "unauthorized")
	a.expect("GET", "/v1/ws", nil, 401, "unauthorized")
	// 登录被拒：403 account_disabled，且不会像正常重新登录那样清空密钥和队列
	status, code, _ := e.verify(a.Email, "")
	if status != 403 || code != "account_disabled" {
		t.Fatalf("停用账号登录应返回 403 account_disabled，得到 %d %s", status, code)
	}
	var d store.AdminUserDetail
	mod.must("GET", "/admin/api/users/"+a.ID, nil, &d)
	if d.DisabledAt == 0 || d.DeviceGeneration != 1 || d.QueueItems == 0 || d.Sessions != 0 {
		t.Fatalf("停用后状态不对（设备代不应增加、队列不应被清空）: %+v", d)
	}
	var page pageOf[map[string]any]
	mod.must("GET", "/admin/api/users?status=disabled", nil, &page)
	if page.Total != 1 || page.Items[0]["id"] != a.ID {
		t.Fatalf("按停用状态筛选不对: %+v", page)
	}
	au := e.audited("user.disable", "user:"+a.ID)
	if au.AdminName != "mod1" || !strings.Contains(au.Detail, "spam") {
		t.Fatalf("审计不对: %+v", au)
	}
	// 启用后可重新登录
	mod.must("POST", "/admin/api/users/"+a.ID+"/enable", nil, nil)
	e.audited("user.enable", "user:"+a.ID)
	if status, code, u := e.verify(a.Email, ""); status != 200 || u.ID != a.ID {
		t.Fatalf("启用后应能登录: %d %s", status, code)
	}
	// 真实验证码路径：停用账号登录被拒时验证码不被消费，启用后同一个验证码仍可用
	mod.must("POST", "/admin/api/users/"+b.ID+"/disable", nil, nil)
	if st, body := e.apiDo("POST", "/v1/auth/code", "", map[string]any{"email": b.Email}); st != 200 {
		t.Fatalf("申请验证码失败: %d %s", st, body)
	}
	realCode := e.mailer.code(b.Email)
	body := map[string]any{"email": b.Email, "code": realCode, "platform": "ios"}
	if st, raw := e.apiDo("POST", "/v1/auth/verify", "", body); st != 403 || !strings.Contains(string(raw), "account_disabled") {
		t.Fatalf("应为 403 account_disabled: %d %s", st, raw)
	}
	mod.must("POST", "/admin/api/users/"+b.ID+"/enable", nil, nil)
	if st, raw := e.apiDo("POST", "/v1/auth/verify", "", body); st != 200 {
		t.Fatalf("启用后同一验证码应可登录: %d %s", st, raw)
	}
	if st, _ := e.apiDo("POST", "/v1/auth/verify", "", body); st != 401 {
		t.Fatalf("验证码只能消费一次: %d", st)
	}
}

func TestForceLogoutAndResetProfile(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	a, b := e.register("alice"), e.register("bob")
	befriend(t, a, b)
	avatar := a.upload(pngBytes)
	a.must("PUT", "/v1/me/profile", map[string]any{"name": "违规昵称", "about": "违规签名", "avatar_id": avatar}, nil)
	conn := a.dial()
	bconn := b.dial()

	mod.must("POST", "/admin/api/users/"+a.ID+"/reset-profile", map[string]any{"name": true, "about": true, "avatar": true}, nil)
	ev := bconn.until("profile_updated(重置后)", func(typ string, d map[string]any) bool {
		p, _ := d["profile"].(map[string]any)
		return typ == "event" && d["type"] == "profile_updated" && p != nil && p["about"] == ""
	})
	p := ev["profile"].(map[string]any)
	if p["id"] != a.ID || p["avatar_id"] != nil || strings.Contains(p["name"].(string), "违规") || p["name"] == "" {
		t.Fatalf("重置后的资料不对: %v", p)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "attachments", avatar)); !os.IsNotExist(err) {
		t.Fatalf("被清除的头像文件应已删除: %v", err)
	}
	mod.expect("POST", "/admin/api/users/"+a.ID+"/reset-profile", map[string]any{}, 400, "bad_request")
	e.audited("user.reset_profile", "user:"+a.ID)

	mod.must("POST", "/admin/api/users/"+a.ID+"/logout", nil, nil)
	conn.expectKicked("forced_logout")
	a.expect("GET", "/v1/me", nil, 401, "unauthorized")
	e.audited("user.logout", "user:"+a.ID)
	// 强制下线不是停用：可以重新登录
	if status, _, _ := e.verify(a.Email, ""); status != 200 {
		t.Fatalf("强制下线后应能重新登录: %d", status)
	}
	mod.expect("POST", "/admin/api/users/nope/logout", nil, 404, "not_found")
}

func TestDeleteUserCascade(t *testing.T) {
	e := newEnv(t)
	mod := e.mkAdmin("mod1", store.AdminModerator)
	a, b, c := e.register("alice"), e.register("bob"), e.register("carol")
	befriend(t, a, b)
	befriend(t, a, c)
	befriend(t, b, c)
	owned := a.createGroup("a 的群", b, c)  // a 是群主
	solo := a.createGroup("只有 a")         // 只有 a 一人
	joined := b.createGroup("b 的群", a, c) // a 是普通成员
	img := a.upload(pngBytes)
	mid := a.post("a 的动态", img)
	bm := b.post("b 的动态")
	a.must("POST", "/v1/moments/"+bm+"/comments", map[string]any{"text": "a 的评论"}, nil)
	a.must("POST", "/v1/moments/"+bm+"/like", nil, nil)
	c.must("POST", "/v1/moments/"+bm+"/comments", map[string]any{"text": "回复 a", "reply_to_user_id": a.ID}, nil)
	b.must("POST", "/v1/messages", map[string]any{"messages": []map[string]any{{"to": a.ID, "type": 0, "body": "QUJD"}}}, nil)
	a.must("POST", "/v1/blocks/"+c.ID, nil, nil)
	conn := a.dial()
	// 清空 b 的队列，之后只看删除产生的事件
	ackAll(b)

	// 二次确认：邮箱不符 → 400，什么都不变
	mod.expect("DELETE", "/admin/api/users/"+a.ID, map[string]any{"confirm_email": "wrong@example.com"}, 400, "bad_request")
	mod.expect("DELETE", "/admin/api/users/"+a.ID, nil, 400, "bad_request")
	a.must("GET", "/v1/me", nil, nil)
	if len(b.events()) != 0 {
		t.Fatalf("被拒绝的删除不应产生事件: %v", b.events())
	}

	mod.must("DELETE", "/admin/api/users/"+a.ID, map[string]any{"confirm_email": a.Email}, nil)
	conn.expectKicked("account_deleted")
	a.expect("GET", "/v1/me", nil, 401, "unauthorized")
	mod.expect("GET", "/admin/api/users/"+a.ID, nil, 404, "not_found")
	mod.expect("DELETE", "/admin/api/users/"+a.ID, map[string]any{"confirm_email": a.Email}, 404, "not_found")

	// 好友收到 friend_removed；好友列表里不再有 a
	if !b.hasEvent("friend_removed", func(ev map[string]any) bool { return ev["user_id"] == a.ID }) {
		t.Fatalf("b 应收到 friend_removed: %v", b.events())
	}
	var friends []map[string]any
	b.must("GET", "/v1/friends", nil, &friends)
	if len(friends) != 1 || friends[0]["id"] != c.ID {
		t.Fatalf("b 的好友列表不对: %v", friends)
	}
	// a 当群主的群：群主已转让给现有成员，成员收到 group_updated，群里不再有 a
	var g store.Group
	b.must("GET", "/v1/groups/"+owned, nil, &g)
	if g.OwnerID != b.ID && g.OwnerID != c.ID {
		t.Fatalf("群主应转让给现有成员: %+v", g)
	}
	ownerRole := ""
	for _, m := range g.Members {
		if m.UserID == a.ID {
			t.Fatal("群里不应还有已删除的用户")
		}
		if m.UserID == g.OwnerID {
			ownerRole = m.Role
		}
	}
	if len(g.Members) != 2 || ownerRole != store.RoleOwner {
		t.Fatalf("群成员 / 群主角色不对: %+v", g)
	}
	for _, gid := range []string{owned, joined} {
		if !b.hasEvent("group_updated", func(ev map[string]any) bool {
			grp, _ := ev["group"].(map[string]any)
			return grp != nil && grp["id"] == gid && len(grp["members"].([]any)) == 2
		}) {
			t.Fatalf("b 应收到群 %s 的 group_updated: %v", gid, b.events())
		}
	}
	// 只有 a 的群被解散
	mod.expect("GET", "/admin/api/groups/"+solo, nil, 404, "not_found")
	// 朋友圈、评论、点赞、队列、密钥、图片文件都已清理
	var feed []store.Moment
	b.must("GET", "/v1/moments/feed", nil, &feed)
	// 别人回复 a 的评论保留，但不再指向已删除的用户
	if len(feed) != 1 || feed[0].ID != bm || len(feed[0].Likes) != 0 || len(feed[0].Comments) != 1 ||
		feed[0].Comments[0].Author.ID != c.ID || feed[0].Comments[0].ReplyTo != nil {
		t.Fatalf("a 的动态 / 评论 / 点赞应已删除，回复 a 的评论应保留且 reply_to 为空: %+v", feed)
	}
	if _, err := e.st.GetMomentRow(context.Background(), mid); err == nil {
		t.Fatal("a 的动态应已删除")
	}
	if n, bytes, _ := e.st.QueueUsage(context.Background(), a.ID); n != 0 || bytes != 0 {
		t.Fatalf("a 的队列应已清空: %d %d", n, bytes)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "attachments", img)); !os.IsNotExist(err) {
		t.Fatalf("a 的朋友圈图片文件应已删除: %v", err)
	}
	b.expect("GET", "/v1/keys/identity/"+a.ID, nil, 404, "not_found")
	au := e.audited("user.delete", "user:"+a.ID)
	if !strings.Contains(au.Detail, a.Email) {
		t.Fatalf("审计应记录被删账号的邮箱: %+v", au)
	}
	// 同一邮箱可以重新注册，是全新的账号
	status, _, again := e.verify(a.Email, "")
	if status != 200 || again.ID == a.ID {
		t.Fatalf("重新注册应得到新账号: %d %s", status, again.ID)
	}
	var me map[string]any
	again.must("GET", "/v1/me", nil, &me)
	var groups []store.Group
	again.must("GET", "/v1/groups", nil, &groups)
	if len(groups) != 0 {
		t.Fatalf("新账号不应继承旧群: %v", groups)
	}
}

// ackAll 确认用户队列里的全部信封与事件。
func ackAll(u *user) {
	u.e.t.Helper()
	var res struct {
		Messages []map[string]any `json:"messages"`
		Events   []map[string]any `json:"events"`
	}
	u.must("GET", "/v1/messages?limit=500", nil, &res)
	ids := []string{}
	for _, m := range append(res.Messages, res.Events...) {
		ids = append(ids, m["id"].(string))
	}
	if len(ids) > 0 {
		u.must("POST", "/v1/messages/ack", map[string]any{"ids": ids}, nil)
	}
}

// 1x1 PNG
var pngBytes = func() []byte {
	var b []byte
	json.Unmarshal([]byte(`"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="`), &b)
	return b
}()
