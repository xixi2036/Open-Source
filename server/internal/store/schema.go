package store

// migrations 按顺序执行，索引+1 即 PRAGMA user_version。只允许追加，不要修改已发布的条目。
var migrations = []string{
	`
CREATE TABLE users (
	id                TEXT PRIMARY KEY,
	email             TEXT NOT NULL UNIQUE,
	name              TEXT NOT NULL DEFAULT '',
	about             TEXT NOT NULL DEFAULT '',
	avatar_id         TEXT,
	device_generation INTEGER NOT NULL DEFAULT 1,
	platform          TEXT NOT NULL DEFAULT '',
	device_name       TEXT NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL
);

CREATE TABLE tokens (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL
);
CREATE INDEX tokens_user ON tokens(user_id);

CREATE TABLE auth_codes (
	email      TEXT PRIMARY KEY,
	code       TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	sent_at    INTEGER NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE identity_keys (
	user_id           TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	identity_key      TEXT NOT NULL,
	signing_key       TEXT NOT NULL,
	device_generation INTEGER NOT NULL
);

CREATE TABLE one_time_keys (
	seq       INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	key_id    TEXT NOT NULL,
	key       TEXT NOT NULL,
	signature TEXT NOT NULL,
	UNIQUE(user_id, key_id)
);

CREATE TABLE fallback_keys (
	user_id   TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	key_id    TEXT NOT NULL,
	key       TEXT NOT NULL,
	signature TEXT NOT NULL
);

-- 信封与事件统一的按接收方持久化队列；ack 后删除。
CREATE TABLE queue (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    TEXT NOT NULL,
	id         TEXT NOT NULL,
	kind       TEXT NOT NULL, -- envelope | event
	payload    TEXT NOT NULL, -- 帧的 d 部分（JSON）
	created_at INTEGER NOT NULL,
	UNIQUE(user_id, id)
);
CREATE INDEX queue_user_seq ON queue(user_id, seq);

CREATE TABLE friend_requests (
	id         TEXT PRIMARY KEY,
	from_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	to_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	message    TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL, -- pending | accepted | rejected
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX friend_requests_to ON friend_requests(to_id, status);
CREATE INDEX friend_requests_from ON friend_requests(from_id, status);

-- 好友关系双向各存一行。
CREATE TABLE friendships (
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	friend_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(user_id, friend_id)
);

CREATE TABLE blocks (
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	blocked_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(user_id, blocked_id)
);

CREATE TABLE groups (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	avatar_id    TEXT,
	announcement TEXT NOT NULL DEFAULT '',
	owner_id     TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);

CREATE TABLE group_members (
	group_id  TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role      TEXT NOT NULL, -- owner | admin | member
	joined_at INTEGER NOT NULL,
	PRIMARY KEY(group_id, user_id)
);
CREATE INDEX group_members_user ON group_members(user_id);

CREATE TABLE attachments (
	id          TEXT PRIMARY KEY,
	uploader_id TEXT NOT NULL,
	size        INTEGER NOT NULL,
	created_at  INTEGER NOT NULL
);

CREATE TABLE moments (
	id         TEXT PRIMARY KEY,
	author_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	text       TEXT NOT NULL DEFAULT '',
	media      TEXT NOT NULL DEFAULT '[]',
	vis_mode   TEXT NOT NULL DEFAULT 'all', -- all | include | exclude
	created_at INTEGER NOT NULL
);
CREATE INDEX moments_author_time ON moments(author_id, created_at);

CREATE TABLE moment_visibility (
	moment_id TEXT NOT NULL REFERENCES moments(id) ON DELETE CASCADE,
	user_id   TEXT NOT NULL,
	PRIMARY KEY(moment_id, user_id)
);

CREATE TABLE moment_likes (
	moment_id  TEXT NOT NULL REFERENCES moments(id) ON DELETE CASCADE,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(moment_id, user_id)
);

CREATE TABLE moment_comments (
	id          TEXT PRIMARY KEY,
	moment_id   TEXT NOT NULL REFERENCES moments(id) ON DELETE CASCADE,
	author_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	reply_to_id TEXT,
	text        TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX moment_comments_moment ON moment_comments(moment_id, created_at);

CREATE TABLE moment_notifications (
	id         TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,             -- 接收者
	kind       TEXT NOT NULL,             -- like | comment
	moment_id  TEXT NOT NULL REFERENCES moments(id) ON DELETE CASCADE,
	from_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	comment_id TEXT,                      -- kind=comment 时对应评论
	text       TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX moment_notifications_user ON moment_notifications(user_id, created_at);
`,
	// v2：附件最近访问时间（90 天未访问清理）、按上传者统计每日用量、队列按时间清理。
	`
ALTER TABLE attachments ADD COLUMN last_access INTEGER NOT NULL DEFAULT 0;
UPDATE attachments SET last_access = created_at;
CREATE INDEX attachments_uploader_time ON attachments(uploader_id, created_at);
CREATE INDEX attachments_last_access ON attachments(last_access);
CREATE INDEX queue_created ON queue(created_at);
`,
	// v3：
	//   - 队列按字节计量（size 列）并由触发器维护按用户的用量计数器 queue_usage，配额检查 O(1)；
	//   - 队列事件可带合并键（同一主体的旧事件在入队时被新事件替换）；
	//   - 同一 (from,to) 只允许一条待处理好友申请（先去重再建部分唯一索引，旧库里可能已有重复）；
	//   - 验证码按邮箱的滚动失败计数（重发不清零）。
	`
ALTER TABLE queue ADD COLUMN size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue ADD COLUMN coalesce_key TEXT;
UPDATE queue SET size = LENGTH(CAST(payload AS BLOB));
CREATE INDEX queue_coalesce ON queue(user_id, coalesce_key) WHERE coalesce_key IS NOT NULL;

CREATE TABLE queue_usage (
	user_id TEXT PRIMARY KEY,
	items   INTEGER NOT NULL DEFAULT 0,
	bytes   INTEGER NOT NULL DEFAULT 0
);
INSERT INTO queue_usage (user_id, items, bytes) SELECT user_id, COUNT(*), SUM(size) FROM queue GROUP BY user_id;
CREATE TRIGGER queue_usage_ins AFTER INSERT ON queue BEGIN
	INSERT OR IGNORE INTO queue_usage (user_id, items, bytes) VALUES (NEW.user_id, 0, 0);
	UPDATE queue_usage SET items = items + 1, bytes = bytes + NEW.size WHERE user_id = NEW.user_id;
END;
CREATE TRIGGER queue_usage_del AFTER DELETE ON queue BEGIN
	UPDATE queue_usage SET items = items - 1, bytes = bytes - OLD.size WHERE user_id = OLD.user_id;
END;

DELETE FROM friend_requests WHERE status = 'pending' AND rowid NOT IN (
	SELECT MAX(rowid) FROM friend_requests WHERE status = 'pending' GROUP BY from_id, to_id);
CREATE UNIQUE INDEX friend_requests_pending ON friend_requests(from_id, to_id) WHERE status = 'pending';

ALTER TABLE auth_codes ADD COLUMN failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE auth_codes ADD COLUMN failures_since INTEGER NOT NULL DEFAULT 0;
`,
	// v4：令牌滑动过期（S-C）。已有令牌的最近使用时间取迁移时刻。
	`
ALTER TABLE tokens ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0;
UPDATE tokens SET last_used_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000;
`,
	// v5：动态与附件的关联表（S-E）。附件清理按它判断「被朋友圈引用」，不再用 json_each 扫描全部动态；
	// 从已有动态的 media JSON 回填。头像引用判断也改为走索引。
	`
CREATE TABLE moment_media (
	moment_id     TEXT NOT NULL REFERENCES moments(id) ON DELETE CASCADE,
	attachment_id TEXT NOT NULL,
	PRIMARY KEY(moment_id, attachment_id)
);
CREATE INDEX moment_media_attachment ON moment_media(attachment_id);
INSERT OR IGNORE INTO moment_media (moment_id, attachment_id)
	SELECT m.id, json_extract(j.value, '$.attachment_id') FROM moments m, json_each(m.media) j
	WHERE json_extract(j.value, '$.attachment_id') IS NOT NULL;
CREATE INDEX users_avatar ON users(avatar_id) WHERE avatar_id IS NOT NULL;
CREATE INDEX groups_avatar ON groups(avatar_id) WHERE avatar_id IS NOT NULL;
`,
	// v6：管理后台（2026-09-30 admin-and-scale A 系列）。全部为新增表 / 新增列，不改动已有数据：
	//   - users.disabled_at（停用时间，0=正常）、users.last_active_at（最近一次认证请求，节流写入）；
	//   - admins / admin_sessions / admin_audit（审计表由触发器禁止 UPDATE、DELETE）；
	//   - settings（在线设置覆盖启动参数）、reports（举报）、invite_codes / invite_uses、announcements；
	//   - user_activity（按天去重的活跃用户）、stats_hourly（每小时入队信封数）、group_stats（群消息计数）。
	`
ALTER TABLE users ADD COLUMN disabled_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN last_active_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX users_created ON users(created_at);
CREATE INDEX users_last_active ON users(last_active_at);

CREATE TABLE admins (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL, -- owner | moderator | viewer
	disabled      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	last_login_at INTEGER NOT NULL DEFAULT 0,
	failed_count  INTEGER NOT NULL DEFAULT 0, -- 连续登录失败次数
	locked_until  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE admin_sessions (
	token_hash   TEXT PRIMARY KEY,
	admin_id     TEXT NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	ip           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX admin_sessions_admin ON admin_sessions(admin_id);

-- 审计日志：不引用 admins（管理员被删除后记录仍在），只允许追加。
CREATE TABLE admin_audit (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         INTEGER NOT NULL,
	admin_id   TEXT NOT NULL,
	admin_name TEXT NOT NULL,
	action     TEXT NOT NULL,
	target     TEXT NOT NULL DEFAULT '',
	detail     TEXT NOT NULL DEFAULT '',
	ip         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX admin_audit_ts ON admin_audit(ts);
CREATE INDEX admin_audit_admin ON admin_audit(admin_name, ts);
CREATE INDEX admin_audit_action ON admin_audit(action, ts);
CREATE TRIGGER admin_audit_no_update BEFORE UPDATE ON admin_audit BEGIN
	SELECT RAISE(ABORT, 'admin_audit is append-only');
END;
CREATE TRIGGER admin_audit_no_delete BEFORE DELETE ON admin_audit BEGIN
	SELECT RAISE(ABORT, 'admin_audit is append-only');
END;

CREATE TABLE settings (
	key        TEXT PRIMARY KEY,
	value      TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	updated_by TEXT NOT NULL DEFAULT ''
);

CREATE TABLE reports (
	id          TEXT PRIMARY KEY,
	reporter_id TEXT NOT NULL,
	target_type TEXT NOT NULL, -- user | moment | comment | group
	target_id   TEXT NOT NULL,
	reason      TEXT NOT NULL,
	detail      TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT 'pending', -- pending | resolved
	action      TEXT NOT NULL DEFAULT '',        -- ignore | delete_content | disable_account
	note        TEXT NOT NULL DEFAULT '',
	handled_by  TEXT NOT NULL DEFAULT '',
	handled_at  INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
);
CREATE INDEX reports_status_time ON reports(status, created_at);
CREATE INDEX reports_target ON reports(target_type, target_id, status);
CREATE UNIQUE INDEX reports_pending_once ON reports(reporter_id, target_type, target_id) WHERE status = 'pending';

CREATE TABLE invite_codes (
	code       TEXT PRIMARY KEY,
	max_uses   INTEGER NOT NULL DEFAULT 1,
	used       INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL DEFAULT 0, -- 0 = 不过期
	revoked    INTEGER NOT NULL DEFAULT 0,
	note       TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE invite_uses (
	code    TEXT NOT NULL,
	user_id TEXT NOT NULL,
	email   TEXT NOT NULL,
	used_at INTEGER NOT NULL
);
CREATE INDEX invite_uses_code ON invite_uses(code, used_at);

CREATE TABLE announcements (
	id         TEXT PRIMARY KEY,
	title      TEXT NOT NULL,
	body       TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0, -- 0 = 不过期
	created_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX announcements_time ON announcements(created_at);

CREATE TABLE user_activity (
	day     INTEGER NOT NULL, -- 服务器本地日期 yyyymmdd
	user_id TEXT NOT NULL,
	PRIMARY KEY(day, user_id)
) WITHOUT ROWID;

CREATE TABLE stats_hourly (
	hour      INTEGER PRIMARY KEY, -- Unix 小时（秒 / 3600）
	envelopes INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE group_stats (
	group_id        TEXT PRIMARY KEY,
	messages        INTEGER NOT NULL DEFAULT 0,
	last_message_at INTEGER NOT NULL DEFAULT 0
);
`,
	// v7：缺陷修复与性能升级（2026-09-30 admin-and-scale 第 2、3 节）。
	//   - envelopes：同一条内容发给很多人时正文只存一份（群消息、发给多人的事件），队列行用 shared=1 按 id 引用；
	//     最后一个引用被删除时由触发器删掉正文。迁移把已有队列里「同一个 id 出现在多个接收方」的正文搬过去。
	//   - queue.sender_id：信封的发送方（从已有信封的 from 字段回填），用于每（发送方, 接收方）子配额。
	//   - queue_usage.env_items / env_bytes：只统计信封的用量——配额只看它，事件不再占用信封配额。
	//   - send_dedup：(发送方, 目标, client_id) 24 小时幂等键（存哈希）。
	//   - auth_failures：验证码失败次数按（邮箱, IP）分桶。
	//   - announcements.fanout_*：公告事件分批扇出的进度（重启后续发）。
	//   - 清理用的索引（朋友圈通知、已处理的好友申请）。
	`
CREATE TABLE envelopes (
	id         TEXT PRIMARY KEY,
	payload    TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
ALTER TABLE queue ADD COLUMN shared INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue ADD COLUMN sender_id TEXT;
UPDATE queue SET sender_id = json_extract(payload, '$.from') WHERE kind = 'envelope' AND json_valid(payload);
INSERT INTO envelopes (id, payload, created_at)
	SELECT id, payload, MIN(created_at) FROM queue GROUP BY id HAVING COUNT(*) > 1;
UPDATE queue SET shared = 1, payload = '' WHERE id IN (SELECT id FROM envelopes);
CREATE INDEX queue_shared_id ON queue(id) WHERE shared = 1;
CREATE INDEX queue_sender ON queue(user_id, sender_id, size) WHERE sender_id IS NOT NULL;
CREATE TRIGGER queue_shared_del AFTER DELETE ON queue WHEN OLD.shared = 1 BEGIN
	DELETE FROM envelopes WHERE id = OLD.id AND NOT EXISTS (SELECT 1 FROM queue WHERE id = OLD.id AND shared = 1);
END;

ALTER TABLE queue_usage ADD COLUMN env_items INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_usage ADD COLUMN env_bytes INTEGER NOT NULL DEFAULT 0;
UPDATE queue_usage SET
	env_items = (SELECT COUNT(*) FROM queue q WHERE q.user_id = queue_usage.user_id AND q.kind = 'envelope'),
	env_bytes = (SELECT COALESCE(SUM(q.size), 0) FROM queue q WHERE q.user_id = queue_usage.user_id AND q.kind = 'envelope');
DROP TRIGGER queue_usage_ins;
DROP TRIGGER queue_usage_del;
CREATE TRIGGER queue_usage_ins AFTER INSERT ON queue BEGIN
	INSERT INTO queue_usage (user_id, items, bytes, env_items, env_bytes)
		VALUES (NEW.user_id, 1, NEW.size, NEW.kind = 'envelope', CASE WHEN NEW.kind = 'envelope' THEN NEW.size ELSE 0 END)
		ON CONFLICT(user_id) DO UPDATE SET items = items + 1, bytes = bytes + NEW.size,
			env_items = env_items + (NEW.kind = 'envelope'),
			env_bytes = env_bytes + CASE WHEN NEW.kind = 'envelope' THEN NEW.size ELSE 0 END;
END;
CREATE TRIGGER queue_usage_del AFTER DELETE ON queue BEGIN
	UPDATE queue_usage SET items = items - 1, bytes = bytes - OLD.size,
		env_items = env_items - (OLD.kind = 'envelope'),
		env_bytes = env_bytes - CASE WHEN OLD.kind = 'envelope' THEN OLD.size ELSE 0 END
		WHERE user_id = OLD.user_id;
END;

-- key = SHA-256(发送方 \0 目标 \0 client_id) 的前 16 字节（目标：单聊为接收方 id，群消息为群 id）；
-- id 是首次发送时分配的信封 id（UUID 的 16 字节）。每条消息只占几十字节。
CREATE TABLE send_dedup (
	key       BLOB PRIMARY KEY,
	id        BLOB NOT NULL,
	server_ts INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX send_dedup_ts ON send_dedup(server_ts);

CREATE TABLE auth_failures (
	email         TEXT NOT NULL,
	ip            TEXT NOT NULL,
	failures      INTEGER NOT NULL DEFAULT 0, -- 滚动窗口内的失败次数
	since         INTEGER NOT NULL DEFAULT 0, -- 窗口起点
	code_attempts INTEGER NOT NULL DEFAULT 0, -- 对当前这个验证码的失败次数（换新验证码时清零）
	updated_at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(email, ip)
) WITHOUT ROWID;
CREATE INDEX auth_failures_updated ON auth_failures(updated_at);

ALTER TABLE announcements ADD COLUMN fanout_done INTEGER NOT NULL DEFAULT 1;
ALTER TABLE announcements ADD COLUMN fanout_cursor TEXT NOT NULL DEFAULT '';
ALTER TABLE announcements ADD COLUMN recipients INTEGER NOT NULL DEFAULT 0;

CREATE INDEX moment_notifications_created ON moment_notifications(created_at);
CREATE INDEX friend_requests_done ON friend_requests(updated_at) WHERE status <> 'pending';
`,
}
