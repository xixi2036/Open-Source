//! 本地 SQLite 存储。敏感字段（消息正文 JSON、附件密钥、登录 token）用库密钥 AES-256-GCM 加密；
//! vodozemac pickle 使用 vodozemac 自带的 pickle 加密（密钥同为库密钥）。

use std::collections::{BTreeMap, HashSet};
use std::path::Path;
use std::sync::Mutex;

use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use vodozemac::megolm::{GroupSession, InboundGroupSession};

use crate::content::WireAttachment;
use crate::crypto::fieldenc::DbCipher;
use crate::crypto::{megolm, olm};
use crate::error::{HushError, Result};
use crate::types::*;

pub use inbox::InboxPlain;

mod inbox;
mod messages;
mod sessions;
mod social;
#[cfg(test)]
mod tests;

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS profiles (user_id TEXT PRIMARY KEY, json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS friends (user_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS blocks (user_id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS identities (
  user_id TEXT PRIMARY KEY, identity_key TEXT NOT NULL, signing_key TEXT NOT NULL,
  device_generation INTEGER NOT NULL DEFAULT 0, verified INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS olm_sessions (
  user_id TEXT NOT NULL, session_id TEXT NOT NULL, pickle TEXT NOT NULL, last_used INTEGER NOT NULL,
  PRIMARY KEY (user_id, session_id));
CREATE TABLE IF NOT EXISTS megolm_out (
  group_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, pickle TEXT NOT NULL,
  created_at INTEGER NOT NULL, msg_count INTEGER NOT NULL, shared_with TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS megolm_in (
  sender TEXT NOT NULL, session_id TEXT NOT NULL, group_id TEXT NOT NULL, pickle TEXT NOT NULL,
  PRIMARY KEY (sender, session_id));
CREATE TABLE IF NOT EXISTS megolm_pending (
  envelope_id TEXT PRIMARY KEY, group_id TEXT NOT NULL, sender TEXT NOT NULL, session_id TEXT NOT NULL,
  body TEXT NOT NULL, server_ts INTEGER NOT NULL, received_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS groups (group_id TEXT PRIMARY KEY, json TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS conversations (
  conv_id TEXT PRIMARY KEY, last_ts INTEGER NOT NULL DEFAULT 0, unread INTEGER NOT NULL DEFAULT 0,
  pinned INTEGER NOT NULL DEFAULT 0, muted INTEGER NOT NULL DEFAULT 0, archived INTEGER NOT NULL DEFAULT 0,
  expire_secs INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY, conv_id TEXT NOT NULL, sender_id TEXT NOT NULL, outgoing INTEGER NOT NULL,
  sent_at INTEGER NOT NULL, server_ts INTEGER NOT NULL DEFAULT 0, sort_ts INTEGER NOT NULL,
  status TEXT NOT NULL, body BLOB NOT NULL, expire_secs INTEGER NOT NULL DEFAULT 0, expires_at INTEGER,
  deleted INTEGER NOT NULL DEFAULT 0, read INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS messages_conv ON messages(conv_id, sort_ts);
CREATE INDEX IF NOT EXISTS messages_exp ON messages(expires_at);
CREATE TABLE IF NOT EXISTS reactions (
  message_id TEXT NOT NULL, user_id TEXT NOT NULL, emoji TEXT NOT NULL, ts INTEGER NOT NULL,
  PRIMARY KEY (message_id, user_id));
CREATE TABLE IF NOT EXISTS seen (id TEXT PRIMARY KEY, ts INTEGER NOT NULL);
"#;

/// v2：收件箱（C-H）。收到的信封先原样落库，解密后明文（字段加密）与推进后的棘轮状态在同一事务写入该行，
/// 处理完成后删除。
const V2_INBOX: &str = r#"
CREATE TABLE IF NOT EXISTS inbox (
  envelope_id TEXT PRIMARY KEY, envelope TEXT NOT NULL, plaintext BLOB, received_at INTEGER NOT NULL);
"#;

/// v4（v3 收尾，K3/K5/K6）：消息表加「提及我」标记（未读提及计数用部分索引）、草稿表（字段加密）；
/// 已发送消息的排序时间回填为服务端时间，并把同一会话内相同的 sort_ts 错开（翻页游标不漏不重）。
const V4_SCHEMA: &str = r#"
ALTER TABLE messages ADD COLUMN mentions_me INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS messages_mentions ON messages(conv_id) WHERE mentions_me=1 AND read=0;
CREATE TABLE IF NOT EXISTS drafts (conv_id TEXT PRIMARY KEY, text BLOB NOT NULL, updated_at INTEGER NOT NULL);
UPDATE messages SET sort_ts=server_ts WHERE outgoing=1 AND server_ts>0 AND sort_ts<>server_ts;
"#;

/// v5（2026-09-30 审查修复）：
/// - `messages.has_files`：该消息是否有本地附件文件（C5：删除 / 清空会话时不必解密全部消息找附件），随后回填；
/// - 部分索引 `messages_unread`（C4 / A10：未读数实时统计、`mark_read` 只碰未读行）、`messages_files`；
/// - `megolm_pending(sender, session_id)` 索引；
/// - `megolm_in.seen_idx`：该入站会话已处理过的消息序号（A8：去重记录过期后重放的密文不再重新出现）；
/// - 清理旧版本处理 `from` 为空的信封后留下的坏会话（A2：`u:` / `g:` 这类无法解析的会话 id 及其消息）；
/// - 被极端 server_ts 顶到未来的会话时间拉回当前时间（A6）；
/// - 未读数改为按消息表实时统计（A10）：旧计数为 0 的会话里残留的 `read=0` 行补成已读，升级后不会凭空多出未读。
const V5_SCHEMA: &str = r#"
ALTER TABLE messages ADD COLUMN has_files INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS messages_unread ON messages(conv_id) WHERE read=0 AND outgoing=0;
CREATE INDEX IF NOT EXISTS messages_files ON messages(conv_id) WHERE has_files=1;
CREATE INDEX IF NOT EXISTS megolm_pending_key ON megolm_pending(sender, session_id);
ALTER TABLE megolm_in ADD COLUMN seen_idx TEXT NOT NULL DEFAULT '';
DELETE FROM reactions WHERE message_id IN
  (SELECT id FROM messages WHERE length(conv_id)<3 OR substr(conv_id,1,2) NOT IN ('u:','g:'));
DELETE FROM messages WHERE length(conv_id)<3 OR substr(conv_id,1,2) NOT IN ('u:','g:');
DELETE FROM drafts WHERE length(conv_id)<3 OR substr(conv_id,1,2) NOT IN ('u:','g:');
DELETE FROM conversations WHERE length(conv_id)<3 OR substr(conv_id,1,2) NOT IN ('u:','g:');
UPDATE conversations SET last_ts=CAST(strftime('%s','now') AS INTEGER)*1000
  WHERE last_ts > (CAST(strftime('%s','now') AS INTEGER)+86400)*1000;
UPDATE messages SET read=1 WHERE outgoing=0 AND read=0
  AND conv_id IN (SELECT conv_id FROM conversations WHERE unread=0);
"#;

/// 一步迁移。`Sql` 不依赖库密钥；`Data` 需要解密 / 重新加密已有数据，只在库密钥校验通过之后执行。
#[derive(Clone, Copy)]
pub(crate) enum Migration {
    Sql(&'static str),
    Data(fn(&rusqlite::Transaction, &DbCipher) -> Result<()>),
}

/// 本地库迁移：下标 + 1 即 `PRAGMA user_version`。**只允许在末尾追加，不要修改已发布的条目。**
/// - v1 是引入迁移框架前的完整表结构（全部 IF NOT EXISTS），所以旧库（user_version=0、表已存在）执行它是无害的。
/// - v2 新增 inbox 表（C-H）。
/// - v3 给旧的通话记录补上结束原因（C-B，数据迁移，需要库密钥）。
/// - v4（v3 收尾）：提及标记、草稿表、已发送消息 sort_ts 回填为 server_ts 并错开同一会话内的重复值。
/// - v5（2026-09-30 审查修复）：见 `V5_SCHEMA`。
const MIGRATIONS: &[Migration] = &[
    Migration::Sql(SCHEMA),
    Migration::Sql(V2_INBOX),
    Migration::Data(migrate_call_log_reason),
    Migration::Data(migrate_v4),
    Migration::Data(migrate_v5),
];

/// 当前库版本。
#[cfg(test)]
pub const SCHEMA_VERSION: i64 = MIGRATIONS.len() as i64;

fn user_version(conn: &Connection) -> Result<i64> {
    Ok(conn.pragma_query_value(None, "user_version", |r| r.get(0))?)
}

/// 按 user_version 依次执行尚未执行的迁移，每一步在一个事务内完成（失败则回滚、版本不变）。
/// 调用方（`Store::migrate`）保证数据迁移执行前库密钥已校验通过。
fn migrate_with(conn: &mut Connection, migrations: &[Migration], cipher: &DbCipher) -> Result<()> {
    let v = user_version(conn)?;
    if v > migrations.len() as i64 {
        return Err(HushError::storage(format!("本地数据库版本 {v} 比当前核心库支持的 {} 新，不能降级打开", migrations.len())));
    }
    for (i, m) in migrations.iter().enumerate().skip(v as usize) {
        let tx = conn.transaction()?;
        let r = match m {
            Migration::Sql(sql) => tx.execute_batch(sql).map_err(HushError::from),
            Migration::Data(f) => f(&tx, cipher),
        };
        r.map_err(|e| HushError::storage(format!("本地库迁移 v{} 失败: {e}", i + 1)))?;
        tx.pragma_update(None, "user_version", (i + 1) as i64)?;
        tx.commit()?;
    }
    Ok(())
}

/// 库密钥校验值：首次写入，之后打开时验证，防止错误密钥写乱数据。
fn check_key(conn: &Connection, cipher: &DbCipher) -> Result<()> {
    let v: Option<Vec<u8>> = conn.query_row("SELECT v FROM kv WHERE k='key_check'", [], |r| r.get(0)).optional()?;
    match v {
        Some(v) => {
            cipher.open("key_check", &v).map_err(|_| HushError::KeyMismatch { message: KEY_MISMATCH.into() })?;
        }
        None => {
            conn.execute("INSERT OR REPLACE INTO kv(k,v) VALUES('key_check',?1)", params![cipher.seal("key_check", b"hush")])?;
        }
    }
    Ok(())
}

/// 与此前 Storage 错误的文案保持一致（平台层旧版本按整句匹配）。
const KEY_MISMATCH: &str = "db_key 与已有数据库不匹配";

/// v3 数据迁移（C-B）：旧通话记录没有结束原因，按「duration>0 → Completed，missed → Missed，其余 → Cancelled」补上。
/// 正文无法解密的行保持原样（读取时同一规则兜底）。
fn migrate_call_log_reason(tx: &rusqlite::Transaction, cipher: &DbCipher) -> Result<()> {
    // 逐行解密，只把需要改写的通话记录收集起来（峰值内存与消息总量无关）
    let updates: Vec<(String, Vec<u8>)> = {
        let mut st = tx.prepare("SELECT id, body FROM messages")?;
        let mut rows = st.query([])?;
        let mut out = Vec::new();
        while let Some(r) = rows.next()? {
            let body: Vec<u8> = r.get(1)?;
            let Ok(plain) = cipher.open(LABEL_BODY, &body) else { continue };
            let Ok(mut msg) = serde_json::from_slice::<StoredMessage>(&plain) else { continue };
            if let StoredBody::CallLog { duration_secs, missed, reason: reason @ None, .. } = &mut msg.body {
                *reason = Some(legacy_call_reason(*duration_secs, *missed));
                out.push((r.get(0)?, cipher.seal(LABEL_BODY, &serde_json::to_vec(&msg)?)));
            }
        }
        out
    };
    for (id, body) in updates {
        tx.execute("UPDATE messages SET body=?2 WHERE id=?1", params![id, body])?;
    }
    Ok(())
}

/// v4：见 `V4_SCHEMA`。回填之后逐会话按 (sort_ts, rowid) 扫描，把与前一条相同（或更小）的 sort_ts 改为前一条 + 1，
/// 使同一会话内 sort_ts 唯一——`messages(before_ts)` 以 sort_ts 为游标，重复值跨页时会漏掉消息。
/// 只收集需要改的行（通常极少），不解密正文。
fn migrate_v4(tx: &rusqlite::Transaction, _cipher: &DbCipher) -> Result<()> {
    tx.execute_batch(V4_SCHEMA)?;
    let updates: Vec<(i64, i64)> = {
        let mut st = tx.prepare("SELECT rowid, conv_id, sort_ts FROM messages ORDER BY conv_id, sort_ts, rowid")?;
        let mut rows = st.query([])?;
        let mut out = Vec::new();
        let mut prev: Option<(String, i64)> = None;
        while let Some(r) = rows.next()? {
            let (rowid, conv, ts): (i64, String, i64) = (r.get(0)?, r.get(1)?, r.get(2)?);
            let ts = match &prev {
                Some((c, p)) if *c == conv && ts <= *p => {
                    out.push((rowid, p + 1));
                    p + 1
                }
                _ => ts,
            };
            prev = Some((conv, ts));
        }
        out
    };
    for (rowid, ts) in updates {
        tx.execute("UPDATE messages SET sort_ts=?2 WHERE rowid=?1", params![rowid, ts])?;
    }
    Ok(())
}

/// v5：见 `V5_SCHEMA`。建列之后逐行解密正文，给已有本地附件文件的消息回填 `has_files=1`
/// （只解析需要的字段，不为缩略图分配内存；正文无法读取的行保持 0）。
fn migrate_v5(tx: &rusqlite::Transaction, cipher: &DbCipher) -> Result<()> {
    tx.execute_batch(V5_SCHEMA)?;
    let with_files: Vec<i64> = {
        let mut st = tx.prepare("SELECT rowid, body FROM messages")?;
        let mut rows = st.query([])?;
        let mut out = Vec::new();
        while let Some(r) = rows.next()? {
            let body: Vec<u8> = r.get(1)?;
            let Ok(plain) = cipher.open(LABEL_BODY, &body) else { continue };
            if serde_json::from_slice::<SlimMessage>(&plain).is_ok_and(|m| m.has_files()) {
                out.push(r.get(0)?);
            }
        }
        out
    };
    for rowid in with_files {
        tx.execute("UPDATE messages SET has_files=1 WHERE rowid=?1", [rowid])?;
    }
    Ok(())
}

/// 只含搜索与「是否有本地文件」判断所需字段的消息正文视图：解析时跳过缩略图、密钥等大字段，不为它们分配内存。
#[derive(Deserialize)]
pub(crate) struct SlimMessage {
    pub body: SlimBody,
}

#[derive(Deserialize)]
#[serde(tag = "t", rename_all = "snake_case")]
pub(crate) enum SlimBody {
    Text {
        text: String,
    },
    Attachments {
        #[serde(default)]
        items: Vec<SlimAttachment>,
        #[serde(default)]
        caption: Option<String>,
    },
    #[serde(other)]
    Other,
}

#[derive(Deserialize)]
pub(crate) struct SlimAttachment {
    #[serde(default)]
    pub file_name: String,
    #[serde(default)]
    pub local_path: Option<String>,
}

impl SlimMessage {
    fn has_files(&self) -> bool {
        matches!(&self.body, SlimBody::Attachments { items, .. } if items.iter().any(|a| a.local_path.is_some()))
    }

    /// 本地搜索（C2）：文字消息比对正文；附件只比对说明与文件名。`q` 已转小写。
    pub fn matches(&self, q: &str) -> bool {
        let hit = |s: &str| s.to_lowercase().contains(q);
        match &self.body {
            SlimBody::Text { text } => hit(text),
            SlimBody::Attachments { items, caption } => caption.as_deref().is_some_and(hit) || items.iter().any(|a| hit(&a.file_name)),
            SlimBody::Other => false,
        }
    }
}

/// 排序 / 会话时间最多比本机当前时间晚这么多（A6）：服务端给出的时间戳不可全信，
/// 极端值（`i64::MAX`、2100 年）会让消息不可见或让会话永远置顶。
pub(crate) const MAX_FUTURE_MS: i64 = 24 * 3600 * 1000;

/// 把来自外部的时间戳钳到「当前时间 + 容差」以内；负数视为未知（0）。
pub(crate) fn clamp_ts(ts: i64) -> i64 {
    ts.clamp(0, crate::util::now_ms() + MAX_FUTURE_MS)
}

/// kv 中记录「已删除的会话曾设为免打扰」的键（K1：会话因新消息重新出现时沿用免打扰）。
pub(crate) fn muted_hint_key(conv_id: &str) -> String {
    format!("muted_hint:{conv_id}")
}

/// 没有记录结束原因的旧通话记录的推断规则（C-B）。
pub fn legacy_call_reason(duration_secs: u32, missed: bool) -> CallEndReason {
    if duration_secs > 0 {
        CallEndReason::Completed
    } else if missed {
        CallEndReason::Missed
    } else {
        CallEndReason::Cancelled
    }
}

const LABEL_BODY: &str = "messages.body";
const LABEL_DRAFT: &str = "drafts.text";
/// 每个对端保留的 Olm 会话上限。
pub const MAX_OLM_SESSIONS: i64 = 5;
const LABEL_KV: &str = "kv";

/// 本地存储的附件（含密钥、digest，仅加密后落库）。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct StoredAttachment {
    #[serde(flatten)]
    pub wire: WireAttachment,
    #[serde(default)]
    pub local_path: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "t", rename_all = "snake_case")]
pub enum StoredBody {
    Text { text: String },
    Attachments { items: Vec<StoredAttachment>, caption: Option<String> },
    System { text: String },
    Undecryptable,
    CallLog {
        video: bool,
        duration_secs: u32,
        missed: bool,
        outgoing: bool,
        /// v2 起写入；旧记录缺省为 None（迁移 v3 补齐，读取时按 `legacy_call_reason` 兜底）
        #[serde(default, skip_serializing_if = "Option::is_none")]
        reason: Option<CallEndReason>,
    },
}

impl StoredBody {
    pub fn to_api(&self) -> MessageBody {
        match self {
            StoredBody::Text { text } => MessageBody::Text { text: text.clone() },
            StoredBody::Attachments { items, caption } => MessageBody::Attachments {
                items: items
                    .iter()
                    .map(|a| AttachmentItem {
                        id: a.wire.id.clone(),
                        mime: a.wire.mime.clone(),
                        file_name: a.wire.file_name.clone(),
                        kind: a.wire.kind,
                        size: a.wire.size,
                        width: a.wire.width,
                        height: a.wire.height,
                        duration_ms: a.wire.duration_ms,
                        thumbnail: a.wire.thumbnail.as_deref().and_then(|t| crate::util::b64d(t).ok()),
                        local_path: a.local_path.clone(),
                    })
                    .collect(),
                caption: caption.clone(),
            },
            StoredBody::System { text } => MessageBody::System { text: text.clone() },
            StoredBody::Undecryptable => MessageBody::Undecryptable,
            StoredBody::CallLog { video, duration_secs, missed, outgoing, reason } => MessageBody::CallLog {
                video: *video,
                duration_secs: *duration_secs,
                missed: *missed,
                outgoing: *outgoing,
                reason: reason.unwrap_or_else(|| legacy_call_reason(*duration_secs, *missed)),
            },
        }
    }

    /// 用于会话预览、引用与本地搜索的文本。
    pub fn preview_text(&self) -> String {
        match self {
            StoredBody::Text { text } => text.clone(),
            StoredBody::Attachments { items, caption } => {
                if let Some(c) = caption.as_ref().filter(|c| !c.is_empty()) {
                    return c.clone();
                }
                match items.first().map(|a| a.wire.kind) {
                    Some(AttachmentKind::Image) => "[图片]".into(),
                    Some(AttachmentKind::Voice) => "[语音]".into(),
                    Some(AttachmentKind::Video) => "[视频]".into(),
                    Some(AttachmentKind::File) => format!("[文件] {}", items[0].wire.file_name),
                    None => String::new(),
                }
            }
            StoredBody::System { text } => text.clone(),
            StoredBody::Undecryptable => "[无法解密的消息]".into(),
            StoredBody::CallLog { video, .. } => if *video { "[视频通话]".into() } else { "[语音通话]".into() },
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct StoredMessage {
    pub body: StoredBody,
    #[serde(default)]
    pub quote: Option<Quote>,
    /// 转发的消息（v3 / K4）
    #[serde(default, skip_serializing_if = "is_false")]
    pub forwarded: bool,
    /// 文字消息的 @提及（v3 / K6，已校验）
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub mentions: Vec<Mention>,
}

fn is_false(v: &bool) -> bool {
    !*v
}

impl StoredMessage {
    /// 没有转发标记、没有提及的消息。
    pub fn plain(body: StoredBody, quote: Option<Quote>) -> Self {
        StoredMessage { body, quote, forwarded: false, mentions: vec![] }
    }

    /// 是否有本地附件文件（`messages.has_files` 列，C5）。
    pub fn has_files(&self) -> bool {
        matches!(&self.body, StoredBody::Attachments { items, .. } if items.iter().any(|a| a.local_path.is_some()))
    }

    /// 可见的聊天消息（文字 / 附件）：只有它们参与已读回执。
    pub fn is_chat(&self) -> bool {
        matches!(self.body, StoredBody::Text { .. } | StoredBody::Attachments { .. })
    }
}

/// 消息 id 冲突时使用的派生 id。收到的内容 id 限定为 `[A-Za-z0-9_-]`（见 `valid_content_id`），不含 `:`，所以无法伪造。
pub fn derived_message_id(sender_id: &str, content_id: &str) -> String {
    format!("{sender_id}:{content_id}")
}

/// 会话 id 是否合法：`u:<非空>` 或 `g:<非空>`。
pub fn valid_conv_id(conv_id: &str) -> bool {
    conv_id.len() > 2 && (conv_id.starts_with("u:") || conv_id.starts_with("g:"))
}

/// 收到的 `Content.id` 是否合法：1–128 个字符，只含字母数字、`-`、`_`（本库生成的是 UUID）。
pub fn valid_content_id(id: &str) -> bool {
    !id.is_empty() && id.len() <= 128 && id.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

#[derive(Debug, Clone)]
pub struct MsgRow {
    /// 本地消息 id（导出为 `MessageItem.id`）：通常就是 `Content.id`，冲突时为 `发送者:Content.id`
    pub id: String,
    pub conv_id: String,
    pub sender_id: String,
    pub outgoing: bool,
    pub sent_at: i64,
    pub server_ts: i64,
    pub sort_ts: i64,
    pub status: MessageStatus,
    pub msg: StoredMessage,
    pub expire_secs: u32,
    pub expires_at: Option<i64>,
    pub deleted: bool,
    pub read: bool,
    /// 收到的消息提及了我（v3 / K6）：未读提及计数与 `MessageAdded.mentions_me`
    pub mentions_me: bool,
}

impl MsgRow {
    /// 协议层（回执、回应、撤回、引用）使用的内容 id：去掉冲突时加的 `发送者:` 前缀。
    pub fn content_id(&self) -> &str {
        self.id.strip_prefix(&format!("{}:", self.sender_id)).unwrap_or(&self.id)
    }
}

#[derive(Debug, Clone)]
pub struct ConvRow {
    pub conv_id: String,
    pub last_ts: i64,
    pub unread: u32,
    pub pinned: bool,
    pub muted: bool,
    pub archived: bool,
    pub expire_secs: u32,
    /// 未读消息中提及我的条数（K6，按消息表实时统计）
    pub unread_mentions: u32,
    /// 草稿密文与更新时间（K5）
    pub draft: Option<(Vec<u8>, i64)>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct IdentityRec {
    pub user_id: String,
    pub identity_key: String,
    pub signing_key: String,
    pub device_generation: i64,
    pub verified: bool,
}

pub struct OutboundRec {
    pub session: GroupSession,
    pub created_at: i64,
    pub msg_count: u32,
    pub shared_with: HashSet<String>,
}

#[derive(Debug, Clone)]
pub struct PendingRec {
    pub envelope_id: String,
    pub group_id: String,
    pub sender: String,
    pub session_id: String,
    pub body: String,
    pub server_ts: i64,
}

pub struct Store {
    conn: Mutex<Connection>,
    pub cipher: DbCipher,
}

fn b(v: i64) -> bool {
    v != 0
}

impl Store {
    pub fn open(path: &Path, cipher: DbCipher) -> Result<Self> {
        let mut conn = Connection::open(path)?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        // B8：删除的内容（消息密文、会话 pickle、资料缓存）在页内清零，不留在空闲页里
        conn.pragma_update(None, "secure_delete", "ON")?;
        conn.busy_timeout(std::time::Duration::from_secs(5))?;
        Self::migrate(&mut conn, MIGRATIONS, &cipher)?;
        Ok(Store { conn: Mutex::new(conn), cipher })
    }

    /// 执行迁移并校验库密钥：v1（建表）不需要密钥；之后先校验密钥（不符返回 `KeyMismatch`，不会用错误的密钥改写数据），
    /// 再执行其余迁移（其中数据迁移需要解密 / 重新加密）。
    pub(crate) fn migrate(conn: &mut Connection, migrations: &[Migration], cipher: &DbCipher) -> Result<()> {
        if user_version(conn)? == 0 && !migrations.is_empty() {
            migrate_with(conn, &migrations[..1], cipher)?;
        }
        check_key(conn, cipher)?;
        migrate_with(conn, migrations, cipher)
    }

    #[cfg(test)]
    pub fn open_in_memory(cipher: DbCipher) -> Result<Self> {
        let mut conn = Connection::open_in_memory()?;
        Self::migrate(&mut conn, MIGRATIONS, &cipher)?;
        Ok(Store { conn: Mutex::new(conn), cipher })
    }

    fn conn(&self) -> std::sync::MutexGuard<'_, Connection> {
        self.conn.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// 测试用：直接执行 SQL（构造旧版本留下的数据）。
    #[cfg(test)]
    pub fn debug_exec(&self, sql: &str) {
        let c = self.conn();
        let mut st = c.prepare(sql).unwrap();
        let mut rows = st.query([]).unwrap();
        while rows.next().unwrap().is_some() {}
    }

    /// 测试用：执行返回单个整数的查询。
    #[cfg(test)]
    pub fn debug_query_i64(&self, sql: &str) -> i64 {
        self.conn().query_row(sql, [], |r| r.get(0)).unwrap()
    }

    // ---------- kv ----------
    pub fn kv_get(&self, k: &str) -> Result<Option<Vec<u8>>> {
        Ok(self.conn().query_row("SELECT v FROM kv WHERE k=?1", [k], |r| r.get(0)).optional()?)
    }
    pub fn kv_set(&self, k: &str, v: &[u8]) -> Result<()> {
        self.conn().execute("INSERT OR REPLACE INTO kv(k,v) VALUES(?1,?2)", params![k, v])?;
        Ok(())
    }
    pub fn kv_del(&self, k: &str) -> Result<()> {
        self.conn().execute("DELETE FROM kv WHERE k=?1", [k])?;
        Ok(())
    }
    pub fn kv_get_json<T: for<'a> Deserialize<'a>>(&self, k: &str) -> Result<Option<T>> {
        match self.kv_get(k)? {
            Some(v) => Ok(Some(serde_json::from_slice(&self.cipher.open(LABEL_KV, &v)?)?)),
            None => Ok(None),
        }
    }
    /// 以加密形式写入 JSON。
    pub fn kv_set_json<T: Serialize>(&self, k: &str, v: &T) -> Result<()> {
        let data = serde_json::to_vec(v)?;
        self.kv_set(k, &self.cipher.seal(LABEL_KV, &data))
    }

    /// 清空全部用户数据（换账号 / 退出登录）。
    pub fn wipe_all(&self) -> Result<()> {
        self.conn().execute_batch(
            "DELETE FROM profiles; DELETE FROM friends; DELETE FROM blocks; DELETE FROM identities;
             DELETE FROM olm_sessions; DELETE FROM megolm_out; DELETE FROM megolm_in; DELETE FROM megolm_pending;
             DELETE FROM groups; DELETE FROM conversations; DELETE FROM messages; DELETE FROM reactions;
             DELETE FROM seen; DELETE FROM inbox; DELETE FROM drafts; DELETE FROM kv WHERE k NOT IN ('key_check');",
        )?;
        Ok(())
    }

    /// 登出后收尾（B8）：把 WAL 合并回主库并截断、VACUUM 重建文件，已删除的数据不留在 WAL 与空闲页里。
    pub fn compact_after_wipe(&self) -> Result<()> {
        let c = self.conn();
        c.query_row("PRAGMA wal_checkpoint(TRUNCATE)", [], |_| Ok(()))?;
        c.execute_batch("VACUUM")?;
        c.query_row("PRAGMA wal_checkpoint(TRUNCATE)", [], |_| Ok(()))?;
        Ok(())
    }

    /// 清空加密会话状态（新设备代 / 被挤下线）。
    pub fn wipe_crypto(&self) -> Result<()> {
        self.conn().execute_batch(
            "DELETE FROM olm_sessions; DELETE FROM megolm_out; DELETE FROM megolm_in; DELETE FROM megolm_pending;
             DELETE FROM inbox WHERE plaintext IS NULL; DELETE FROM kv WHERE k='account';",
        )?;
        Ok(())
    }

}
