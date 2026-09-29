package api_test

import (
	"testing"
)

type groupResp struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	AvatarID     *string `json:"avatar_id"`
	Announcement string  `json:"announcement"`
	OwnerID      string  `json:"owner_id"`
	Members      []struct {
		UserID   string `json:"user_id"`
		Role     string `json:"role"`
		JoinedAt int64  `json:"joined_at"`
	} `json:"members"`
	UpdatedAt int64 `json:"updated_at"`
}

func (g groupResp) role(uid string) string {
	for _, m := range g.Members {
		if m.UserID == uid {
			return m.Role
		}
	}
	return ""
}

func megolm(body string) map[string]any {
	return map[string]any{"session_id": "sess-1", "body": body, "client_id": "c-" + body, "urgent": true}
}

func TestGroupsLifecycleAndFanout(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("owner")
	b := e.register("b")
	c := e.register("c")
	d := e.register("d")
	x := e.register("stranger")
	befriend(t, a, b)
	befriend(t, a, c)
	befriend(t, a, d)

	bws, cws := b.dial(), c.dial()
	bws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })
	cws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	// 成员必须是创建者好友
	a.expectErr("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID, x.ID}}, 403, "not_friends")
	a.expectErr("POST", "/v1/groups", map[string]any{"name": "", "member_ids": []string{b.ID}}, 400, "bad_request")

	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "周末爬山", "member_ids": []string{b.ID, c.ID}}, &g)
	if g.ID == "" || g.OwnerID != a.ID || len(g.Members) != 3 || g.role(a.ID) != "owner" || g.role(b.ID) != "member" || g.AvatarID != nil {
		t.Fatalf("建群结果异常: %+v", g)
	}
	if ev := bws.event("group_updated"); ev["group"].(map[string]any)["id"] != g.ID {
		t.Fatalf("B 应收到 group_updated: %v", ev)
	}
	cws.event("group_updated")

	var list []groupResp
	b.must("GET", "/v1/groups", nil, &list)
	if len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("B 的群列表异常: %+v", list)
	}
	x.expectErr("GET", "/v1/groups/"+g.ID, nil, 403, "forbidden")
	x.expectErr("GET", "/v1/groups/00000000-0000-4000-8000-000000000000", nil, 404, "not_found")

	// 群消息扇出：除发送者外全体成员
	var mr map[string]any
	a.must("POST", "/v1/groups/"+g.ID+"/messages", megolm("hello-all"), &mr)
	if mr["id"] == nil || mr["server_ts"] == nil {
		t.Fatalf("群消息响应异常: %v", mr)
	}
	for _, w := range []*wsClient{bws, cws} {
		env := w.until("群信封", func(f frame) bool { return f.T == "envelope" }).envelope()
		if env.Kind != "megolm" || env.From != a.ID || env.GroupID == nil || *env.GroupID != g.ID ||
			env.SessionID == nil || *env.SessionID != "sess-1" || env.Body != "hello-all" || env.ID != mr["id"] {
			t.Fatalf("megolm 信封异常: %+v", env)
		}
	}
	var inbox inboxResp
	a.must("GET", "/v1/messages", nil, &inbox)
	for _, m := range inbox.Messages {
		if m.Kind == "megolm" {
			t.Fatal("发送者不应收到自己的群消息")
		}
	}
	x.expectErr("POST", "/v1/groups/"+g.ID+"/messages", megolm("intrude"), 403, "forbidden")

	// 非管理员不能加人/改资料
	b.expectErr("POST", "/v1/groups/"+g.ID+"/members", map[string]any{"user_ids": []string{d.ID}}, 403, "forbidden")
	b.expectErr("PUT", "/v1/groups/"+g.ID, map[string]any{"name": "x"}, 403, "forbidden")

	// 管理员加人（须为操作者好友）
	a.expectErr("POST", "/v1/groups/"+g.ID+"/members", map[string]any{"user_ids": []string{x.ID}}, 403, "not_friends")
	a.must("POST", "/v1/groups/"+g.ID+"/members", map[string]any{"user_ids": []string{d.ID}}, &g)
	if len(g.Members) != 4 || g.role(d.ID) != "member" {
		t.Fatalf("加人后成员异常: %+v", g)
	}
	var dInbox inboxResp
	d.must("GET", "/v1/messages", nil, &dInbox)
	foundUpdate := false
	for _, ev := range dInbox.Events {
		if ev["type"] == "group_updated" {
			foundUpdate = true
		}
	}
	if !foundUpdate {
		t.Fatal("新成员应收到 group_updated")
	}

	// 共同在群内的非好友之间允许 Olm（用于群密钥分发）
	var sr sendResp
	b.must("POST", "/v1/messages", olmMsg(d.ID, "room_key", false), &sr)
	if sr.Results[0].Error != "" {
		t.Fatalf("同群成员之间应允许 Olm: %+v", sr)
	}

	// 修改群资料
	a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"name": "新名字", "announcement": "周六 8 点集合"}, &g)
	if g.Name != "新名字" || g.Announcement != "周六 8 点集合" {
		t.Fatalf("修改群资料异常: %+v", g)
	}
	bws.until("群改名", func(f frame) bool {
		ev := f.event()
		return f.T == "event" && ev["type"] == "group_updated" && ev["group"].(map[string]any)["name"] == "新名字"
	})

	// 踢人：C 收到 group_removed，其余成员收到 group_updated
	a.must("DELETE", "/v1/groups/"+g.ID+"/members/"+c.ID, nil, nil)
	if ev := cws.event("group_removed"); ev["group_id"] != g.ID {
		t.Fatalf("group_removed 异常: %v", ev)
	}
	bws.until("踢人后的 group_updated", func(f frame) bool {
		ev := f.event()
		return f.T == "event" && ev["type"] == "group_updated" && len(ev["group"].(map[string]any)["members"].([]any)) == 3
	})
	c.expectErr("POST", "/v1/groups/"+g.ID+"/messages", megolm("still-here?"), 403, "forbidden")

	// 被踢者不再收到群消息
	a.must("POST", "/v1/groups/"+g.ID+"/messages", megolm("after-kick"), nil)
	bws.until("踢人后群消息", func(f frame) bool { return f.T == "envelope" && f.envelope().Body == "after-kick" })
	var cInbox inboxResp
	c.must("GET", "/v1/messages", nil, &cInbox)
	for _, m := range cInbox.Messages {
		if m.Body == "after-kick" {
			t.Fatal("被移除的成员不应收到群消息")
		}
	}

	// 管理员设置：仅群主
	b.expectErr("PUT", "/v1/groups/"+g.ID+"/admins/"+d.ID, map[string]any{"admin": true}, 403, "forbidden")
	a.must("PUT", "/v1/groups/"+g.ID+"/admins/"+b.ID, map[string]any{"admin": true}, &g)
	if g.role(b.ID) != "admin" {
		t.Fatalf("设置管理员失败: %+v", g)
	}
	// 管理员可踢普通成员，不能踢群主
	b.expectErr("DELETE", "/v1/groups/"+g.ID+"/members/"+a.ID, nil, 403, "forbidden")
	b.must("DELETE", "/v1/groups/"+g.ID+"/members/"+d.ID, nil, nil)

	// 群主退群 → 群主转让给管理员 B
	a.must("DELETE", "/v1/groups/"+g.ID+"/members/"+a.ID, nil, nil)
	b.must("GET", "/v1/groups/"+g.ID, nil, &g)
	if g.OwnerID != b.ID || g.role(b.ID) != "owner" || len(g.Members) != 1 {
		t.Fatalf("群主转让异常: %+v", g)
	}
	// 最后一人退群 → 群解散
	b.must("DELETE", "/v1/groups/"+g.ID+"/members/"+b.ID, nil, nil)
	b.expectErr("GET", "/v1/groups/"+g.ID, nil, 404, "not_found")
}

