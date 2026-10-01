package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DayOf 返回时间 t 所在的本地日期编号 yyyymmdd（user_activity.day 的取值）。
func DayOf(t time.Time) int {
	y, m, d := t.Date()
	return y*10000 + int(m)*100 + d
}

// ActivityMark 表示某用户在某天有过认证请求。
type ActivityMark struct {
	UserID string
	Day    int   // DayOf
	At     int64 // 最近一次请求的毫秒时间
}

// GroupDelta 是一段时间内某个群新增的消息数。
type GroupDelta struct {
	Messages int64
	LastAt   int64
}

// StatsBatch 是一批待落库的统计增量（由 api 包在内存里累计，定时写入，避免每个请求都写库）。
type StatsBatch struct {
	Active []ActivityMark
	Hourly map[int64]int64 // Unix 小时 → 入队信封数
	Groups map[string]GroupDelta
}

// Empty 判断这批增量是否为空。
func (b StatsBatch) Empty() bool {
	return len(b.Active) == 0 && len(b.Hourly) == 0 && len(b.Groups) == 0
}

// RecordStats 在一个事务里写入一批统计增量。
func (s *Store) RecordStats(ctx context.Context, b StatsBatch) error {
	if b.Empty() {
		return nil
	}
	return s.withTx(ctx, func(tx dbtx) error {
		for _, a := range b.Active {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO user_activity (day, user_id) SELECT ?, id FROM users WHERE id = ?", a.Day, a.UserID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE users SET last_active_at = ? WHERE id = ? AND last_active_at < ?", a.At, a.UserID, a.At); err != nil {
				return err
			}
		}
		for hour, n := range b.Hourly {
			if _, err := tx.ExecContext(ctx, `INSERT INTO stats_hourly (hour, envelopes) VALUES (?, ?)
				ON CONFLICT(hour) DO UPDATE SET envelopes = envelopes + excluded.envelopes`, hour, n); err != nil {
				return err
			}
		}
		for gid, d := range b.Groups {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_stats (group_id, messages, last_message_at) SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM groups WHERE id = ?)
				ON CONFLICT(group_id) DO UPDATE SET messages = messages + excluded.messages, last_message_at = MAX(last_message_at, excluded.last_message_at)`,
				gid, d.Messages, d.LastAt, gid); err != nil {
				return err
			}
		}
		return nil
	})
}

// PruneStats 删除过旧的统计明细：day 之前的活跃记录、hour 之前的小时计数。
func (s *Store) PruneStats(ctx context.Context, beforeDay int, beforeHour int64) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM user_activity WHERE day < ?", beforeDay); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM stats_hourly WHERE hour < ?", beforeHour)
	return err
}

// Totals 是仪表盘上的全局计数（只有数量与大小）。
type Totals struct {
	Users           int64 `json:"users"`
	UsersDisabled   int64 `json:"users_disabled"`
	NewToday        int64 `json:"new_today"`
	ActiveToday     int64 `json:"active_today"`
	Groups          int64 `json:"groups"`
	Moments         int64 `json:"moments"`
	QueueItems      int64 `json:"queue_items"`
	QueueBytes      int64 `json:"queue_bytes"`
	Attachments     int64 `json:"attachments"`
	AttachmentBytes int64 `json:"attachment_bytes"`
	PendingReports  int64 `json:"pending_reports"`
}

// Totals 返回全局计数。dayStart 是今天零点（毫秒），day 是今天的日期编号。
func (s *Store) Totals(ctx context.Context, dayStart int64, day int) (Totals, error) {
	var t Totals
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM users WHERE disabled_at > 0),
		(SELECT COUNT(*) FROM users WHERE created_at >= ?),
		(SELECT COUNT(*) FROM user_activity WHERE day = ?),
		(SELECT COUNT(*) FROM groups),
		(SELECT COUNT(*) FROM moments),
		(SELECT COALESCE(SUM(items), 0) FROM queue_usage),
		(SELECT COALESCE(SUM(bytes), 0) FROM queue_usage),
		(SELECT COUNT(*) FROM attachments),
		(SELECT COALESCE(SUM(size), 0) FROM attachments),
		(SELECT COUNT(*) FROM reports WHERE status = 'pending')`, dayStart, day).
		Scan(&t.Users, &t.UsersDisabled, &t.NewToday, &t.ActiveToday, &t.Groups, &t.Moments, &t.QueueItems, &t.QueueBytes, &t.Attachments, &t.AttachmentBytes, &t.PendingReports)
	return t, err
}

