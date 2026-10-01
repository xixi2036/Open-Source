package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 从 v5 的库升级到当前版本：已有数据完好，新列取默认值。
func TestMigrateFromV5KeepsData(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "hush.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	db.Exec("PRAGMA user_version = 5")
	db.Exec("INSERT INTO users (id, email, name, created_at, updated_at) VALUES ('u1', 'a@example.com', 'A', 1000, 1000)")
	db.Exec("INSERT INTO tokens (token_hash, user_id, created_at, last_used_at) VALUES ('h', 'u1', 1, 1)")
	db.Exec("INSERT INTO queue (user_id, id, kind, payload, created_at, size) VALUES ('u1', 'q1', 'envelope', '{}', 1, 2)")
	db.Close()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("升级失败: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != len(migrations) || version < 6 {
		t.Fatalf("user_version = %d", version)
	}
	u, err := s.AdminGetUser(ctx, "u1")
	if err != nil || u.Email != "a@example.com" || u.DisabledAt != 0 || u.LastActiveAt != 0 || u.QueueItems != 1 || u.QueueBytes != 2 || u.Sessions != 1 {
		t.Fatalf("升级后数据不对: %+v %v", u, err)
	}
	if n, _ := s.CountAdmins(ctx); n != 0 {
		t.Fatal("升级不应创建任何默认管理员")
	}
	kv, _ := s.Settings(ctx)
	if len(kv) != 0 {
		t.Fatalf("升级不应写入任何设置: %v", kv)
	}
	for _, table := range []string{"admins", "admin_sessions", "admin_audit", "settings", "reports", "invite_codes", "invite_uses", "announcements", "user_activity", "stats_hourly", "group_stats"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("表 %s: %d %v", table, n, err)
		}
	}
	// 再次打开不重复迁移
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

// 审计日志只能追加：UPDATE / DELETE 被触发器拒绝。
func TestAuditIsAppendOnly(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.AppendAudit(ctx, AuditEntry{AdminID: "a", AdminName: "root", Action: fmt.Sprintf("user.op%d", i), Target: "user:1", IP: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"DELETE FROM admin_audit", "DELETE FROM admin_audit WHERE id = 1", "UPDATE admin_audit SET action = 'x'", "UPDATE admin_audit SET admin_name = 'someone' WHERE id = 2"} {
		if _, err := s.db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%q 应被拒绝，得到 %v", q, err)
		}
	}
	list, total, err := s.ListAudit(ctx, AuditFilter{Action: "user."}, 1, 2)
	if err != nil || total != 3 || len(list) != 2 || list[0].Action != "user.op2" || list[0].TS == 0 {
		t.Fatalf("审计查询不对: %+v %d %v", list, total, err)
	}
	if _, total, _ := s.ListAudit(ctx, AuditFilter{Action: "user_"}, 1, 10); total != 0 {
		t.Fatal("动作前缀里的 _ 不应被当作通配符")
	}
}

