package api_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type momentResp struct {
	ID     string `json:"id"`
	Author struct {
		ID string `json:"id"`
	} `json:"author"`
	Text  string `json:"text"`
	Media []struct {
		AttachmentID string `json:"attachment_id"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
	} `json:"media"`
	Visibility *struct {
		Mode    string   `json:"mode"`
		UserIDs []string `json:"user_ids"`
	} `json:"visibility"`
	Likes []struct {
		ID string `json:"id"`
	} `json:"likes"`
	Comments []struct {
		ID     string `json:"id"`
		Author struct {
			ID string `json:"id"`
		} `json:"author"`
		ReplyTo *struct {
			ID string `json:"id"`
		} `json:"reply_to"`
		Text string `json:"text"`
	} `json:"comments"`
	CreatedAt int64 `json:"created_at"`
}

func feedIDs(c *client, path string) map[string]momentResp {
	var list []momentResp
	c.must("GET", path, nil, &list)
	out := map[string]momentResp{}
	for _, m := range list {
		out[m.Text] = m
	}
	return out
}

func post(c *client, text, mode string, ids []string) momentResp {
	var m momentResp
	body := map[string]any{"text": text, "media": []any{}}
	if mode != "" {
		body["visibility"] = map[string]any{"mode": mode, "user_ids": ids}
	}
	c.must("POST", "/v1/moments", body, &m)
	return m
}

func TestMomentsVisibility(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("author")
	b := e.register("b")
	c := e.register("c")
	d := e.register("d")
	x := e.register("stranger")
	befriend(t, a, b)
	befriend(t, a, c)
	befriend(t, a, d)
	befriend(t, b, d) // B 与 D 是好友；B 与 C 不是

	var att struct {
		ID string `json:"id"`
	}
	a.must("POST", "/v1/attachments", []byte("jpeg"), &att)
	var withMedia momentResp
	a.must("POST", "/v1/moments", map[string]any{"text": "all", "media": []any{map[string]any{"attachment_id": att.ID, "width": 640, "height": 480}}}, &withMedia)
	if len(withMedia.Media) != 1 || withMedia.Media[0].Width != 640 || withMedia.Visibility == nil || withMedia.Visibility.Mode != "all" {
		t.Fatalf("发布结果异常: %+v", withMedia)
	}
	inc := post(a, "include-b", "include", []string{b.ID})
	post(a, "exclude-c", "exclude", []string{c.ID})
	a.expectErr("POST", "/v1/moments", map[string]any{"text": "x", "visibility": map[string]any{"mode": "weird"}}, 400, "bad_request")
	a.expectErr("POST", "/v1/moments", map[string]any{"text": ""}, 400, "bad_request")

	// 作者看到全部，且带 visibility
	af := feedIDs(a, "/v1/moments/feed")
	if len(af) != 3 || af["include-b"].Visibility == nil || af["include-b"].Visibility.Mode != "include" || af["include-b"].Visibility.UserIDs[0] != b.ID {
		t.Fatalf("作者 feed 异常: %+v", af)
	}
	// B：all + include-b + exclude-c
	bf := feedIDs(b, "/v1/moments/feed")
	if len(bf) != 3 {
		t.Fatalf("B 应看到 3 条: %v", keys(bf))
	}
	if bf["all"].Visibility != nil {
		t.Fatal("非作者不应看到 visibility 字段")
	}
	// C：all（include 名单外、exclude 名单内）
	if cf := feedIDs(c, "/v1/moments/feed"); len(cf) != 1 || cf["all"].ID == "" {
		t.Fatalf("C 应只看到 all: %v", keys(cf))
	}
	// D：all + exclude-c
	if df := feedIDs(d, "/v1/moments/feed"); len(df) != 2 || df["exclude-c"].ID == "" {
		t.Fatalf("D 应看到 all 与 exclude-c: %v", keys(df))
	}
	// 陌生人：feed 与个人页均为空
	if xf := feedIDs(x, "/v1/moments/feed"); len(xf) != 0 {
		t.Fatalf("陌生人 feed 应为空: %v", keys(xf))
	}
	if xu := feedIDs(x, "/v1/moments/user/"+a.ID); len(xu) != 0 {
		t.Fatalf("非好友个人页应为空: %v", keys(xu))
	}
	// 个人页：C 看 A
	if cu := feedIDs(c, "/v1/moments/user/"+a.ID); len(cu) != 1 {
		t.Fatalf("C 看 A 个人页应为 1 条: %v", keys(cu))
	}
	// 分页：limit=1 + before
	var page []momentResp
	b.must("GET", "/v1/moments/feed?limit=1", nil, &page)
	if len(page) != 1 || page[0].Text != "exclude-c" {
		t.Fatalf("分页第一页异常: %+v", page)
	}
	b.must("GET", "/v1/moments/feed?limit=10&before="+strconv.FormatInt(page[0].CreatedAt, 10), nil, &page)
	for _, m := range page {
		if m.Text == "exclude-c" {
			t.Fatal("before 应排除该时间及之后的动态")
		}
	}

	// 不可见的动态不能点赞/评论
	c.expectErr("POST", "/v1/moments/"+inc.ID+"/like", nil, 404, "not_found")
	x.expectErr("POST", "/v1/moments/"+withMedia.ID+"/comments", map[string]any{"text": "hi"}, 404, "not_found")

	aws := a.dial()
	aws.until("queue_empty", func(f frame) bool { return f.T == "queue_empty" })

	// 点赞与评论
	mid := withMedia.ID
	b.must("POST", "/v1/moments/"+mid+"/like", nil, nil)
	b.must("POST", "/v1/moments/"+mid+"/like", nil, nil) // 幂等
	c.must("POST", "/v1/moments/"+mid+"/like", nil, nil)
	n1 := aws.event("moment_notification")["notification"].(map[string]any)
	if n1["kind"] != "like" || n1["moment_id"] != mid || n1["from"].(map[string]any)["id"] != b.ID {
		t.Fatalf("点赞通知异常: %v", n1)
	}

	var cm map[string]any
	b.must("POST", "/v1/moments/"+mid+"/comments", map[string]any{"text": "B 的评论"}, &cm)
	if cm["author"].(map[string]any)["id"] != b.ID || cm["reply_to"] != nil || cm["text"] != "B 的评论" {
		t.Fatalf("评论响应异常: %v", cm)
	}
	var cc map[string]any
	c.must("POST", "/v1/moments/"+mid+"/comments", map[string]any{"text": "C 的评论"}, &cc)
	var reply map[string]any
	a.must("POST", "/v1/moments/"+mid+"/comments", map[string]any{"text": "回复 B", "reply_to_user_id": b.ID}, &reply)
	if reply["reply_to"].(map[string]any)["id"] != b.ID {
		t.Fatalf("回复评论异常: %v", reply)
	}
	d.must("POST", "/v1/moments/"+mid+"/comments", map[string]any{"text": "D 回复 B", "reply_to_user_id": b.ID}, nil)

	// 可见性过滤：只返回查看者本人或其好友的点赞/评论
	check := func(viewer *client, wantLikes []string, wantComments []string) {
		t.Helper()
		m := feedIDs(viewer, "/v1/moments/feed")["all"]
		var gotLikes, gotComments []string
		for _, l := range m.Likes {
			gotLikes = append(gotLikes, l.ID)
		}
		for _, c := range m.Comments {
			gotComments = append(gotComments, c.Text)
		}
		if !sameSet(gotLikes, wantLikes) || !equal(gotComments, wantComments) {
			t.Fatalf("查看者 %s：likes=%v comments=%v，期望 likes=%v comments=%v", viewer.Email, gotLikes, gotComments, wantLikes, wantComments)
		}
	}
	check(a, []string{b.ID, c.ID}, []string{"B 的评论", "C 的评论", "回复 B", "D 回复 B"})
	check(b, []string{b.ID}, []string{"B 的评论", "回复 B", "D 回复 B"})
	check(c, []string{c.ID}, []string{"C 的评论", "回复 B"})
	check(d, []string{b.ID}, []string{"B 的评论", "回复 B", "D 回复 B"})

	// 通知：A 收到 2 个赞 + 3 条评论；B 收到 A、D 的回复
	var an []map[string]any
	a.must("GET", "/v1/moments/notifications", nil, &an)
	if len(an) != 5 {
		t.Fatalf("A 的通知数应为 5: %d %v", len(an), an)
	}
	if an[0]["kind"] != "comment" || an[0]["text"] != "D 回复 B" {
		t.Fatalf("通知应按时间倒序: %v", an[0])
	}
	var bn []map[string]any
	b.must("GET", "/v1/moments/notifications", nil, &bn)
	if len(bn) != 2 {
		t.Fatalf("B 应收到 2 条回复通知: %v", bn)
	}
	for _, n := range bn {
		if n["kind"] != "comment" || n["moment_id"] != mid {
			t.Fatalf("B 的通知异常: %v", n)
		}
	}

	// 取消点赞 → 点赞与其通知消失（DELETE /v1/moments/{id}/like 路由）
	c.must("DELETE", "/v1/moments/"+mid+"/like", nil, nil)
	check(a, []string{b.ID}, []string{"B 的评论", "C 的评论", "回复 B", "D 回复 B"})
	a.must("GET", "/v1/moments/notifications", nil, &an)
	if len(an) != 4 {
		t.Fatalf("取消点赞后通知数应为 4: %d", len(an))
	}

	// 删除评论：他人不可删；动态作者可删（DELETE /v1/moments/comments/{cid} 路由）
	d.expectErr("DELETE", "/v1/moments/comments/"+cc["id"].(string), nil, 403, "forbidden")
	a.must("DELETE", "/v1/moments/comments/"+cc["id"].(string), nil, nil)
	b.must("DELETE", "/v1/moments/comments/"+cm["id"].(string), nil, nil) // 评论作者自删
	check(a, []string{b.ID}, []string{"回复 B", "D 回复 B"})

	// 删除动态：仅作者
	b.expectErr("DELETE", "/v1/moments/"+mid, nil, 403, "forbidden")
	a.must("DELETE", "/v1/moments/"+mid, nil, nil)
	if af := feedIDs(a, "/v1/moments/feed"); len(af) != 2 {
		t.Fatalf("删除后作者应剩 2 条: %v", keys(af))
	}
	a.must("GET", "/v1/moments/notifications", nil, &an)
	if len(an) != 0 {
		t.Fatalf("删除动态后相关通知应清除: %v", an)
	}
	raw, _ := json.Marshal(an)
	if string(raw) != "[]" {
		t.Fatalf("空列表应序列化为 []: %s", raw)
	}
}

func keys(m map[string]momentResp) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// 分页参数 before_id：用上一页最后一条的 (created_at, id) 翻页，取全且不重复。
func TestMomentsPagingBeforeID(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	for i := 0; i < 4; i++ {
		a.must("POST", "/v1/moments", map[string]any{"text": fmt.Sprintf("m%d", i)}, nil)
	}
	type item struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
	}
	seen := map[string]bool{}
	path := "/v1/moments/feed?limit=1"
	for i := 0; i < 6; i++ {
		var page []item
		a.must("GET", path, nil, &page)
		if len(page) == 0 {
			break
		}
		if seen[page[0].ID] {
			t.Fatalf("重复: %s", page[0].ID)
		}
		seen[page[0].ID] = true
		path = fmt.Sprintf("/v1/moments/feed?limit=1&before=%d&before_id=%s", page[0].CreatedAt, page[0].ID)
	}
	if len(seen) != 4 {
		t.Fatalf("应取到 4 条，得到 %d", len(seen))
	}
	a.expectErr("GET", "/v1/moments/feed?before_id="+strings.Repeat("x", 65), nil, 400, "bad_request")
}
