package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Attachment 是附件元数据；内容存放在 data/attachments/<id>。
type Attachment struct {
	ID         string
	UploaderID string
	Size       int64
	CreatedAt  int64
}

// CreateAttachment 记录附件元数据（最近访问时间 = 创建时间）。
func (s *Store) CreateAttachment(ctx context.Context, a Attachment) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO attachments (id, uploader_id, size, created_at, last_access) VALUES (?, ?, ?, ?, ?)",
		a.ID, a.UploaderID, a.Size, a.CreatedAt, a.CreatedAt)
	return err
}

// touchInterval 是附件访问时间的最小写库间隔。
const touchInterval = 24 * time.Hour

// TouchAttachment 更新最近访问时间（距上次记录超过 touchInterval 才写，避免每次下载都写库）。
func (s *Store) TouchAttachment(ctx context.Context, id string, now int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE attachments SET last_access = ? WHERE id = ? AND last_access < ?", now, id, now-touchInterval.Milliseconds())
	return err
}

// UploadedBytesSince 返回某用户自 since（毫秒）以来上传的附件总字节数。
func (s *Store) UploadedBytesSince(ctx context.Context, uploaderID string, since int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0) FROM attachments WHERE uploader_id = ? AND created_at >= ?", uploaderID, since).Scan(&n)
	return n, err
}

// staleCond 是「最近访问早于 ?、且没有被头像（用户/群）或朋友圈图片引用」的条件（直接引用表名 attachments，1 个参数）。
// 三个引用判断都走索引（users_avatar、groups_avatar、moment_media_attachment），不扫描全部动态。
const staleCond = `attachments.last_access < ?
		AND NOT EXISTS (SELECT 1 FROM users u WHERE u.avatar_id = attachments.id)
		AND NOT EXISTS (SELECT 1 FROM groups g WHERE g.avatar_id = attachments.id)
		AND NOT EXISTS (SELECT 1 FROM moment_media mm WHERE mm.attachment_id = attachments.id)`

// StaleAttachments 返回满足清理条件的附件 id（最多 limit 个）。
func (s *Store) StaleAttachments(ctx context.Context, before int64, limit int) ([]string, error) {
	return s.queryIDs(ctx, "SELECT id FROM attachments WHERE "+staleCond+" LIMIT ?", before, limit)
}

// DeleteStaleAttachments 删除 ids 中**此刻仍满足清理条件**的附件元数据，返回实际删除的 id。
// 条件与删除在同一条语句里：挑出候选之后才被设为头像或被动态引用的附件不会被删。
func (s *Store) DeleteStaleAttachments(ctx context.Context, ids []string, before int64) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	deleted, err := s.queryIDs(ctx, "DELETE FROM attachments WHERE id IN ("+placeholders(len(ids))+") AND "+staleCond+" RETURNING id",
		append(toArgs(ids), before)...)
	for _, id := range deleted {
		out[id] = true
	}
	return out, err
}

// GetAttachment 查询附件元数据。
func (s *Store) GetAttachment(ctx context.Context, id string) (Attachment, error) {
	a := Attachment{ID: id}
	err := s.db.QueryRowContext(ctx, "SELECT uploader_id, size, created_at FROM attachments WHERE id = ?", id).
		Scan(&a.UploaderID, &a.Size, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}
