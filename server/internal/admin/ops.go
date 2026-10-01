package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"hush/server/internal/api"
	"hush/server/internal/metrics"
	"hush/server/internal/store"
)

// ---------- A6 概览 ----------

// GET /admin/api/dashboard → 全局计数 + 最近 30 天注册 / 活跃趋势 + 最近 24 小时每小时入队信封数
func (c *ctx) dashboard() {
	rctx := c.r.Context()
	if err := c.s.api.FlushStats(rctx); err != nil {
		log.Printf("[admin] 统计落库失败: %v", err)
	}
	now := c.now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	totals, err := c.s.st.Totals(rctx, today.UnixMilli(), store.DayOf(now))
	if err != nil {
		c.internal(err)
		return
	}
	const days = 30
	first := today.AddDate(0, 0, -(days - 1))
	regs, err := c.s.st.RegistrationsSince(rctx, first.UnixMilli())
	if err != nil {
		c.internal(err)
		return
	}
	active, err := c.s.st.ActiveByDay(rctx, store.DayOf(first))
	if err != nil {
		c.internal(err)
		return
	}
	regByDay := map[int]int64{}
	for _, ms := range regs {
		regByDay[store.DayOf(time.UnixMilli(ms).In(now.Location()))]++
	}
	type dayPoint struct {
		Day           string `json:"day"`
		Registrations int64  `json:"registrations"`
		Active        int64  `json:"active"`
	}
	trend := make([]dayPoint, days)
	for i := range trend {
		d := first.AddDate(0, 0, i)
		trend[i] = dayPoint{Day: d.Format("2006-01-02"), Registrations: regByDay[store.DayOf(d)], Active: active[store.DayOf(d)]}
	}
	curHour := now.Unix() / 3600
	hourly, err := c.s.st.HourlyEnvelopes(rctx, curHour-23)
	if err != nil {
		c.internal(err)
		return
	}
	type hourPoint struct {
		TS        int64 `json:"ts"` // 该小时起点（毫秒）
		Envelopes int64 `json:"envelopes"`
	}
	hours := make([]hourPoint, 24)
	for i := range hours {
		h := curHour - 23 + int64(i)
		hours[i] = hourPoint{TS: h * 3600 * 1000, Envelopes: hourly[h]}
	}
	c.ok(obj{
		"totals":         totals,
		"online":         c.s.hub.OnlineCount(),
		"uptime_seconds": int64(time.Since(c.s.api.Metrics().Start()).Seconds()),
		"version":        c.s.cfg.Version,
		"go_version":     runtime.Version(),
		"trend":          trend,
		"hourly":         hours,
		"registration":   c.s.api.Settings().RegistrationMode,
	})
}

// ---------- A11 邀请码 ----------

const inviteAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去掉易混淆的 0/O/1/I

func newInviteCode() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = inviteAlphabet[int(b[i])%len(inviteAlphabet)] // 256 是 32 的整数倍，无取模偏差
	}
	return string(b[:5]) + "-" + string(b[5:]), nil
}

// GET /admin/api/invites?page=&size=
func (c *ctx) listInvites() {
	page, size := c.page()
	list, total, err := c.s.st.ListInvites(c.r.Context(), page, size)
	if err != nil {
		c.internal(err)
		return
	}
	if roleRank[c.admin.Role] < roleRank[store.AdminModerator] {
		for i := range list { // 邀请码等同于注册凭证：只读角色看不到完整的码
			list[i].Code = maskCode(list[i].Code)
		}
	}
	res := paged(list, total, page, size)
	res["registration_mode"] = c.s.api.Settings().RegistrationMode
	c.ok(res)
}

