package api

import (
	"errors"
	"hush/server/internal/logx"
	"net/http"
	"time"

	"hush/server/internal/store"
)

const (
	maxKeyLen      = 512
	maxOTKPerBatch = 500
)

func validSignedKey(k store.SignedKey) bool {
	return k.KeyID != "" && k.Key != "" && k.Signature != "" &&
		len(k.KeyID) <= maxKeyLen && len(k.Key) <= maxKeyLen && len(k.Signature) <= maxKeyLen
}

// PUT /v1/keys → {one_time_key_count}
func (s *Server) handleUploadKeys(w http.ResponseWriter, r *http.Request, uid string) {
	if !s.allowWrite(w, uid, "keys") {
		return
	}
	var req struct {
		IdentityKey string            `json:"identity_key"`
		SigningKey  string            `json:"signing_key"`
		OneTimeKeys []store.SignedKey `json:"one_time_keys"`
		FallbackKey *store.SignedKey  `json:"fallback_key"`
	}
	if !decode(w, r, &req, 0) {
		return
	}
	if req.IdentityKey == "" || req.SigningKey == "" || len(req.IdentityKey) > maxKeyLen || len(req.SigningKey) > maxKeyLen {
		fail(w, codeBadRequest, "identity_key 与 signing_key 必填")
		return
	}
	if len(req.OneTimeKeys) > maxOTKPerBatch {
		fail(w, codeBadRequest, "单次最多上传 500 个 one_time_keys")
		return
	}
	for _, k := range req.OneTimeKeys {
		if !validSignedKey(k) {
			fail(w, codeBadRequest, "one_time_keys 条目须包含 key_id、key、signature")
			return
		}
	}
	if req.FallbackKey != nil && !validSignedKey(*req.FallbackKey) {
		fail(w, codeBadRequest, "fallback_key 须包含 key_id、key、signature")
		return
	}
	n, err := s.st.UploadKeys(r.Context(), uid, store.KeyUpload{
		IdentityKey: req.IdentityKey, SigningKey: req.SigningKey,
		OneTimeKeys: req.OneTimeKeys, FallbackKey: req.FallbackKey,
		TokenHash: tokenHash(r),
	})
	if errors.Is(err, store.ErrNotFound) {
		fail(w, codeUnauthorized, "token 无效或已失效")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		fail(w, codeConflict, "当前设备代的身份密钥已存在且不同，不可修改")
		return
	}
	if err != nil {
		internalErr(w, r, err)
		return
	}
	logx.Debugf("[keys] user=%s 上传 %d 个 OTK（fallback=%v），剩余 %d", uid, len(req.OneTimeKeys), req.FallbackKey != nil, n)
	ok(w, obj{"one_time_key_count": n})
}

// GET /v1/keys/count → {one_time_key_count}
func (s *Server) handleKeyCount(w http.ResponseWriter, r *http.Request, uid string) {
	n, err := s.st.CountOneTimeKeys(r.Context(), uid)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	ok(w, obj{"one_time_key_count": n})
}

// POST /v1/keys/claim/{user_id}
func (s *Server) handleClaimKey(w http.ResponseWriter, r *http.Request, uid string) {
	target := r.PathValue("user_id")
	// 只有能向对方发 Olm 消息的人（好友或共同群成员，且双方都没有拉黑）才能 claim：
	// 否则陌生人或被拉黑的人能反复 claim 耗尽他人的 OTK。
	code, err := s.canMessage(r.Context(), uid, target)
	if err != nil {
		internalErr(w, r, err)
		return
	}
	if code != "" {
		fail(w, codeForbidden, "只能获取好友或共同群成员的预密钥")
		return
	}
	if !s.claimLimiter.allow(uid+"|"+target, time.Now()) {
		logx.Debugf("[keys] user=%s claim %s 过于频繁", uid, target)
		fail(w, codeRateLimited, "获取预密钥过于频繁，请稍后再试")
		return
	}
	id, key, err := s.st.ClaimKey(r.Context(), target)
	if err != nil {
		storeErr(w, r, err, "该用户没有可用的预密钥")
		return
	}
	logx.Debugf("[keys] user=%s claim %s 的密钥（fallback=%v）", uid, target, key.Fallback)
	ok(w, obj{
		"user_id":           id.UserID,
		"identity_key":      id.IdentityKey,
		"signing_key":       id.SigningKey,
		"device_generation": id.DeviceGeneration,
		"one_time_key":      key,
	})
}

// GET /v1/keys/identity/{user_id}
func (s *Server) handleGetIdentity(w http.ResponseWriter, r *http.Request, uid string) {
	id, err := s.st.GetIdentity(r.Context(), r.PathValue("user_id"))
	if err != nil {
		storeErr(w, r, err, "该用户尚未上传身份密钥")
		return
	}
	ok(w, id)
}
