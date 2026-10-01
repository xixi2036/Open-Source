package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 管理员角色（管理后台 A4）。
const (
	AdminOwner     = "owner"
	AdminModerator = "moderator"
	AdminViewer    = "viewer"
)

// ErrLastOwner 操作会让系统里不再有可用的 owner。
var ErrLastOwner = errors.New("last owner")

// Admin 是管理后台账号。PasswordHash 不会被序列化。
type Admin struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	CreatedAt    int64  `json:"created_at"`
	LastLoginAt  int64  `json:"last_login_at"`
	FailedCount  int    `json:"-"`
	LockedUntil  int64  `json:"locked_until"`
	PasswordHash string `json:"-"`
}

const adminCols = "id, username, password_hash, role, disabled, created_at, last_login_at, failed_count, locked_until"

func scanAdmin(sc interface{ Scan(...any) error }) (Admin, error) {
	var a Admin
	err := sc.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.Disabled, &a.CreatedAt, &a.LastLoginAt, &a.FailedCount, &a.LockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// ValidAdminRole 判断角色名是否合法。
func ValidAdminRole(role string) bool {
	return role == AdminOwner || role == AdminModerator || role == AdminViewer
}

// CreateAdmin 新建管理员；用户名已存在返回 ErrConflict。
func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash, role string) (Admin, error) {
	a := Admin{ID: NewID(), Username: username, PasswordHash: passwordHash, Role: role, CreatedAt: NowMillis()}
	_, err := s.db.ExecContext(ctx, "INSERT INTO admins (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)",
		a.ID, a.Username, a.PasswordHash, a.Role, a.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return a, ErrConflict
	}
	return a, err
}

// GetAdmin 按 id 查询管理员。
func (s *Store) GetAdmin(ctx context.Context, id string) (Admin, error) {
	return scanAdmin(s.db.QueryRowContext(ctx, "SELECT "+adminCols+" FROM admins WHERE id = ?", id))
}

// GetAdminByUsername 按用户名查询管理员。
func (s *Store) GetAdminByUsername(ctx context.Context, username string) (Admin, error) {
	return scanAdmin(s.db.QueryRowContext(ctx, "SELECT "+adminCols+" FROM admins WHERE username = ?", username))
}

// ListAdmins 返回全部管理员（按创建时间）。
func (s *Store) ListAdmins(ctx context.Context) ([]Admin, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+adminCols+" FROM admins ORDER BY created_at, rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Admin{}
	for rows.Next() {
		a, err := scanAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountAdmins 返回管理员总数。
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admins").Scan(&n)
	return n, err
}

// AdminUpdate 描述对管理员的可选修改；nil 表示不改。
type AdminUpdate struct {
	Role         *string
	Disabled     *bool
	PasswordHash *string
}

func activeOwners(ctx context.Context, tx dbtx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM admins WHERE role = 'owner' AND disabled = 0").Scan(&n)
	return n, err
}

// UpdateAdmin 修改管理员的角色 / 停用状态 / 密码。任何一项变化都会作废该管理员的全部会话（需重新登录）；
// 改完后若系统里没有可用的 owner，则整体回滚并返回 ErrLastOwner。审计记录 audit 在同一事务内写入。
func (s *Store) UpdateAdmin(ctx context.Context, id string, up AdminUpdate, audit AuditEntry) error {
	return s.withTx(ctx, func(tx dbtx) error {
		if _, err := scanAdmin(tx.QueryRowContext(ctx, "SELECT "+adminCols+" FROM admins WHERE id = ?", id)); err != nil {
			return err
		}
		before, err := activeOwners(ctx, tx)
		if err != nil {
			return err
		}
		if up.Role != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE admins SET role = ? WHERE id = ?", *up.Role, id); err != nil {
				return err
			}
		}
		if up.Disabled != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE admins SET disabled = ? WHERE id = ?", *up.Disabled, id); err != nil {
				return err
			}
		}
		if up.PasswordHash != nil {
			if _, err := tx.ExecContext(ctx, "UPDATE admins SET password_hash = ?, failed_count = 0, locked_until = 0 WHERE id = ?", *up.PasswordHash, id); err != nil {
				return err
			}
		}
		// 只有「改之前还有可用的 owner、改之后没有了」才拒绝（本来就没有 owner 的库里，其他管理员照常可以改密码）
		if n, err := activeOwners(ctx, tx); err != nil {
			return err
		} else if n == 0 && before > 0 {
			return ErrLastOwner
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM admin_sessions WHERE admin_id = ?", id); err != nil {
			return err
		}
		return appendAudit(ctx, tx, audit)
	})
}