// 修改群头像须引用已上传的附件；空串清除。
func TestGroupAvatarMustExist(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)
	var g groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{b.ID}}, &g)
	a.expectErr("PUT", "/v1/groups/"+g.ID, map[string]any{"avatar_id": "00000000-0000-4000-8000-000000000000"}, 404, "not_found")
	var up struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/attachments", []byte("png"), &up)
	a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"avatar_id": up.ID}, &g)
	if g.AvatarID == nil || *g.AvatarID != up.ID {
		t.Fatalf("群头像应已更新: %+v", g.AvatarID)
	}
	a.must("PUT", "/v1/groups/"+g.ID, map[string]any{"avatar_id": ""}, &g)
	if g.AvatarID != nil {
		t.Fatalf("空串应清除群头像: %v", *g.AvatarID)
	}
}

// 群消息扇出跳过拉黑了发送者的成员；群列表一次取回多个群及各自成员。
func TestMegolmSkipsBlockersAndListGroups(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	c := e.register("c")
	befriend(t, a, b)
	befriend(t, a, c)
	var g1, g2 groupResp
	a.must("POST", "/v1/groups", map[string]any{"name": "g1", "member_ids": []string{b.ID, c.ID}}, &g1)
	a.must("POST", "/v1/groups", map[string]any{"name": "g2", "member_ids": []string{b.ID}}, &g2)
	c.must("POST", "/v1/blocks/"+a.ID, nil, nil)

	a.must("POST", "/v1/groups/"+g1.ID+"/messages", megolm("from-a"), nil)
	b.must("POST", "/v1/groups/"+g1.ID+"/messages", megolm("from-b"), nil)
	var inbox inboxResp
	b.must("GET", "/v1/messages?limit=500", nil, &inbox)
	if len(inbox.Messages) != 1 {
		t.Fatalf("b 应收到 a 的群消息: %d", len(inbox.Messages))
	}
	c.must("GET", "/v1/messages?limit=500", nil, &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].From != b.ID {
		t.Fatalf("c 拉黑了 a：只应收到 b 的群消息: %+v", inbox.Messages)
	}

	var list []groupResp
	b.must("GET", "/v1/groups", nil, &list)
	if len(list) != 2 || list[0].ID != g1.ID || list[1].ID != g2.ID || len(list[0].Members) != 3 || len(list[1].Members) != 2 {
		t.Fatalf("群列表异常: %+v", list)
	}
	c.must("GET", "/v1/groups", nil, &list)
	if len(list) != 1 || list[0].ID != g1.ID {
		t.Fatalf("c 只在 g1: %+v", list)
	}
}
