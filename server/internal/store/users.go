package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Profile 是对外公开的用户资料（契约 3.1）。
type Profile struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	About     string  `json:"about"`
	AvatarID  *string `json:"avatar_id"`
	UpdatedAt int64   `json:"updated_at"`
	// Email 仅在查看者是本人或与该用户互为好友时填写（接口 v2 S-B），否则为空、序列化时省略。
	// 默认一律为空：只有 WithEmail / Friends / ProfilesWithEmail 这类明确的出口才会填。
	Email string `json:"email,omitempty"`
}

// User 是完整用户记录（含私有字段）。Email 是私有字段，Profile.Email 默认保持为空。
type User struct {
	Profile
	Email            string
	DeviceGeneration int64
}

// WithEmail 返回带邮箱的资料（调用方须已确认查看者是本人或好友）。
func (u User) WithEmail() Profile {
	p := u.Profile
	p.Email = u.Email
	return p
}

const userCols = "id, name, about, avatar_id, updated_at, email, device_generation"

func scanUser(sc interface{ Scan(...any) error }) (User, error) {
	var u User
	var avatar sql.NullString
	err := sc.Scan(&u.ID, &u.Name, &u.About, &avatar, &u.UpdatedAt, &u.Email, &u.DeviceGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if avatar.Valid {
		u.AvatarID = &avatar.String
	}
	return u, err
}

// LoginResult 是 Login 的结果。
type LoginResult struct {
	User  User
	IsNew bool
}

// Login 实现单设备登录语义（见 Tx.Login），独立事务。
func (s *Store) Login(ctx context.Context, email, platform, deviceName, tokenHash string) (LoginResult, error) {
	return update(s, ctx, func(t *Tx) (LoginResult, error) { return t.Login(email, platform, deviceName, tokenHash) })
}

// Login 实现单设备登录语义：不存在则创建用户；已存在则 device_generation+1，
// 作废旧 token、清空旧预密钥与未投递队列。随后写入新 token 哈希。
func (t *Tx) Login(email, platform, deviceName, tokenHash string) (LoginResult, error) {
	ctx, tx := t.ctx, t.tx
	var res LoginResult
	now := NowMillis()
	u, err := scanUser(tx.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE email = ?", email))
	switch {
	case errors.Is(err, ErrNotFound):
		res.IsNew = true
		u = User{Profile: Profile{ID: NewID(), Name: defaultName(email), UpdatedAt: now}, Email: email, DeviceGeneration: 1}
		_, err = tx.ExecContext(ctx, `INSERT INTO users (id, email, name, device_generation, platform, device_name, created_at, updated_at)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, u.ID, email, u.Name, platform, deviceName, now, now)
		if err != nil {
			return res, err
		}
	case err != nil:
		return res, err
	default:
		u.DeviceGeneration++
		t.InvalidateUserTokens(u.ID)
		stmts := []string{
			"DELETE FROM tokens WHERE user_id = ?",
			"DELETE FROM identity_keys WHERE user_id = ?",
			"DELETE FROM one_time_keys WHERE user_id = ?",
			"DELETE FROM fallback_keys WHERE user_id = ?",
			"DELETE FROM queue WHERE user_id = ?",
		}
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q, u.ID); err != nil {
				return res, err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET device_generation = ?, platform = ?, device_name = ? WHERE id = ?",
			u.DeviceGeneration, platform, deviceName, u.ID); err != nil {
			return res, err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO tokens (token_hash, user_id, created_at, last_used_at) VALUES (?, ?, ?, ?)", tokenHash, u.ID, now, now); err != nil {
		return res, err
	}
	res.User = u
	return res, nil
}

// defaultName 新用户默认昵称取邮箱 @ 前部分。
func defaultName(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// GetUser 按 id 查询。
func (s *Store) GetUser(ctx context.Context, id string) (User, error) { return getUser(ctx, s.db, id) }

// GetUser 在事务内按 id 查询。
func (t *Tx) GetUser(id string) (User, error) { return getUser(t.ctx, t.tx, id) }

func getUser(ctx context.Context, ex execer, id string) (User, error) {
	return scanUser(ex.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

// GetUserByEmail 按（已规范化的）邮箱查询。
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE email = ?", email))
}

// UserExists 判断用户是否存在。
func (s *Store) UserExists(ctx context.Context, id string) (bool, error) {
	return exists(ctx, s.db, "SELECT 1 FROM users WHERE id = ?", id)
}

// ProfileUpdate 描述可选字段更新；nil 表示不修改。AvatarID 指向空串表示清除头像。
type ProfileUpdate struct {
	Name     *string
	About    *string
	AvatarID *string
}

// UpdateProfile 更新资料并返回最新 Profile（不含邮箱），独立事务。
func (s *Store) UpdateProfile(ctx context.Context, id string, up ProfileUpdate) (Profile, error) {
	return update(s, ctx, func(t *Tx) (Profile, error) { return t.UpdateProfile(id, up) })
}

// UpdateProfile 更新资料并返回最新 Profile（不含邮箱：它会作为 profile_updated 发给非好友的共同群成员）。
func (t *Tx) UpdateProfile(id string, up ProfileUpdate) (Profile, error) {
	ctx, tx := t.ctx, t.tx
	if up.Name != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET name = ? WHERE id = ?", *up.Name, id); err != nil {
			return Profile{}, err
		}
	}
	if up.About != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET about = ? WHERE id = ?", *up.About, id); err != nil {
			return Profile{}, err
		}
	}
	if up.AvatarID != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE users SET avatar_id = ? WHERE id = ?", nullIfEmpty(*up.AvatarID), id); err != nil {
			return Profile{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET updated_at = ? WHERE id = ?", NowMillis(), id); err != nil {
		return Profile{}, err
	}
	u, err := getUser(ctx, tx, id)
	return u.Profile, err
}

// Profiles 批量查询资料（不含邮箱），返回 id→Profile。
func (s *Store) Profiles(ctx context.Context, ids []string) (map[string]Profile, error) {
	return profiles(ctx, s.db, ids, false)
}

// ProfilesWithEmail 批量查询资料并带上邮箱（仅用于查看者有权看到邮箱的场景，如好友申请双方）。
func (s *Store) ProfilesWithEmail(ctx context.Context, ids []string) (map[string]Profile, error) {
	return profiles(ctx, s.db, ids, true)
}

// ProfilesWithEmail 在事务内批量查询带邮箱的资料。
func (t *Tx) ProfilesWithEmail(ids []string) (map[string]Profile, error) {
	return profiles(t.ctx, t.tx, ids, true)
}

func profiles(ctx context.Context, ex execer, ids []string, withEmail bool) (map[string]Profile, error) {
	out := make(map[string]Profile, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := ex.QueryContext(ctx, "SELECT "+userCols+" FROM users WHERE id IN ("+placeholders(len(ids))+")", toArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		if withEmail {
			out[u.ID] = u.WithEmail()
		} else {
			out[u.ID] = u.Profile
		}
	}
	return out, rows.Err()
}

// Contacts 返回与用户相关的联系人：好友 ∪ 共同群成员（不含自己）。
func (s *Store) Contacts(ctx context.Context, userID string) ([]string, error) {
	return contacts(ctx, s.db, userID)
}

// Contacts 在事务内查询联系人（好友 ∪ 共同群成员）。
func (t *Tx) Contacts(userID string) ([]string, error) { return contacts(t.ctx, t.tx, userID) }

func contacts(ctx context.Context, ex execer, userID string) ([]string, error) {
	return queryIDs(ctx, ex, `
		SELECT friend_id FROM friendships WHERE user_id = ?
		UNION
		SELECT gm2.user_id FROM group_members gm1
		JOIN group_members gm2 ON gm1.group_id = gm2.group_id
		WHERE gm1.user_id = ? AND gm2.user_id <> ?`, userID, userID, userID)
}

func (s *Store) queryIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	return queryIDs(ctx, s.db, q, args...)
}

func queryIDs(ctx context.Context, ex execer, q string, args ...any) ([]string, error) {
	rows, err := ex.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
