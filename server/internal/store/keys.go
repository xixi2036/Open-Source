package store

import (
	"context"
	"database/sql"
	"errors"
)

// MaxOneTimeKeys 是每个用户在服务端保留的一次性密钥总量上限（客户端的目标存量是 100）。
// 超出时丢弃最早上传的：上传接口没有别的总量限制，否则可以无限灌库。
const MaxOneTimeKeys = 200

// SignedKey 是一次性密钥或 fallback 密钥（契约 3.2）。
type SignedKey struct {
	KeyID     string `json:"key_id"`
	Key       string `json:"key"`
	Signature string `json:"signature"`
}

// IdentityKeys 是用户的长期身份公钥。
type IdentityKeys struct {
	UserID           string `json:"user_id"`
	IdentityKey      string `json:"identity_key"`
	SigningKey       string `json:"signing_key"`
	DeviceGeneration int64  `json:"device_generation"`
}

// ClaimedKey 是 claim 返回的一次性（或 fallback）密钥。
type ClaimedKey struct {
	SignedKey
	Fallback bool `json:"fallback"`
}

// KeyUpload 是 PUT /v1/keys 的内容。
type KeyUpload struct {
	IdentityKey string
	SigningKey  string
	OneTimeKeys []SignedKey
	FallbackKey *SignedKey
	TokenHash   string // 上传者所用 token：设备代在事务内按它确定
}

// UploadKeys 写入身份密钥（同一 generation 内不可变）、追加 OTK、替换 fallback。返回剩余 OTK 数。
// 设备代在事务内由 up.TokenHash 确定：token 已被重新登录作废时返回 ErrNotFound，
// 避免旧设备在重新登录的同时补充 OTK，把旧身份密钥以新设备代写入。
func (s *Store) UploadKeys(ctx context.Context, userID string, up KeyUpload) (int, error) {
	var count int
	err := s.withTx(ctx, func(tx dbtx) error {
		var curGen int64
		err := tx.QueryRowContext(ctx, `SELECT u.device_generation FROM tokens t JOIN users u ON u.id = t.user_id
			WHERE t.token_hash = ? AND t.user_id = ?`, up.TokenHash, userID).Scan(&curGen)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var ik, sk string
		var gen int64
		err = tx.QueryRowContext(ctx, "SELECT identity_key, signing_key, device_generation FROM identity_keys WHERE user_id = ?", userID).Scan(&ik, &sk, &gen)
		switch {
		case errors.Is(err, sql.ErrNoRows) || (err == nil && gen != curGen):
			if _, err := tx.ExecContext(ctx, `INSERT INTO identity_keys (user_id, identity_key, signing_key, device_generation) VALUES (?, ?, ?, ?)
				ON CONFLICT(user_id) DO UPDATE SET identity_key = excluded.identity_key, signing_key = excluded.signing_key, device_generation = excluded.device_generation`,
				userID, up.IdentityKey, up.SigningKey, curGen); err != nil {
				return err
			}
		case err != nil:
			return err
		case ik != up.IdentityKey || sk != up.SigningKey:
			return ErrConflict
		}
		for _, k := range up.OneTimeKeys {
			if _, err := tx.ExecContext(ctx, `INSERT INTO one_time_keys (user_id, key_id, key, signature) VALUES (?, ?, ?, ?)
				ON CONFLICT(user_id, key_id) DO UPDATE SET key = excluded.key, signature = excluded.signature`,
				userID, k.KeyID, k.Key, k.Signature); err != nil {
				return err
			}
		}
		if up.FallbackKey != nil {
			k := up.FallbackKey
			if _, err := tx.ExecContext(ctx, `INSERT INTO fallback_keys (user_id, key_id, key, signature) VALUES (?, ?, ?, ?)
				ON CONFLICT(user_id) DO UPDATE SET key_id = excluded.key_id, key = excluded.key, signature = excluded.signature`,
				userID, k.KeyID, k.Key, k.Signature); err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM one_time_keys WHERE user_id = ?", userID).Scan(&count); err != nil {
			return err
		}
		if count > MaxOneTimeKeys {
			// 超出总量上限：丢弃最早上传的，保留最新的（客户端手里一定还有它们的私钥）
			if _, err := tx.ExecContext(ctx, `DELETE FROM one_time_keys WHERE user_id = ?1 AND seq NOT IN (
				SELECT seq FROM one_time_keys WHERE user_id = ?1 ORDER BY seq DESC LIMIT ?2)`, userID, MaxOneTimeKeys); err != nil {
				return err
			}
			count = MaxOneTimeKeys
		}
		return nil
	})
	return count, err
}

// CountOneTimeKeys 返回剩余 OTK 数。
func (s *Store) CountOneTimeKeys(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM one_time_keys WHERE user_id = ?", userID).Scan(&n)
	return n, err
}

// GetIdentity 查询身份公钥；未上传返回 ErrNotFound。
func (s *Store) GetIdentity(ctx context.Context, userID string) (IdentityKeys, error) {
	return getIdentity(ctx, s.db, userID)
}

func getIdentity(ctx context.Context, ex execer, userID string) (IdentityKeys, error) {
	k := IdentityKeys{UserID: userID}
	err := ex.QueryRowContext(ctx, "SELECT identity_key, signing_key, device_generation FROM identity_keys WHERE user_id = ?", userID).
		Scan(&k.IdentityKey, &k.SigningKey, &k.DeviceGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

// ClaimKey 取出一个 OTK（删除）；耗尽时返回 fallback（不删除）；都没有返回 ErrNotFound。
func (s *Store) ClaimKey(ctx context.Context, userID string) (IdentityKeys, ClaimedKey, error) {
	var id IdentityKeys
	var ck ClaimedKey
	err := s.withTx(ctx, func(tx dbtx) error {
		var err error
		if id, err = getIdentity(ctx, tx, userID); err != nil {
			return err
		}
		var seq int64
		err = tx.QueryRowContext(ctx, "SELECT seq, key_id, key, signature FROM one_time_keys WHERE user_id = ? ORDER BY seq LIMIT 1", userID).
			Scan(&seq, &ck.KeyID, &ck.Key, &ck.Signature)
		if err == nil {
			_, err = tx.ExecContext(ctx, "DELETE FROM one_time_keys WHERE seq = ?", seq)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ck.Fallback = true
		err = tx.QueryRowContext(ctx, "SELECT key_id, key, signature FROM fallback_keys WHERE user_id = ?", userID).
			Scan(&ck.KeyID, &ck.Key, &ck.Signature)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, ck, err
}