func TestAdminSessionsAndLastOwner(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	a, err := s.CreateAdmin(ctx, "root", "hash", AdminOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAdmin(ctx, "root", "hash", AdminViewer); err != ErrConflict {
		t.Fatalf("重名应为 ErrConflict: %v", err)
	}
	now := NowMillis()
	if err := s.AdminLoginOK(ctx, a.ID, "sess", "127.0.0.1", now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AdminSession(ctx, "sess", now+time.Hour.Milliseconds(), 12*time.Hour); err != nil || got.ID != a.ID {
		t.Fatalf("会话应有效: %v", err)
	}
	// 刚才的访问刷新了最近活动时间：从那时起再过 12 小时才过期
	if _, err := s.AdminSession(ctx, "sess", now+12*time.Hour.Milliseconds()+time.Minute.Milliseconds(), 12*time.Hour); err != nil {
		t.Fatalf("有操作应续期: %v", err)
	}
	if _, err := s.AdminSession(ctx, "sess", now+30*time.Hour.Milliseconds(), 12*time.Hour); err != ErrNotFound {
		t.Fatalf("闲置超时应过期: %v", err)
	}
	if _, err := s.AdminSession(ctx, "sess", now, 12*time.Hour); err != ErrNotFound {
		t.Fatal("过期的会话应已删除")
	}
	// 最后一个 owner 不能停用 / 降级 / 删除，失败时整体回滚（会话也不被清掉）
	s.AdminLoginOK(ctx, a.ID, "sess2", "", now)
	yes, viewer := true, AdminViewer
	if err := s.UpdateAdmin(ctx, a.ID, AdminUpdate{Disabled: &yes}, AuditEntry{Action: "x"}); err != ErrLastOwner {
		t.Fatalf("应为 ErrLastOwner: %v", err)
	}
	if err := s.UpdateAdmin(ctx, a.ID, AdminUpdate{Role: &viewer}, AuditEntry{}); err != ErrLastOwner {
		t.Fatalf("应为 ErrLastOwner: %v", err)
	}
	if err := s.DeleteAdmin(ctx, a.ID, AuditEntry{}); err != ErrLastOwner {
		t.Fatalf("应为 ErrLastOwner: %v", err)
	}
	if got, err := s.GetAdmin(ctx, a.ID); err != nil || got.Disabled || got.Role != AdminOwner {
		t.Fatalf("失败的修改应回滚: %+v %v", got, err)
	}
	if _, err := s.AdminSession(ctx, "sess2", now, time.Hour); err != nil {
		t.Fatalf("失败的修改不应清会话: %v", err)
	}
	if _, total, _ := s.ListAudit(ctx, AuditFilter{}, 1, 10); total != 0 {
		t.Fatal("回滚的修改不应留下审计")
	}
	// 库里本来就没有可用 owner 时（例如第一个管理员建成了 viewer），其他管理员仍然可以改密码
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v, _ := s2.CreateAdmin(ctx, "onlyviewer", "hash", AdminViewer)
	newHash := "hash2"
	if err := s2.UpdateAdmin(ctx, v.ID, AdminUpdate{PasswordHash: &newHash}, AuditEntry{}); err != nil {
		t.Fatalf("没有 owner 的库里改密码不应失败: %v", err)
	}
	// 连续失败计数与锁定
	for i := 0; i < 2; i++ {
		if locked, _ := s.AdminLoginFailed(ctx, a.ID, 3, time.Minute, now); locked {
			t.Fatal("未达上限不应锁定")
		}
	}
	if locked, _ := s.AdminLoginFailed(ctx, a.ID, 3, time.Minute, now); !locked {
		t.Fatal("第 3 次失败应锁定")
	}
	if got, _ := s.GetAdmin(ctx, a.ID); got.LockedUntil != now+time.Minute.Milliseconds() {
		t.Fatalf("锁定时间不对: %+v", got)
	}
}

func TestRecordStats(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	res, _ := s.Login(ctx, "a@example.com", "ios", "d", "h1")
	g, _ := s.CreateGroup(ctx, res.User.ID, "g", nil)
	now := time.Now()
	day := DayOf(now)
	batch := StatsBatch{
		Active: []ActivityMark{{UserID: res.User.ID, Day: day, At: now.UnixMilli()}, {UserID: "ghost", Day: day, At: 1}},
		Hourly: map[int64]int64{100: 5},
		Groups: map[string]GroupDelta{g.ID: {Messages: 2, LastAt: 50}, "gone": {Messages: 9, LastAt: 1}},
	}
	for i := 0; i < 2; i++ { // 幂等的活跃标记、可累加的计数
		if err := s.RecordStats(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	tt, err := s.Totals(ctx, 0, day)
	if err != nil || tt.ActiveToday != 1 || tt.Users != 1 || tt.Groups != 1 {
		t.Fatalf("Totals 不对（不存在的用户不计入活跃）: %+v %v", tt, err)
	}
	if h, _ := s.HourlyEnvelopes(ctx, 0); h[100] != 10 {
		t.Fatalf("小时计数应累加: %v", h)
	}
	ag, _, err := s.AdminGetGroup(ctx, g.ID)
	if err != nil || ag.Messages != 4 || ag.LastMessageAt != 50 {
		t.Fatalf("群消息计数不对: %+v %v", ag, err)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM group_stats").Scan(&n)
	if n != 1 {
		t.Fatal("不存在的群不应留下计数")
	}
	if err := s.PruneStats(ctx, day+1, 101); err != nil {
		t.Fatal(err)
	}
	if tt, _ := s.Totals(ctx, 0, day); tt.ActiveToday != 0 {
		t.Fatal("PruneStats 应删除过旧的活跃记录")
	}
}