// POST /admin/api/invites {count?, max_uses?, expires_at?, note?} → [Invite]
func (c *ctx) createInvites() {
	var req struct {
		Count     int    `json:"count"`
		MaxUses   int    `json:"max_uses"`
		ExpiresAt int64  `json:"expires_at"`
		Note      string `json:"note"`
	}
	if !c.decode(&req) {
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}
	if req.MaxUses == 0 {
		req.MaxUses = 1
	}
	req.Note = strings.TrimSpace(req.Note)
	switch {
	case req.Count < 1 || req.Count > 100:
		c.bad("一次生成 1–100 个")
		return
	case req.MaxUses < 1 || req.MaxUses > 100000:
		c.bad("次数上限须为 1–100000")
		return
	case req.ExpiresAt != 0 && req.ExpiresAt <= c.now().UnixMilli():
		c.bad("过期时间必须晚于现在")
		return
	case utf8.RuneCountInString(req.Note) > 100:
		c.bad("备注不能超过 100 个字符")
		return
	}
	invites := make([]store.Invite, req.Count)
	for i := range invites {
		code, err := newInviteCode()
		if err != nil {
			c.internal(err)
			return
		}
		invites[i] = store.Invite{Code: code, MaxUses: req.MaxUses, ExpiresAt: req.ExpiresAt, Note: req.Note, CreatedBy: c.admin.Username, CreatedAt: c.now().UnixMilli()}
	}
	// 邀请码本身不进审计（审计日志可被所有 owner 查看，邀请码等同于注册凭证）
	audit := c.audit("invite.create", "invite", jsonDetail(obj{"count": req.Count, "max_uses": req.MaxUses, "expires_at": req.ExpiresAt, "note": req.Note}))
	if err := c.s.st.CreateInvites(c.r.Context(), invites, audit); err != nil {
		c.internal(err)
		return
	}
	c.ok(invites)
}

// POST /admin/api/invites/{code}/revoke → {}
func (c *ctx) revokeInvite() {
	code := store.NormalizeInvite(c.path("code"))
	if err := c.s.st.RevokeInvite(c.r.Context(), code, c.audit("invite.revoke", "invite:"+maskCode(code), "")); err != nil {
		c.storeErr(err, "邀请码不存在")
		return
	}
	c.ok(obj{})
}

func maskCode(code string) string {
	if len(code) <= 4 {
		return "****"
	}
	return code[:4] + "…"
}

// GET /admin/api/invites/{code}/uses → [使用记录]
func (c *ctx) inviteUses() {
	list, err := c.s.st.InviteUses(c.r.Context(), store.NormalizeInvite(c.path("code")))
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(list)
}

// ---------- A12 系统公告 ----------

// GET /admin/api/announcements → [Announcement]（含已过期的，最多 200 条）
func (c *ctx) listAnnouncements() {
	list, err := c.s.st.ListAnnouncements(c.r.Context(), 1<<62, 200, false)
	if err != nil {
		c.internal(err)
		return
	}
	c.ok(list)
}

