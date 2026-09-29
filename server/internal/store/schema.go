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
}