// RegistrationsSince 返回 since（毫秒）以来注册的用户的注册时间（由调用方按本地日期分桶）。
func (s *Store) RegistrationsSince(ctx context.Context, since int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT created_at FROM users WHERE created_at >= ?", since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ActiveByDay 返回 fromDay 起每天的活跃用户数（日期编号 → 人数）。
func (s *Store) ActiveByDay(ctx context.Context, fromDay int) (map[int]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT day, COUNT(*) FROM user_activity WHERE day >= ? GROUP BY day", fromDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var d int
		var n int64
		if err := rows.Scan(&d, &n); err != nil {
			return nil, err
		}
		out[d] = n
	}
	return out, rows.Err()
}

// HourlyEnvelopes 返回 fromHour 起每小时入队的信封数（Unix 小时 → 条数）。
func (s *Store) HourlyEnvelopes(ctx context.Context, fromHour int64) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT hour, envelopes FROM stats_hourly WHERE hour >= ?", fromHour)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var h, n int64
		if err := rows.Scan(&h, &n); err != nil {
			return nil, err
		}
		out[h] = n
	}
	return out, rows.Err()
}

// Uploader 是附件占用排行里的一行。
type Uploader struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// TopUploaders 返回附件占用最多的上传者（账号已删除的上传者 Name / Email 为空）。
func (s *Store) TopUploaders(ctx context.Context, limit int) ([]Uploader, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.uploader_id, COALESCE(u.name, ''), COALESCE(u.email, ''), COUNT(*), SUM(a.size) AS total
		FROM attachments a LEFT JOIN users u ON u.id = a.uploader_id GROUP BY a.uploader_id ORDER BY total DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Uploader{}
	for rows.Next() {
		var u Uploader
		if err := rows.Scan(&u.UserID, &u.Name, &u.Email, &u.Files, &u.Bytes); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DBStats 返回数据库统计：连接数取自只读池；WaitCount / WaitDuration 是「写队列等待」——
// 交给写协程的写操作总数与它们排队等待的累计时间。只读池自己的等待见 ReadPoolStats。
func (s *Store) DBStats() sql.DBStats {
	st := s.rd.DB.Stats()
	w := s.WriterStats()
	st.WaitCount = w.Ops
	st.WaitDuration = w.Wait
	return st
}

// ReadPoolStats 返回只读连接池的统计（WaitCount / WaitDuration 是等不到空闲读连接的次数与时间）。
func (s *Store) ReadPoolStats() sql.DBStats { return s.rd.DB.Stats() }

// BackupInto 把数据库在线备份到 path（文件不能已存在）：先 VACUUM INTO 得到一致的副本，再从**副本**里删掉离线队列中的全部信封
// （端到端加密的消息密文）并再次 VACUUM，使被删的内容不残留在空闲页里。备份因此不含任何消息内容——管理后台可以下载备份，
// 而后台不提供任何拿到信封的途径；代价是用备份恢复后，当时尚未投递的消息不会再投递。事件、账号、关系等其余数据完整保留。
// 出错时 path 可能残留不完整的文件，由调用方删除。VACUUM INTO 期间其他读写排队等待。
func (s *Store) BackupInto(ctx context.Context, path string) error {
	// VACUUM INTO 在一条独立的连接上执行：WAL 下它只持有读快照，备份期间其他读写照常进行，不占用写连接。
	src, err := sql.Open("sqlite", "file:"+s.path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	src.SetMaxOpenConns(1)
	_, err = src.ExecContext(ctx, "VACUUM INTO ?", path)
	src.Close()
	if err != nil {
		return err
	}
	// 副本用独立连接处理，不占用主库的连接；回滚日志模式，结束后不留 -wal / -shm 文件
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(DELETE)&_pragma=secure_delete(1)&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// 信封正文有两处：队列行内联的，以及 envelopes 表里的共享正文（群消息）。共享正文里属于事件的要留下。
	for _, q := range []string{
		"DELETE FROM queue WHERE kind = 'envelope'",
		"DELETE FROM envelopes WHERE NOT EXISTS (SELECT 1 FROM queue q WHERE q.id = envelopes.id AND q.shared = 1)",
		"DELETE FROM send_dedup",
		"VACUUM",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("清除备份里的信封: %w", err)
		}
	}
	return db.Close()
}