// POST /admin/api/announcements {title, body, expires_at?} → {announcement, recipients}
//
// 公告与发给全体（未停用）用户的 announcement 事件在同一事务内写入（outbox）：事件 id 就是公告 id，
// 客户端照常 ack；离线用户上线后作为积压收到，重新登录清空队列的用户通过 GET /v1/announcements 补拉。
func (c *ctx) publishAnnouncement() {
	var req struct {
		Title     string `json:"title"`
		Body      string `json:"body"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if !c.decode(&req) {
		return
	}
	req.Title, req.Body = strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
	now := c.now().UnixMilli()
	switch {
	case req.Title == "" || utf8.RuneCountInString(req.Title) > 100:
		c.bad("标题须为 1–100 个字符")
		return
	case req.Body == "" || utf8.RuneCountInString(req.Body) > 5000:
		c.bad("正文须为 1–5000 个字符")
		return
	case req.ExpiresAt != 0 && req.ExpiresAt <= now:
		c.bad("截止时间必须晚于现在")
		return
	}
	a := store.Announcement{ID: store.NewID(), Title: req.Title, Body: req.Body, CreatedAt: now, ExpiresAt: req.ExpiresAt, CreatedBy: c.admin.Username}
	if _, err := store.AnnouncementEvent(a); err != nil {
		c.internal(err)
		return
	}
	err := c.s.st.Update(c.r.Context(), func(t *store.Tx) error {
		if err := t.CreateAnnouncement(a); err != nil {
			return err
		}
		return t.AppendAudit(c.audit("announcement.publish", "announcement:"+a.ID, jsonDetail(obj{"title": a.Title, "expires_at": a.ExpiresAt})))
	})
	if err != nil {
		c.internal(err)
		return
	}
	// 扇出分批进行（每批一个小事务），不在一个大事务里占住写连接；进度落库，进程中途退出会在下次启动时续发。
	// 用不随请求取消的 ctx：管理员关掉页面不应让公告只发到一半。
	recipients, err := c.s.st.FanoutAnnouncement(context.WithoutCancel(c.r.Context()), a.ID)
	if err != nil {
		log.Printf("[admin] 公告 %s 扇出中断（已发 %d 人，重启后续发）: %v", a.ID, recipients, err)
		c.internal(err)
		return
	}
	log.Printf("[admin] %s 发布公告 %s -> %d 个用户", c.admin.Username, a.ID, recipients)
	c.ok(obj{"announcement": a, "recipients": recipients})
}

// DELETE /admin/api/announcements/{id} → {}（撤下：不再出现在历史里；已推送的事件不回收）
func (c *ctx) deleteAnnouncement() {
	id := c.path("id")
	if err := c.s.st.DeleteAnnouncement(c.r.Context(), id, c.audit("announcement.delete", "announcement:"+id, "")); err != nil {
		c.storeErr(err, "公告不存在")
		return
	}
	c.ok(obj{})
}

// ---------- A13 系统设置 ----------

// GET /admin/api/settings → {fields, values, defaults, overridden}
func (c *ctx) getSettings() {
	kv, err := c.s.st.Settings(c.r.Context())
	if err != nil {
		c.internal(err)
		return
	}
	overridden := []string{}
	for k := range kv {
		overridden = append(overridden, k)
	}
	sort.Strings(overridden)
	c.ok(obj{"fields": api.SettingFields, "values": c.s.api.Settings().Map(), "defaults": c.s.api.Defaults().Map(), "overridden": overridden})
}

// PUT /admin/api/settings {values: {key: value}, reset: [key]} → 同 GET（owner 专属；写入 settings 表并立即生效）
func (c *ctx) putSettings() {
	var req struct {
		Values map[string]string `json:"values"`
		Reset  []string          `json:"reset"`
	}
	if !c.decode(&req) {
		return
	}
	if len(req.Values) == 0 && len(req.Reset) == 0 {
		c.bad("没有要修改的设置")
		return
	}
	known := map[string]bool{}
	for _, f := range api.SettingFields {
		known[f.Key] = true
	}
	for _, k := range req.Reset {
		if !known[k] {
			c.bad("未知的设置项 " + k)
			return
		}
		delete(req.Values, k)
	}
	// 先在「默认值 + 已保存的覆盖 + 本次修改」上整体校验，全部合法才写库
	rctx := c.r.Context()
	saved, err := c.s.st.Settings(rctx)
	if err != nil {
		c.internal(err)
		return
	}
	for _, k := range req.Reset {
		delete(saved, k)
	}
	next := c.s.api.Defaults()
	for k, v := range saved {
		if _, overriding := req.Values[k]; overriding {
			continue
		}
		if n, err := next.WithOverrides(map[string]string{k: v}); err == nil {
			next = n
		}
	}
	next, err = next.WithOverrides(req.Values)
	if err != nil {
		c.bad(err.Error())
		return
	}
	normalized := next.Map()
	toSave := map[string]string{}
	for k := range req.Values {
		toSave[k] = normalized[k]
	}
	if req.Reset == nil {
		req.Reset = []string{}
	}
	audit := c.audit("settings.update", "settings", jsonDetail(obj{"values": toSave, "reset": req.Reset}))
	if err := c.s.st.SaveSettings(rctx, toSave, req.Reset, c.admin.Username, audit); err != nil {
		c.internal(err)
		return
	}
	c.s.api.ApplySettings(next)
	log.Printf("[admin] %s 修改系统设置 %v 重置 %v", c.admin.Username, toSave, req.Reset)
	c.getSettings()
}

// ---------- A14 存储与维护 ----------

var backupNameRe = regexp.MustCompile(`^hush-\d{8}-\d{6}\.db$`)

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, "backups") }

type backupInfo struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

// listBackups 返回备份列表（新的在前）。
func (s *Server) listBackups() []backupInfo {
	out := []backupInfo{}
	entries, err := os.ReadDir(s.backupDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			out = append(out, backupInfo{Name: e.Name(), Size: info.Size(), CreatedAt: info.ModTime().UnixMilli()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// GET /admin/api/storage → 附件占用排行、总量与备份列表
func (c *ctx) storage() {
	rctx := c.r.Context()
	now := c.now()
	totals, err := c.s.st.Totals(rctx, now.UnixMilli(), store.DayOf(now))
	if err != nil {
		c.internal(err)
		return
	}
	top, err := c.s.st.TopUploaders(rctx, 20)
	if err != nil {
		c.internal(err)
		return
	}
	var dbSize int64
	for _, name := range []string{"hush.db", "hush.db-wal"} {
		if info, err := os.Stat(filepath.Join(c.s.cfg.DataDir, name)); err == nil {
			dbSize += info.Size()
		}
	}
	c.ok(obj{
		"attachments": totals.Attachments, "attachment_bytes": totals.AttachmentBytes,
		"queue_items": totals.QueueItems, "queue_bytes": totals.QueueBytes,
		"db_bytes": dbSize, "top_uploaders": top, "backups": c.s.listBackups(),
		"backup_keep": c.s.api.Settings().BackupKeep,
	})
}

// POST /admin/api/maintenance/cleanup → 清理结果（与每小时的后台清理相同：过期队列项、闲置附件、残留临时文件）
func (c *ctx) cleanup() {
	cs, err := api.Cleanup(c.r.Context(), c.s.st, c.s.cfg.DataDir, time.Now())
	if err != nil {
		c.internal(err)
		return
	}
	c.writeAudit("maintenance.cleanup", "", jsonDetail(obj{"queue_items": cs.QueueItems, "attachments": cs.Attachments, "temp_files": cs.TempFiles}))
	c.ok(obj{"queue_items": cs.QueueItems, "attachments": cs.Attachments, "temp_files": cs.TempFiles})
}

// POST /admin/api/backups → {backup, removed}
// SQLite VACUUM INTO 在线备份到 data/backups/，只保留最近 backup_keep 份。备份里**不含离线队列中的信封**（见 store.BackupInto）。
func (c *ctx) createBackup() {
	s := c.s
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	dir := s.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.internal(err)
		return
	}
	name := "hush-" + time.Now().Format("20060102-150405") + ".db"
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		c.conflict("同一秒内已有备份，请稍后再试")
		return
	}
	// 先写到临时文件名（不匹配备份文件名规则，列不出来也下载不到），清掉信封之后再改名：
	// 任何时候都不会有一个带信封的文件出现在可下载的名字下面。
	tmp := path + ".tmp"
	for _, leftover := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
		os.Remove(leftover)
	}
	err := s.st.BackupInto(c.r.Context(), tmp)
	if err == nil {
		if err = os.Chmod(tmp, 0o600); err == nil {
			err = os.Rename(tmp, path)
		}
	}
	if err != nil {
		for _, leftover := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
			os.Remove(leftover)
		}
		c.internal(err)
		return
	}
	var removed []string
	list := s.listBackups()
	keep := s.api.Settings().BackupKeep
	for i := keep; i < len(list); i++ {
		if err := os.Remove(filepath.Join(dir, list[i].Name)); err == nil {
			removed = append(removed, list[i].Name)
		}
	}
	info := backupInfo{Name: name}
	if st, err := os.Stat(path); err == nil {
		info.Size, info.CreatedAt = st.Size(), st.ModTime().UnixMilli()
	}
	if removed == nil {
		removed = []string{}
	}
	c.writeAudit("backup.create", "backup:"+name, jsonDetail(obj{"size": info.Size, "removed": removed}))
	c.ok(obj{"backup": info, "removed": removed})
}

// GET /admin/api/backups/{name} → 备份文件（owner 专属，下载会记入审计）
//
// 备份包含账号资料、关系、朋友圈、管理员密码哈希等，应按最高敏感级别保管；但**不含任何信封**：
// 离线队列里尚未投递的消息密文在生成备份时就已从副本中清除（隐私边界：后台没有任何拿到消息内容的途径，包括密文）。
func (c *ctx) downloadBackup() {
	name := c.path("name")
	if !backupNameRe.MatchString(name) {
		c.notFound("备份不存在")
		return
	}
	path := filepath.Join(c.s.backupDir(), name)
	f, err := os.Open(path)
	if err != nil {
		c.notFound("备份不存在")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		c.notFound("备份不存在")
		return
	}
	if err := c.s.st.AppendAudit(c.r.Context(), c.audit("backup.download", "backup:"+name, jsonDetail(obj{"size": st.Size()}))); err != nil {
		c.internal(err) // 审计写不进去就不给下载
		return
	}
	http.NewResponseController(c.w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
	c.w.Header().Set("Content-Type", "application/octet-stream")
	c.w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeContent(c.w, c.r, "", st.ModTime(), f)
}

// ---------- A15 监控 ----------

type runtimeStats struct {
	Goroutines int    `json:"goroutines"`
	HeapAlloc  uint64 `json:"heap_alloc"`
	HeapInuse  uint64 `json:"heap_inuse"`
	Sys        uint64 `json:"sys"`
	NumGC      uint32 `json:"num_gc"`
}

func readRuntime() runtimeStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return runtimeStats{Goroutines: runtime.NumGoroutine(), HeapAlloc: m.HeapAlloc, HeapInuse: m.HeapInuse, Sys: m.Sys, NumGC: m.NumGC}
}

// GET /admin/api/monitor → 实时指标（页面每 5 秒拉一次）
func (c *ctx) monitor() {
	now := c.now()
	totals, err := c.s.st.Totals(c.r.Context(), now.UnixMilli(), store.DayOf(now))
	if err != nil {
		c.internal(err)
		return
	}
	db := c.s.st.DBStats()
	c.ok(obj{
		"ts":      time.Now().UnixMilli(),
		"online":  c.s.hub.OnlineCount(),
		"metrics": c.s.api.Metrics().Snapshot(),
		"db": obj{"wait_count": db.WaitCount, "wait_ms": float64(db.WaitDuration.Microseconds()) / 1000,
			"in_use": db.InUse, "open": db.OpenConnections},
		"queue_items": totals.QueueItems, "queue_bytes": totals.QueueBytes,
		"runtime": readRuntime(),
	})
}

// GET /metrics → Prometheus 文本格式。只挂在管理监听地址上；需要后台会话，或 Authorization: Bearer <HUSH_METRICS_TOKEN>。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	authed := false
	if tok := s.cfg.MetricsToken; tok != "" {
		if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			authed = subtle.ConstantTimeCompare([]byte(strings.TrimSpace(h[7:])), []byte(tok)) == 1
		}
	}
	if !authed {
		if _, _, ok := s.session(w, r); !ok {
			return
		}
	}
	now := s.cfg.Now()
	totals, err := s.st.Totals(r.Context(), now.UnixMilli(), store.DayOf(now))
	if err != nil {
		internal(w, r, err)
		return
	}
	db := s.st.DBStats()
	ws := s.st.WriterStats()
	rp := s.st.ReadPoolStats()
	rt := readRuntime()
	gauges := []metrics.Gauge{
		{Name: "hush_ws_connections", Help: "当前在线 WebSocket 连接数。", Value: float64(s.hub.OnlineCount())},
		{Name: "hush_users", Help: "注册用户数。", Value: float64(totals.Users)},
		{Name: "hush_groups", Help: "群组数。", Value: float64(totals.Groups)},
		{Name: "hush_queue_items", Help: "离线队列条数（全部用户合计）。", Value: float64(totals.QueueItems)},
		{Name: "hush_queue_bytes", Help: "离线队列负载字节数（全部用户合计）。", Value: float64(totals.QueueBytes)},
		{Name: "hush_attachments", Help: "附件个数。", Value: float64(totals.Attachments)},
		{Name: "hush_attachment_bytes", Help: "附件占用字节数。", Value: float64(totals.AttachmentBytes)},
		{Name: "hush_db_wait_count", Help: "交给写协程的写操作累计数（写队列的排队次数）。", Value: float64(db.WaitCount)},
		{Name: "hush_db_wait_seconds", Help: "写操作在写队列里等待的累计时长。", Value: db.WaitDuration.Seconds()},
		{Name: "hush_db_read_wait_count", Help: "等不到空闲只读连接的累计次数。", Value: float64(rp.WaitCount)},
		{Name: "hush_db_read_wait_seconds", Help: "等待只读连接的累计时长。", Value: rp.WaitDuration.Seconds()},
		{Name: "hush_db_write_ops", Help: "交给写协程的写操作累计数。", Value: float64(ws.Ops)},
		{Name: "hush_db_write_commits", Help: "写协程的提交次数（write_ops / write_commits = 平均每次提交合并的操作数）。", Value: float64(ws.Batches)},
		{Name: "hush_db_write_work_seconds", Help: "执行写操作本身的累计时间。", Value: ws.Work.Seconds()},
		{Name: "hush_db_write_commit_seconds", Help: "COMMIT（写 WAL）的累计时间。", Value: ws.Commit.Seconds()},
		{Name: "hush_db_write_hook_seconds", Help: "提交钩子（把队列项交给在线连接）的累计时间。", Value: ws.Hooks.Seconds()},
		{Name: "go_goroutines", Help: "Go 协程数。", Value: float64(rt.Goroutines)},
		{Name: "go_memstats_heap_alloc_bytes", Help: "堆上已分配字节数。", Value: float64(rt.HeapAlloc)},
		{Name: "go_memstats_sys_bytes", Help: "向操作系统申请的字节数。", Value: float64(rt.Sys)},
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.api.Metrics().WritePrometheus(w, gauges)
}