// DeleteAdmin 删除管理员（会话级联删除）；会让系统失去最后一个可用 owner 时返回 ErrLastOwner。
func (s *Store) DeleteAdmin(ctx context.Context, id string, audit AuditEntry) error {
	return s.withTx(ctx, func(tx dbtx) error {
		before, err := activeOwners(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM admins WHERE id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if n, err := activeOwners(ctx, tx); err != nil {
			return err
		} else if n == 0 && before > 0 {
			return ErrLastOwner
		}
		return appendAudit(ctx, tx, audit)
	})
}

// AdminLoginFailed 累计一次连续登录失败；达到 maxFailures 时锁定 lock 这么久并把计数清零。返回是否刚被锁定。
func (s *Store) AdminLoginFailed(ctx context.Context, id string, maxFailures int, lock time.Duration, now int64) (bool, error) {
	lockedNow := false
	err := s.withTx(ctx, func(tx dbtx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT failed_count FROM admins WHERE id = ?", id).Scan(&n); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		n++
		if n >= maxFailures {
			lockedNow = true
			_, err := tx.ExecContext(ctx, "UPDATE admins SET failed_count = 0, locked_until = ? WHERE id = ?", now+lock.Milliseconds(), id)
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE admins SET failed_count = ? WHERE id = ?", n, id)
		return err
	})
	return lockedNow, err
}

// AdminLoginOK 记录一次成功登录：清零失败计数并写入会话（只存令牌哈希）。
func (s *Store) AdminLoginOK(ctx context.Context, id, sessionHash, ip string, now int64) error {
	return s.withTx(ctx, func(tx dbtx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE admins SET failed_count = 0, locked_until = 0, last_login_at = ? WHERE id = ?", now, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO admin_sessions (token_hash, admin_id, created_at, last_seen_at, ip) VALUES (?, ?, ?, ?, ?)",
			sessionHash, id, now, now, ip)
		return err
	})
}

// adminSessionTouch 是会话最近活动时间的最小写库间隔。
const adminSessionTouch = time.Minute

// AdminSession 由会话令牌哈希查询管理员。会话 idle 这么久无操作即过期（删除并返回 ErrNotFound）；
// 管理员已停用同样返回 ErrNotFound。最近活动时间每分钟最多写一次库。
func (s *Store) AdminSession(ctx context.Context, sessionHash string, now int64, idle time.Duration) (Admin, error) {
	var a Admin
	var last int64
	err := s.db.QueryRowContext(ctx, "SELECT "+prefixed("a", adminCols)+", s.last_seen_at FROM admin_sessions s JOIN admins a ON a.id = s.admin_id WHERE s.token_hash = ?", sessionHash).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Role, &a.Disabled, &a.CreatedAt, &a.LastLoginAt, &a.FailedCount, &a.LockedUntil, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if a.Disabled || now-last > idle.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM admin_sessions WHERE token_hash = ?", sessionHash); err != nil {
			return a, err
		}
		return a, ErrNotFound
	}
	if now-last >= adminSessionTouch.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, "UPDATE admin_sessions SET last_seen_at = ? WHERE token_hash = ?", now, sessionHash); err != nil {
			return a, err
		}
	}
	return a, nil
}

// DeleteAdminSession 作废一个会话（退出登录）。
func (s *Store) DeleteAdminSession(ctx context.Context, sessionHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM admin_sessions WHERE token_hash = ?", sessionHash)
	return err
}

// PruneAdminSessions 删除最近活动早于 before 的会话。
func (s *Store) PruneAdminSessions(ctx context.Context, before int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM admin_sessions WHERE last_seen_at < ?", before)
	return err
}

// ---------- 审计日志 ----------

// AuditEntry 是一条审计记录（管理后台 A5）。只能追加：表上的触发器禁止 UPDATE / DELETE。
type AuditEntry struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"`
	AdminID   string `json:"admin_id"`
	AdminName string `json:"admin_name"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	IP        string `json:"ip"`
}

func appendAudit(ctx context.Context, ex execer, e AuditEntry) error {
	if e.Action == "" {
		return nil
	}
	if e.TS == 0 {
		e.TS = NowMillis()
	}
	_, err := ex.ExecContext(ctx, "INSERT INTO admin_audit (ts, admin_id, admin_name, action, target, detail, ip) VALUES (?, ?, ?, ?, ?, ?, ?)",
		e.TS, e.AdminID, e.AdminName, e.Action, e.Target, e.Detail, e.IP)
	return err
}

// AppendAudit 追加一条审计记录。
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	return appendAudit(ctx, s.db, e)
}

// AppendAudit 在业务事务内追加审计记录（与业务写入一起提交或回滚）。
func (t *Tx) AppendAudit(e AuditEntry) error { return appendAudit(t.ctx, t.tx, e) }

// AuditFilter 是审计日志的查询条件；零值表示不过滤。
type AuditFilter struct {
	Admin  string // 管理员用户名（精确）
	Action string // 动作前缀，如 "user." 或 "user.disable"
	From   int64
	To     int64
}

// ListAudit 按时间倒序分页查询审计日志，返回当页与总数。
func (s *Store) ListAudit(ctx context.Context, f AuditFilter, page, size int) ([]AuditEntry, int, error) {
	where, args := "1=1", []any{}
	if f.Admin != "" {
		where += " AND admin_name = ?"
		args = append(args, f.Admin)
	}
	if f.Action != "" {
		where += " AND action LIKE ? ESCAPE '\\'"
		args = append(args, likeEscape(f.Action)+"%")
	}
	if f.From > 0 {
		where += " AND ts >= ?"
		args = append(args, f.From)
	}
	if f.To > 0 {
		where += " AND ts <= ?"
		args = append(args, f.To)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_audit WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, ts, admin_id, admin_name, action, target, detail, ip FROM admin_audit WHERE "+where+
		" ORDER BY id DESC LIMIT ? OFFSET ?", append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.AdminID, &e.AdminName, &e.Action, &e.Target, &e.Detail, &e.IP); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// likeEscape 转义 LIKE 模式里的通配符（配合 ESCAPE '\'）。
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ---------- 在线设置 ----------

// Settings 读取 settings 表的全部键值。
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SaveSettings 写入（覆盖）若干设置项、删除 reset 里的设置项（恢复为启动参数决定的默认值），并在同一事务内追加审计记录。
func (s *Store) SaveSettings(ctx context.Context, kv map[string]string, reset []string, by string, audit AuditEntry) error {
	now := NowMillis()
	return s.withTx(ctx, func(tx dbtx) error {
		for k, v := range kv {
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
				k, v, now, by); err != nil {
				return err
			}
		}
		for _, k := range reset {
			if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", k); err != nil {
				return err
			}
		}
		return appendAudit(ctx, tx, audit)
	})
}
