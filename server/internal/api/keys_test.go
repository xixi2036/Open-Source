package api_test

import (
	"fmt"
	"testing"
)

func keyUpload(ik, sk string, otks int, fallback bool) map[string]any {
	list := []map[string]string{}
	for i := 0; i < otks; i++ {
		list = append(list, map[string]string{"key_id": fmt.Sprintf("%s-otk-%d", ik, i), "key": fmt.Sprintf("key-%s-%d", ik, i), "signature": "sig"})
	}
	body := map[string]any{"identity_key": ik, "signing_key": sk, "one_time_keys": list}
	if fallback {
		body["fallback_key"] = map[string]string{"key_id": ik + "-fb", "key": "fallback-" + ik, "signature": "sig-fb"}
	}
	return body
}

type claimResp struct {
	UserID           string `json:"user_id"`
	IdentityKey      string `json:"identity_key"`
	SigningKey       string `json:"signing_key"`
	DeviceGeneration int64  `json:"device_generation"`
	OneTimeKey       struct {
		KeyID     string `json:"key_id"`
		Key       string `json:"key"`
		Signature string `json:"signature"`
		Fallback  bool   `json:"fallback"`
	} `json:"one_time_key"`
}

func TestKeysUploadCountClaim(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	befriend(t, a, b)

	// 未上传时 claim / identity 返回 404
	b.expectErr("POST", "/v1/keys/claim/"+a.ID, nil, 404, "not_found")
	b.expectErr("GET", "/v1/keys/identity/"+a.ID, nil, 404, "not_found")
	// 缺少必填字段
	a.expectErr("PUT", "/v1/keys", map[string]any{"identity_key": "x"}, 400, "bad_request")

	var cnt map[string]int
	a.must("PUT", "/v1/keys", keyUpload("IK", "SK", 3, true), &cnt)
	if cnt["one_time_key_count"] != 3 {
		t.Fatalf("上传后应有 3 个 OTK: %v", cnt)
	}
	a.must("GET", "/v1/keys/count", nil, &cnt)
	if cnt["one_time_key_count"] != 3 {
		t.Fatalf("count 应为 3: %v", cnt)
	}

	// 连续 claim 消耗 OTK，每个只发一次
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		var c claimResp
		b.must("POST", "/v1/keys/claim/"+a.ID, nil, &c)
		if c.UserID != a.ID || c.IdentityKey != "IK" || c.SigningKey != "SK" || c.DeviceGeneration != 1 {
			t.Fatalf("claim 响应异常: %+v", c)
		}
		if c.OneTimeKey.Fallback || seen[c.OneTimeKey.KeyID] || c.OneTimeKey.Signature != "sig" {
			t.Fatalf("OTK 异常或重复: %+v", c.OneTimeKey)
		}
		seen[c.OneTimeKey.KeyID] = true
	}
	a.must("GET", "/v1/keys/count", nil, &cnt)
	if cnt["one_time_key_count"] != 0 {
		t.Fatalf("OTK 应耗尽: %v", cnt)
	}

	// 耗尽后返回 fallback（不删除，可重复返回）
	for i := 0; i < 2; i++ {
		var c claimResp
		b.must("POST", "/v1/keys/claim/"+a.ID, nil, &c)
		if !c.OneTimeKey.Fallback || c.OneTimeKey.KeyID != "IK-fb" || c.OneTimeKey.Key != "fallback-IK" || c.OneTimeKey.Signature != "sig-fb" {
			t.Fatalf("期望 fallback key: %+v", c.OneTimeKey)
		}
	}

	// 身份密钥不可变
	a.expectErr("PUT", "/v1/keys", keyUpload("IK-OTHER", "SK", 1, false), 409, "conflict")
	// 相同身份可继续补充 OTK（追加语义）
	a.must("PUT", "/v1/keys", keyUpload("IK", "SK", 2, false), &cnt)
	a.must("PUT", "/v1/keys", map[string]any{"identity_key": "IK", "signing_key": "SK",
		"one_time_keys": []map[string]string{{"key_id": "extra", "key": "k", "signature": "s"}}}, &cnt)
	if cnt["one_time_key_count"] != 3 {
		t.Fatalf("追加后应有 3 个 OTK: %v", cnt)
	}

	var id map[string]any
	b.must("GET", "/v1/keys/identity/"+a.ID, nil, &id)
	if id["user_id"] != a.ID || id["identity_key"] != "IK" || id["signing_key"] != "SK" || id["device_generation"].(float64) != 1 {
		t.Fatalf("identity 响应异常: %v", id)
	}

	// 不存在用户：不是好友，403
	b.expectErr("POST", "/v1/keys/claim/00000000-0000-4000-8000-000000000000", nil, 403, "forbidden")
}

