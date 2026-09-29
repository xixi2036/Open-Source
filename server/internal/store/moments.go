package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// 朋友圈可见范围模式。
const (
	VisAll     = "all"
	VisInclude = "include"
	VisExclude = "exclude"
)

// Media 是动态中的一张图（契约 3.8）。
type Media struct {
	AttachmentID string `json:"attachment_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

// Visibility 是动态的可见范围。
type Visibility struct {
	Mode    string   `json:"mode"`
	UserIDs []string `json:"user_ids"`
}

// Comment 是评论（契约 3.8）。
type Comment struct {
	ID        string   `json:"id"`
	Author    Profile  `json:"author"`
	ReplyTo   *Profile `json:"reply_to"`
	Text      string   `json:"text"`
	CreatedAt int64    `json:"created_at"`
}

// Moment 是按查看者过滤后的动态（契约 3.8）。
type Moment struct {
	ID         string      `json:"id"`
	Author     Profile     `json:"author"`
	Text       string      `json:"text"`
	Media      []Media     `json:"media"`
	Visibility *Visibility `json:"visibility,omitempty"`
	Likes      []Profile   `json:"likes"`
	Comments   []Comment   `json:"comments"`
	CreatedAt  int64       `json:"created_at"`
}

// MomentRow 是动态原始记录。
type MomentRow struct {
	ID        string
	AuthorID  string
	Text      string
	Media     []Media
	VisMode   string
	CreatedAt int64
}

// MomentNotification 是朋友圈互动通知（契约 3.8）。
type MomentNotification struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	MomentID  string  `json:"moment_id"`
	From      Profile `json:"from"`
	Text      *string `json:"text,omitempty"`
	CreatedAt int64   `json:"created_at"`
}

// visibleClause 是「动态 m 对查看者可见」的 SQL 条件，需要 4 个查看者 id 参数。
// 规则：作者本人；或作者的好友且满足 all / include 名单内 / exclude 名单外。
const visibleClause = `(m.author_id = ? OR (
	EXISTS (SELECT 1 FROM friendships f WHERE f.user_id = m.author_id AND f.friend_id = ?)
	AND (m.vis_mode = 'all'
		OR (m.vis_mode = 'include' AND EXISTS (SELECT 1 FROM moment_visibility mv WHERE mv.moment_id = m.id AND mv.user_id = ?))
		OR (m.vis_mode = 'exclude' AND NOT EXISTS (SELECT 1 FROM moment_visibility mv WHERE mv.moment_id = m.id AND mv.user_id = ?)))))`

func visibleArgs(viewer string) []any { return []any{viewer, viewer, viewer, viewer} }

// friendOrSelf 是「user 列是查看者本人或其好友」的条件，需要 2 个查看者 id 参数。
func friendOrSelf(col string) string {
	return "(" + col + " = ? OR " + col + " IN (SELECT friend_id FROM friendships WHERE user_id = ?))"
}

const momentCols = "m.id, m.author_id, m.text, m.media, m.vis_mode, m.created_at"

func scanMoment(sc interface{ Scan(...any) error }) (MomentRow, error) {
	var r MomentRow
	var media string
	err := sc.Scan(&r.ID, &r.AuthorID, &r.Text, &media, &r.VisMode, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(media), &r.Media); err != nil {
		return r, err
	}
	if r.Media == nil {
		r.Media = []Media{}
	}
	return r, nil
}

// CreateMoment 发布动态。
func (s *Store) CreateMoment(ctx context.Context, authorID, text string, media []Media, vis Visibility) (MomentRow, error) {
	if media == nil {
		media = []Media{}
	}
	raw, err := json.Marshal(media)
	if err != nil {
		return MomentRow{}, err
	}
	r := MomentRow{ID: NewID(), AuthorID: authorID, Text: text, Media: media, VisMode: vis.Mode, CreatedAt: NowMillis()}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO moments (id, author_id, text, media, vis_mode, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			r.ID, authorID, text, string(raw), vis.Mode, r.CreatedAt); err != nil {
			return err
		}
		// 关联表：附件清理据此判断图片仍被引用（同一张图出现两次只记一行）
		for _, m := range media {
			if _, err := tx.ExecContext(ctx, "INSERT INTO moment_media (moment_id, attachment_id) VALUES (?, ?) ON CONFLICT DO NOTHING", r.ID, m.AttachmentID); err != nil {
				return err
			}
		}
		if vis.Mode == VisAll {
			return nil
		}
		for _, uid := range vis.UserIDs {
			if _, err := tx.ExecContext(ctx, "INSERT INTO moment_visibility (moment_id, user_id) VALUES (?, ?) ON CONFLICT DO NOTHING", r.ID, uid); err != nil {
				return err
			}
		}
		return nil
	})
	return r, err
}

// VisibleMoment 返回对查看者可见的动态；不可见或不存在均返回 ErrNotFound。
func (s *Store) VisibleMoment(ctx context.Context, viewer, id string) (MomentRow, error) {
	return visibleMoment(ctx, s.db, viewer, id)
}

// VisibleMoment 在事务内查询对查看者可见的动态。
func (t *Tx) VisibleMoment(viewer, id string) (MomentRow, error) {
	return visibleMoment(t.ctx, t.tx, viewer, id)
}

func visibleMoment(ctx context.Context, ex execer, viewer, id string) (MomentRow, error) {
	args := append([]any{id}, visibleArgs(viewer)...)
	return scanMoment(ex.QueryRowContext(ctx, "SELECT "+momentCols+" FROM moments m WHERE m.id = ? AND "+visibleClause, args...))
}

// GetMomentRow 不做可见性检查地读取动态。
func (s *Store) GetMomentRow(ctx context.Context, id string) (MomentRow, error) {
	return scanMoment(s.db.QueryRowContext(ctx, "SELECT "+momentCols+" FROM moments m WHERE m.id = ?", id))
}

// Cursor 是分页游标：(created_at, id) 复合游标，与排序 created_at DESC, rowid DESC 一致。
// BeforeID 为空或查不到时退化为 created_at < Before（旧语义）；否则同一毫秒内排在 BeforeID 之后的条目也会返回，不会漏条。
type Cursor struct {
	Before   int64
	BeforeID string
}

// cursorClause 生成游标条件，需要 3 个参数（见 args）。table 是用于查 rowid 的表名。
func cursorClause(alias, table string) string {
	return "(" + alias + ".created_at < ? OR (" + alias + ".created_at = ? AND " + alias + ".rowid < COALESCE((SELECT rowid FROM " + table + " WHERE id = ?), -1)))"
}

func (c Cursor) args() []any { return []any{c.Before, c.Before, c.BeforeID} }

// Feed 返回查看者可见的动态（本人 + 好友），按时间倒序，位于游标之前。
// 先按作者（本人 ∪ 好友）限定范围，走 moments_author_time 索引，而不是扫描全服动态再逐行判断可见性。
func (s *Store) Feed(ctx context.Context, viewer string, cur Cursor, limit int) ([]MomentRow, error) {
	args := append([]any{viewer, viewer}, cur.args()...)
	args = append(args, visibleArgs(viewer)...)
	args = append(args, limit)
	return s.queryMoments(ctx, "SELECT "+momentCols+" FROM moments m WHERE m.author_id IN (SELECT friend_id FROM friendships WHERE user_id = ? UNION SELECT ?) AND "+
		cursorClause("m", "moments")+" AND "+visibleClause+
		" ORDER BY m.created_at DESC, m.rowid DESC LIMIT ?", args...)
}

// UserMoments 返回某作者对查看者可见的动态。
func (s *Store) UserMoments(ctx context.Context, viewer, author string, cur Cursor, limit int) ([]MomentRow, error) {
	args := append([]any{author}, cur.args()...)
	args = append(args, visibleArgs(viewer)...)
	args = append(args, limit)
	return s.queryMoments(ctx, "SELECT "+momentCols+" FROM moments m WHERE m.author_id = ? AND "+cursorClause("m", "moments")+" AND "+visibleClause+
		" ORDER BY m.created_at DESC, m.rowid DESC LIMIT ?", args...)
}

func (s *Store) queryMoments(ctx context.Context, q string, args ...any) ([]MomentRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MomentRow
	for rows.Next() {
		r, err := scanMoment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteMoment 删除动态（级联删除点赞、评论、通知）。
func (s *Store) DeleteMoment(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM moments WHERE id = ?", id)
	return err
}

// HydrateMoments 为查看者组装完整 Moment：点赞与评论只保留查看者本人或其好友产生的条目；
// visibility 字段只在查看者为作者时返回。
func (s *Store) HydrateMoments(ctx context.Context, viewer string, rows []MomentRow) ([]Moment, error) {
	out := make([]Moment, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, len(rows))
	userIDs := map[string]bool{}
	for i, r := range rows {
		ids[i] = r.ID
		userIDs[r.AuthorID] = true
	}
	idArgs := toArgs(ids)

	// 点赞
	likes := map[string][]string{}
	lrows, err := s.db.QueryContext(ctx, "SELECT moment_id, user_id FROM moment_likes WHERE moment_id IN ("+placeholders(len(ids))+") AND "+
		friendOrSelf("user_id")+" ORDER BY created_at, rowid", append(idArgs, viewer, viewer)...)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var mid, uid string
		if err := lrows.Scan(&mid, &uid); err != nil {
			lrows.Close()
			return nil, err
		}
		likes[mid] = append(likes[mid], uid)
		userIDs[uid] = true
	}
	lrows.Close()

	// 评论
	type rawComment struct {
		id, author, text string
		replyTo          sql.NullString
		createdAt        int64
	}
	comments := map[string][]rawComment{}
	crows, err := s.db.QueryContext(ctx, "SELECT id, moment_id, author_id, reply_to_id, text, created_at FROM moment_comments WHERE moment_id IN ("+
		placeholders(len(ids))+") AND "+friendOrSelf("author_id")+" ORDER BY created_at, rowid", append(idArgs, viewer, viewer)...)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var c rawComment
		var mid string
		if err := crows.Scan(&c.id, &mid, &c.author, &c.replyTo, &c.text, &c.createdAt); err != nil {
			crows.Close()
			return nil, err
		}
		comments[mid] = append(comments[mid], c)
		userIDs[c.author] = true
		if c.replyTo.Valid {
			userIDs[c.replyTo.String] = true
		}
	}
	crows.Close()

	// 可见范围（仅作者本人）
	visUsers := map[string][]string{}
	vrows, err := s.db.QueryContext(ctx, "SELECT mv.moment_id, mv.user_id FROM moment_visibility mv JOIN moments m ON m.id = mv.moment_id WHERE m.author_id = ? AND mv.moment_id IN ("+
		placeholders(len(ids))+")", append([]any{viewer}, idArgs...)...)
	if err != nil {
		return nil, err
	}
	for vrows.Next() {
		var mid, uid string
		if err := vrows.Scan(&mid, &uid); err != nil {
			vrows.Close()
			return nil, err
		}
		visUsers[mid] = append(visUsers[mid], uid)
	}
	vrows.Close()

	uids := make([]string, 0, len(userIDs))
	for id := range userIDs {
		uids = append(uids, id)
	}
	profiles, err := s.Profiles(ctx, uids)
	if err != nil {
		return nil, err
	}

	for _, r := range rows {
		m := Moment{ID: r.ID, Author: profiles[r.AuthorID], Text: r.Text, Media: r.Media, Likes: []Profile{}, Comments: []Comment{}, CreatedAt: r.CreatedAt}
		if r.AuthorID == viewer {
			v := &Visibility{Mode: r.VisMode, UserIDs: visUsers[r.ID]}
			if v.UserIDs == nil {
				v.UserIDs = []string{}
			}
			m.Visibility = v
		}
		for _, uid := range likes[r.ID] {
			m.Likes = append(m.Likes, profiles[uid])
		}
		for _, c := range comments[r.ID] {
			cm := Comment{ID: c.id, Author: profiles[c.author], Text: c.text, CreatedAt: c.createdAt}
			if c.replyTo.Valid {
				p := profiles[c.replyTo.String]
				cm.ReplyTo = &p
			}
			m.Comments = append(m.Comments, cm)
		}
		out = append(out, m)
	}
	return out, nil
}

// Like 点赞；返回是否为新增。
func (s *Store) Like(ctx context.Context, momentID, userID string) (bool, error) {
	return update(s, ctx, func(t *Tx) (bool, error) { return t.Like(momentID, userID) })
}

// Like 在事务内点赞；返回是否为新增。
func (t *Tx) Like(momentID, userID string) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, "INSERT INTO moment_likes (moment_id, user_id, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING",
		momentID, userID, NowMillis())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Unlike 取消点赞，并删除对应的点赞通知。
func (s *Store) Unlike(ctx context.Context, momentID, userID string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM moment_likes WHERE moment_id = ? AND user_id = ?", momentID, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM moment_notifications WHERE moment_id = ? AND from_id = ? AND kind = 'like'", momentID, userID)
		return err
	})
}

// CommentRow 是评论原始记录。
type CommentRow struct {
	ID        string
	MomentID  string
	AuthorID  string
	ReplyToID *string
	Text      string
	CreatedAt int64
}

// CreateComment 写入评论。
func (s *Store) CreateComment(ctx context.Context, momentID, authorID string, replyTo *string, text string) (CommentRow, error) {
	return update(s, ctx, func(t *Tx) (CommentRow, error) { return t.CreateComment(momentID, authorID, replyTo, text) })
}

// CreateComment 在事务内写入评论。
func (t *Tx) CreateComment(momentID, authorID string, replyTo *string, text string) (CommentRow, error) {
	c := CommentRow{ID: NewID(), MomentID: momentID, AuthorID: authorID, ReplyToID: replyTo, Text: text, CreatedAt: NowMillis()}
	_, err := t.tx.ExecContext(t.ctx, "INSERT INTO moment_comments (id, moment_id, author_id, reply_to_id, text, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		c.ID, momentID, authorID, replyTo, text, c.CreatedAt)
	return c, err
}

// GetComment 查询评论。
func (s *Store) GetComment(ctx context.Context, id string) (CommentRow, error) {
	var c CommentRow
	var reply sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT id, moment_id, author_id, reply_to_id, text, created_at FROM moment_comments WHERE id = ?", id).
		Scan(&c.ID, &c.MomentID, &c.AuthorID, &reply, &c.Text, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if reply.Valid {
		c.ReplyToID = &reply.String
	}
	return c, err
}

// DeleteComment 删除评论及其通知。
func (s *Store) DeleteComment(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM moment_notifications WHERE comment_id = ?", id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM moment_comments WHERE id = ?", id)
		return err
	})
}

// NotificationInsert 是一条待写入的通知。
type NotificationInsert struct {
	UserID    string
	Kind      string
	MomentID  string
	FromID    string
	CommentID *string
	Text      *string
}

// CreateNotification 写入通知并返回带资料的通知对象。
func (s *Store) CreateNotification(ctx context.Context, n NotificationInsert) (MomentNotification, error) {
	return update(s, ctx, func(t *Tx) (MomentNotification, error) { return t.CreateNotification(n) })
}

// CreateNotification 在事务内写入通知并返回带资料的通知对象。
func (t *Tx) CreateNotification(n NotificationInsert) (MomentNotification, error) {
	ctx := t.ctx
	id := NewID()
	now := NowMillis()
	if _, err := t.tx.ExecContext(ctx, `INSERT INTO moment_notifications (id, user_id, kind, moment_id, from_id, comment_id, text, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, n.UserID, n.Kind, n.MomentID, n.FromID, n.CommentID, n.Text, now); err != nil {
		return MomentNotification{}, err
	}
	from, err := getUser(ctx, t.tx, n.FromID)
	if err != nil {
		return MomentNotification{}, err
	}
	return MomentNotification{ID: id, Kind: n.Kind, MomentID: n.MomentID, From: from.Profile, Text: n.Text, CreatedAt: now}, nil
}

// Notifications 返回用户的互动通知，按时间倒序，位于游标之前。
func (s *Store) Notifications(ctx context.Context, userID string, cur Cursor, limit int) ([]MomentNotification, error) {
	args := append([]any{userID}, cur.args()...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.kind, n.moment_id, n.from_id, n.text, n.created_at FROM moment_notifications n
		WHERE n.user_id = ? AND `+cursorClause("n", "moment_notifications")+` ORDER BY n.created_at DESC, n.rowid DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	type raw struct {
		n    MomentNotification
		from string
	}
	var list []raw
	for rows.Next() {
		var r raw
		var text sql.NullString
		if err := rows.Scan(&r.n.ID, &r.n.Kind, &r.n.MomentID, &r.from, &text, &r.n.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if text.Valid {
			r.n.Text = &text.String
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, len(list))
	for i, r := range list {
		ids[i] = r.from
	}
	profiles, err := s.Profiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]MomentNotification, 0, len(list))
	for _, r := range list {
		r.n.From = profiles[r.from]
		out = append(out, r.n)
	}
	return out, nil
}