// claim 只允许好友或共同群成员，并按 (调用者, 目标) 限速。
func TestClaimRequiresRelationAndRateLimited(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	c := e.register("c")
	x := e.register("stranger")
	a.must("PUT", "/v1/keys", keyUpload("IKA", "SKA", 30, true), nil)

	// 陌生人、本人都不行
	x.expectErr("POST", "/v1/keys/claim/"+a.ID, nil, 403, "forbidden")
	a.expectErr("POST", "/v1/keys/claim/"+a.ID, nil, 403, "forbidden")

	// 非好友但同群：可以
	befriend(t, a, b)
	befriend(t, b, c)
	var g groupResp
	b.must("POST", "/v1/groups", map[string]any{"name": "g", "member_ids": []string{a.ID, c.ID}}, &g)
	c.must("POST", "/v1/keys/claim/"+a.ID, nil, nil)

	// 好友：每分钟 10 次，第 11 次 429；换一个调用者不受影响
	for i := 0; i < 10; i++ {
		b.must("POST", "/v1/keys/claim/"+a.ID, nil, nil)
	}
	b.expectErr("POST", "/v1/keys/claim/"+a.ID, nil, 429, "rate_limited")
	c.must("POST", "/v1/keys/claim/"+a.ID, nil, nil)

	// 退群后不再是同群成员
	c.must("DELETE", "/v1/groups/"+g.ID+"/members/"+c.ID, nil, nil)
	c.expectErr("POST", "/v1/keys/claim/"+a.ID, nil, 403, "forbidden")
}

// 被拉黑（任一方向）后不能再 claim 对方的预密钥，与 Olm 发送的判定一致（审查 S4 的复现）。
func TestClaimForbiddenWhenBlocked(t *testing.T) {
	e := newEnv(t, true)
	a := e.register("a")
	b := e.register("b")
	b.must("PUT", "/v1/keys", keyUpload("IKB", "SKB", 5, true), nil)
	a.must("PUT", "/v1/keys", keyUpload("IKA", "SKA", 5, true), nil)
	befriend(t, a, b)
	a.must("POST", "/v1/keys/claim/"+b.ID, nil, nil)

	b.must("POST", "/v1/blocks/"+a.ID, nil, nil)
	a.expectErr("POST", "/v1/keys/claim/"+b.ID, nil, 403, "forbidden") // 被拉黑方
	b.expectErr("POST", "/v1/keys/claim/"+a.ID, nil, 403, "forbidden") // 拉黑方自己也不行（与发消息一致）
	var sr sendResp
	a.must("POST", "/v1/messages", olmMsg(b.ID, "x", true), &sr)
	if sr.Results[0].Error != "blocked" {
		t.Fatalf("发消息应同样被拒: %+v", sr)
	}
	var cnt struct {
		N int `json:"one_time_key_count"`
	}
	b.must("GET", "/v1/keys/count", nil, &cnt)
	if cnt.N != 4 {
		t.Fatalf("拉黑后 OTK 不应再被消耗: %d", cnt.N)
	}

	b.must("DELETE", "/v1/blocks/"+a.ID, nil, nil)
	a.must("POST", "/v1/keys/claim/"+b.ID, nil, nil)
}
