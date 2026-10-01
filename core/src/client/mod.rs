//! 客户端核心状态与通用工具。各业务模块以 `impl Inner` 的形式分散在子模块中。

mod account;
mod attachments;
#[cfg(test)]
mod audit_tests;
mod calls;
mod events;
mod groups;
mod hooks;
mod identity;
mod keys;
mod messaging;
#[cfg(test)]
mod perf_tests;
mod receive;
mod secure;
mod social;
mod sync;

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock, RwLock, Weak};

use serde::{Deserialize, Serialize};

use crate::api::Api;
use crate::crypto::fieldenc::DbCipher;
use crate::error::{HushError, Result};
use crate::store::{ConvRow, MsgRow, Store, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::now_ms;

pub use calls::CallTrack;
#[cfg(any(test, feature = "test-hooks"))]
pub use keys::FallbackMeta;
pub use sync::RunHandle;

/// 持久化的登录状态（加密存于 kv）。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthState {
    pub server_url: String,
    pub token: String,
    pub user_id: String,
    pub email: String,
}

/// 调试统计（测试用）。
#[derive(Default)]
pub struct Stats {
    pub olm_prekey_rx: AtomicU64,
    pub olm_normal_rx: AtomicU64,
    pub olm_prekey_tx: AtomicU64,
    pub olm_normal_tx: AtomicU64,
    pub undecryptable: AtomicU64,
    /// WS 收到超过读帧上限的帧（随后退回 HTTP 拉取）的次数
    pub ws_oversized: AtomicU64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ConvRef<'a> {
    Direct(&'a str),
    Group(&'a str),
}

pub fn parse_conv(conv_id: &str) -> Result<ConvRef<'_>> {
    if let Some(u) = conv_id.strip_prefix("u:") {
        if !u.is_empty() {
            return Ok(ConvRef::Direct(u));
        }
    }
    if let Some(g) = conv_id.strip_prefix("g:") {
        if !g.is_empty() {
            return Ok(ConvRef::Group(g));
        }
    }
    Err(HushError::invalid(format!("会话 id 无效: {conv_id}")))
}

/// 本地搜索最多返回的条数
const SEARCH_LIMIT: usize = 200;

/// kv 中记录会话阅后即焚设置最后变更时间的键。
pub(crate) fn timer_key(conv_id: &str) -> String {
    format!("timer_at:{conv_id}")
}

pub fn direct_conv(uid: &str) -> String {
    format!("u:{uid}")
}

pub fn group_conv(gid: &str) -> String {
    format!("g:{gid}")
}

/// 按键加锁：同一键（例如信封 id）的处理串行化，不同键互不影响；无人持有时自动回收。
#[derive(Default)]
pub struct KeyedLocks {
    map: Mutex<HashMap<String, Arc<tokio::sync::Mutex<()>>>>,
}

pub struct KeyedGuard<'a> {
    locks: &'a KeyedLocks,
    key: String,
    arc: Arc<tokio::sync::Mutex<()>>,
    guard: Option<tokio::sync::OwnedMutexGuard<()>>,
}

impl KeyedLocks {
    pub async fn lock(&self, key: &str) -> KeyedGuard<'_> {
        let arc = {
            let mut m = self.map.lock().unwrap_or_else(|e| e.into_inner());
            m.entry(key.to_string()).or_default().clone()
        };
        let guard = arc.clone().lock_owned().await;
        KeyedGuard { locks: self, key: key.to_string(), arc, guard: Some(guard) }
    }

    #[cfg(test)]
    pub fn len(&self) -> usize {
        self.map.lock().unwrap().len()
    }
}

impl Drop for KeyedGuard<'_> {
    fn drop(&mut self) {
        self.guard.take();
        let mut m = self.locks.map.lock().unwrap_or_else(|e| e.into_inner());
        // map 与本守卫各持一份引用；没有其他等待者时回收
        if Arc::strong_count(&self.arc) == 2 {
            m.remove(&self.key);
        }
    }
}

pub struct Inner {
    pub files_dir: PathBuf,
    pub platform: String,
    pub device_name: String,
    pub store: Store,
    pub http: reqwest::Client,
    pub auth: RwLock<Option<AuthState>>,
    /// 登录态代数：token 每变一次加 1（见 `same_session`）
    auth_gen: AtomicU64,
    pub listener: Arc<RwLock<Option<Arc<dyn EventListener>>>>,
    /// 事件队列：emit 只入队，由专门的 `hushcore-events` 线程在锁外投递给平台（合并与上限见 `events.rs`）
    events: events::EventQueue,
    /// 串行化所有 vodozemac 状态修改（账户、Olm/Megolm 会话）。持有期间不做网络请求。
    pub crypto_lock: tokio::sync::Mutex<()>,
    /// 串行化密钥上传（生成 → 落盘 → 上传 → 标记已发布），避免两次上传交错时把对方的密钥误标为已发布
    pub key_upload: tokio::sync::Mutex<()>,
    pub conn_state: Mutex<ConnectionState>,
    pub run: Mutex<Option<RunHandle>>,
    pub calls: Mutex<HashMap<String, CallTrack>>,
    pub reset_sent: Mutex<HashMap<String, i64>>,
    /// claim 被服务端拒绝（403 / 404）的用户 → (时间, 错误)：群 room_key 分发的负缓存（A11）
    pub claim_refused: Mutex<HashMap<String, (i64, HushError)>>,
    /// 待发送的送达回执（C3：攒批）
    pub receipts: Mutex<receive::ReceiptQueue>,
    /// 允许对公网地址使用明文 http（B1，默认 false）
    allow_insecure_http: AtomicBool,
    pub replenishing: AtomicBool,
    pub stats: Stats,
    /// 信封 / 事件按 id 串行处理（WS 与 sync_now 可能同时拿到同一条）
    pub processing: KeyedLocks,
    /// 按群串行化 send_group
    pub group_send: KeyedLocks,
    /// 按群串行化群信息的应用（apply_group / on_group_removed）
    pub group_apply: KeyedLocks,
    /// 朋友圈列表上一页最后一条 (created_at, id)，用于补 before_id 复合游标
    pub moment_cursors: Mutex<HashMap<String, (i64, String)>>,
    /// 未 ack 信封的重投调度：已安排一次延迟拉取时为 true
    pub redelivery_scheduled: AtomicBool,
    /// 连续重投尝试次数（决定下次延迟），处理成功后清零
    pub redelivery_attempts: std::sync::atomic::AtomicU32,
    /// 要求当前 WS 连接断开重连（服务端会重发未 ack 的数据）
    pub reconnect: tokio::sync::Notify,
    /// 指向自身的弱引用（`new_arc` 设置）：REST 请求收到 401 时由 `Api` 回调清除登录态（C-G）
    self_ref: OnceLock<Weak<Inner>>,
    /// 测试注入与调试状态（发布构建不包含）
    #[cfg(any(test, feature = "test-hooks"))]
    pub hooks: hooks::TestHooks,
}

impl Inner {
    /// 构造并记下指向自身的弱引用（401 回调需要）。
    pub fn new_arc(data_dir: String, db_key: Vec<u8>, platform: String, device_name: String) -> Result<Arc<Self>> {
        let inner = Arc::new(Self::new(data_dir, db_key, platform, device_name)?);
        let _ = inner.self_ref.set(Arc::downgrade(&inner));
        Ok(inner)
    }

    fn new(data_dir: String, db_key: Vec<u8>, platform: String, device_name: String) -> Result<Self> {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let cipher = DbCipher::new(&db_key)?;
        let data_dir = PathBuf::from(data_dir);
        std::fs::create_dir_all(&data_dir)?;
        let files_dir = data_dir.join("files");
        std::fs::create_dir_all(files_dir.join("media"))?;
        let store = Store::open(&data_dir.join("hush.sqlite"), cipher)?;
        // 同一数据目录在进程内只有一个 Inner，此时不可能有发送在进行
        match store.fail_interrupted_sends() {
            Ok(0) => {}
            Ok(n) => tracing::info!("{n} 条上次未发送完成的消息标记为发送失败"),
            Err(e) => tracing::warn!("恢复中断的发送状态失败: {e}"),
        }
        let auth: Option<AuthState> = store.kv_get_json("auth")?;
        let listener: Arc<RwLock<Option<Arc<dyn EventListener>>>> = Arc::new(RwLock::new(None));
        let events = events::EventQueue::start(listener.clone())?;
        Ok(Inner {
            files_dir,
            platform,
            device_name,
            store,
            http: crate::api::build_http(),
            auth: RwLock::new(auth),
            auth_gen: AtomicU64::new(0),
            listener,
            events,
            crypto_lock: tokio::sync::Mutex::new(()),
            key_upload: tokio::sync::Mutex::new(()),
            conn_state: Mutex::new(ConnectionState::Disconnected),
            run: Mutex::new(None),
            calls: Mutex::new(HashMap::new()),
            reset_sent: Mutex::new(HashMap::new()),
            claim_refused: Mutex::new(HashMap::new()),
            receipts: Mutex::new(receive::ReceiptQueue::default()),
            allow_insecure_http: AtomicBool::new(false),
            replenishing: AtomicBool::new(false),
            stats: Stats::default(),
            processing: KeyedLocks::default(),
            group_send: KeyedLocks::default(),
            group_apply: KeyedLocks::default(),
            moment_cursors: Mutex::new(HashMap::new()),
            redelivery_scheduled: AtomicBool::new(false),
            redelivery_attempts: std::sync::atomic::AtomicU32::new(0),
            reconnect: tokio::sync::Notify::new(),
            self_ref: OnceLock::new(),
            #[cfg(any(test, feature = "test-hooks"))]
            hooks: hooks::TestHooks::default(),
        })
    }

    // ---------- 事件 ----------
    pub fn set_listener(&self, l: Arc<dyn EventListener>) {
        *self.listener.write().unwrap_or_else(|e| e.into_inner()) = Some(l);
    }

    /// 发出事件：只入队，不在调用方线程（可能持有 crypto_lock 等内部锁）里调用平台回调。
    pub fn emit(&self, e: CoreEvent) {
        self.events.push(e);
    }

    /// 发出 `MessageAdded`。`backlog` 只对收到的消息（含「无法解密」占位）有意义：积压或 HTTP 补拉时为 true；
    /// 本地发出的消息、系统提示、通话记录一律为 false。`mentions_me` 只对收到的、提及了我的消息为 true（K6）。
    pub fn emit_added(&self, row: &MsgRow, backlog: bool) {
        let message = self.msg_item(row);
        let mentions_me = !row.outgoing && row.mentions_me;
        self.emit(CoreEvent::MessageAdded { conv_id: row.conv_id.clone(), message, backlog, mentions_me });
    }

    // ---------- 登录态 ----------
    pub fn auth(&self) -> Result<AuthState> {
        self.auth.read().unwrap_or_else(|e| e.into_inner()).clone().ok_or(HushError::NotLoggedIn)
    }

    pub fn my_id(&self) -> Result<String> {
        Ok(self.auth()?.user_id)
    }

    pub fn is_logged_in(&self) -> bool {
        self.auth.read().unwrap_or_else(|e| e.into_inner()).is_some()
    }

    /// 带当前 token 的 REST 客户端。任何请求收到 401 时（仍是同一 token 的话）清除本地登录态并发出
    /// `LoggedOut { reason: "session_expired" }`（C-G）。
    pub fn api(&self) -> Result<Api> {
        let a = self.auth()?;
        // B1：已保存的登录态若指向公网明文 http（旧版本登录的），不再带着 token 发请求
        crate::api::ensure_secure(&a.server_url, self.allow_insecure_http())?;
        let api = Api::new(self.http.clone(), a.server_url, Some(a.token.clone()));
        Ok(match self.self_ref.get().cloned() {
            Some(weak) => {
                let token = a.token;
                api.on_unauthorized(Arc::new(move || {
                    if let Some(inner) = weak.upgrade() {
                        inner.on_session_expired(&token);
                    }
                }))
            }
            None => api,
        })
    }

    /// 是否允许对公网地址使用明文 http（B1，默认不允许）。
    pub fn allow_insecure_http(&self) -> bool {
        self.allow_insecure_http.load(Ordering::Relaxed)
    }

    pub fn set_allow_insecure_http(&self, allow: bool) {
        self.allow_insecure_http.store(allow, Ordering::Relaxed);
    }

    /// 清除登录态，但只有当前 token 仍是 `token` 时才清（旧登录态的请求晚到的 401 不能登出新的登录）。返回是否清除。
    pub(crate) fn clear_auth_if(&self, token: &str) -> bool {
        let mut cur = self.auth.write().unwrap_or_else(|e| e.into_inner());
        if cur.as_ref().map(|c| c.token.as_str()) != Some(token) {
            return false;
        }
        if let Err(e) = self.store.kv_del("auth") {
            tracing::warn!("清除登录态失败: {e}");
        }
        *cur = None;
        self.auth_gen.fetch_add(1, Ordering::SeqCst);
        true
    }

    pub fn set_auth(&self, a: Option<AuthState>) -> Result<()> {
        match &a {
            Some(v) => self.store.kv_set_json("auth", v)?,
            None => self.store.kv_del("auth")?,
        }
        let mut cur = self.auth.write().unwrap_or_else(|e| e.into_inner());
        // 登录态换了（登出、换号、重新登录）：之前派生的后台任务作废
        if cur.as_ref().map(|c| &c.token) != a.as_ref().map(|c| &c.token) {
            self.auth_gen.fetch_add(1, Ordering::SeqCst);
            self.forget_claim_refusal(None);
        }
        *cur = a;
        Ok(())
    }

    /// 当前登录态的代数。后台任务派生时记下，执行前用 `same_session` 确认登录态没变。
    pub fn session_gen(&self) -> u64 {
        self.auth_gen.load(Ordering::SeqCst)
    }

    /// 仍是派生任务时的那次登录（没有登出、换号或重新登录）。
    pub fn same_session(&self, generation: u64) -> bool {
        self.is_logged_in() && self.session_gen() == generation
    }

    pub fn connection_state(&self) -> ConnectionState {
        *self.conn_state.lock().unwrap_or_else(|e| e.into_inner())
    }

    pub fn set_connection_state(&self, s: ConnectionState) {
        let changed = {
            let mut g = self.conn_state.lock().unwrap_or_else(|e| e.into_inner());
            let changed = *g != s;
            *g = s;
            changed
        };
        if changed {
            self.emit(CoreEvent::ConnectionChanged { state: s });
        }
    }

    // ---------- 设置 ----------
    /// 读取设置。记录损坏或无法解密时退回**隐私优先**的默认值（关闭已读回执与输入状态），
    /// 而不是悄悄重新打开用户关掉的回执。
    pub fn settings(&self) -> Settings {
        match self.store.kv_get_json::<Settings>("settings") {
            Ok(Some(s)) => s,
            Ok(None) => Settings::default(),
            Err(e) => {
                tracing::warn!("设置无法读取，按隐私优先处理: {e}");
                Settings { read_receipts: false, typing_indicators: false, ..Settings::default() }
            }
        }
    }

    pub fn set_settings(&self, s: &Settings) -> Result<()> {
        self.store.kv_set_json("settings", s)
    }

    // ---------- 视图转换 ----------
    pub fn msg_item(&self, r: &MsgRow) -> MessageItem {
        MessageItem {
            id: r.id.clone(),
            conv_id: r.conv_id.clone(),
            sender_id: r.sender_id.clone(),
            is_outgoing: r.outgoing,
            sent_at: r.sent_at,
            server_ts: r.server_ts,
            sort_ts: r.sort_ts,
            status: r.status,
            body: r.msg.body.to_api(),
            quote: r.msg.quote.clone(),
            reactions: self.store.reactions(&r.id).unwrap_or_default(),
            expire_secs: r.expire_secs,
            expires_at: r.expires_at,
            deleted: r.deleted,
            forwarded: r.msg.forwarded,
            mentions: r.msg.mentions.clone(),
        }
    }

    pub fn display_name(&self, uid: &str) -> String {
        match self.store.get_profile(uid).ok().flatten() {
            Some(p) if !p.name.is_empty() => p.name,
            _ => uid.chars().take(8).collect(),
        }
    }

    pub fn conv_summary(&self, row: ConvRow) -> Result<ConversationSummary> {
        let last = self.store.last_message(&row.conv_id)?.map(|m| self.msg_item(&m));
        let draft = row.draft.as_ref().and_then(|(blob, ts)| self.store.open_draft(blob).map(|t| (t, *ts)));
        // 有草稿时最近活动时间取草稿更新时间（较晚者），与列表排序一致（K5）
        let last_activity_ts = match &draft {
            Some((_, ts)) => row.last_ts.max(*ts),
            None => row.last_ts,
        };
        let mut s = ConversationSummary {
            id: row.conv_id.clone(),
            kind: ConversationKind::Direct,
            peer_user_id: None,
            group_id: None,
            title: String::new(),
            avatar_id: None,
            last_message: last,
            last_activity_ts,
            unread_count: row.unread,
            pinned: row.pinned,
            muted: row.muted,
            archived: row.archived,
            disappearing_secs: row.expire_secs,
            active: true,
            draft: draft.map(|(t, _)| t),
            unread_mentions: row.unread_mentions,
        };
        match parse_conv(&row.conv_id)? {
            ConvRef::Direct(uid) => {
                s.peer_user_id = Some(uid.to_string());
                s.title = self.display_name(uid);
                s.avatar_id = self.store.get_profile(uid)?.and_then(|p| p.avatar_id);
            }
            ConvRef::Group(gid) => {
                s.kind = ConversationKind::Group;
                s.group_id = Some(gid.to_string());
                if let Some(g) = self.store.get_group(gid)? {
                    s.title = g.name.clone();
                    s.avatar_id = g.avatar_id.clone();
                    s.active = g.active;
                } else {
                    s.title = "群聊".into();
                }
            }
        }
        Ok(s)
    }

    /// 会话列表。单个会话读不出来（A2：例如旧版本留下的坏会话 id、损坏的资料缓存）只跳过它并记日志，
    /// 不让整个列表失败。
    pub fn conversations(&self, archived: bool) -> Result<Vec<ConversationSummary>> {
        let mut out = Vec::new();
        for r in self.store.list_convs()? {
            if r.archived != archived {
                continue;
            }
            let id = r.conv_id.clone();
            match self.conv_summary(r) {
                Ok(s) => out.push(s),
                Err(e) => tracing::warn!("会话 {id} 无法读取，已从列表中跳过: {e}"),
            }
        }
        Ok(out)
    }

    pub fn conversation(&self, conv_id: &str) -> Result<Option<ConversationSummary>> {
        match self.store.get_conv(conv_id)? {
            Some(r) => Ok(Some(self.conv_summary(r)?)),
            None => Ok(None),
        }
    }

    /// 确保会话存在；新建单聊时使用默认阅后即焚设置（非 0 时记下设置时间，参与与对方的「后写者胜」同步）。
    pub fn ensure_conv(&self, conv_id: &str) -> Result<ConvRow> {
        parse_conv(conv_id)?;
        let default = self.settings().default_disappearing_secs;
        if self.store.ensure_conv(conv_id, default)? && default != 0 {
            self.store.kv_set_json(&timer_key(conv_id), &now_ms())?;
        }
        self.store.get_conv(conv_id)?.ok_or_else(|| HushError::internal("会话创建失败"))
    }

    /// 插入本地系统提示消息。
    pub fn insert_system(&self, conv_id: &str, text: &str) -> Result<MessageItem> {
        self.store.ensure_conv(conv_id, 0)?;
        let now = now_ms();
        let mut row = MsgRow {
            id: crate::util::new_id(),
            conv_id: conv_id.to_string(),
            sender_id: String::new(),
            outgoing: false,
            sent_at: now,
            server_ts: 0,
            sort_ts: now,
            status: MessageStatus::Read,
            msg: StoredMessage::plain(StoredBody::System { text: text.to_string() }, None),
            expire_secs: 0,
            expires_at: None,
            deleted: false,
            read: true,
            mentions_me: false,
        };
        self.store.insert_message(&mut row)?;
        self.store.bump_conv(conv_id, row.sort_ts, 0)?;
        let item = self.msg_item(&row);
        self.emit_added(&row, false);
        self.emit(CoreEvent::ConversationsChanged);
        Ok(item)
    }

    pub fn messages(&self, conv_id: &str, before: Option<i64>, limit: u32) -> Result<Vec<MessageItem>> {
        let limit = if limit == 0 { 50 } else { limit.min(500) };
        Ok(self.store.messages(conv_id, before, limit)?.iter().map(|m| self.msg_item(m)).collect())
    }

    pub fn search_messages(&self, query: &str) -> Result<Vec<MessageItem>> {
        let q = query.trim().to_lowercase();
        if q.is_empty() {
            return Ok(vec![]);
        }
        // C2：流式扫描，够 200 条即停；附件只比对说明与文件名（见 `Store::search_messages`）
        Ok(self.store.search_messages(&q, SEARCH_LIMIT)?.iter().map(|m| self.msg_item(m)).collect())
    }

    pub fn stat_inc(c: &AtomicU64) {
        c.fetch_add(1, Ordering::Relaxed);
    }
}

#[cfg(test)]
mod keyed_lock_tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn same_key_serialized_and_reclaimed() {
        let locks = Arc::new(KeyedLocks::default());
        let active = Arc::new(AtomicUsize::new(0));
        let max_seen = Arc::new(AtomicUsize::new(0));
        let mut tasks = Vec::new();
        for _ in 0..8 {
            let (locks, active, max_seen) = (locks.clone(), active.clone(), max_seen.clone());
            tasks.push(tokio::spawn(async move {
                let _g = locks.lock("env-1").await;
                let n = active.fetch_add(1, Ordering::SeqCst) + 1;
                max_seen.fetch_max(n, Ordering::SeqCst);
                tokio::time::sleep(std::time::Duration::from_millis(5)).await;
                active.fetch_sub(1, Ordering::SeqCst);
            }));
        }
        for t in tasks {
            t.await.unwrap();
        }
        assert_eq!(max_seen.load(Ordering::SeqCst), 1, "同一键不能并发");
        assert_eq!(locks.len(), 0, "无人持有后应回收");
        // 不同键互不阻塞
        let a = locks.lock("a").await;
        let b = tokio::time::timeout(std::time::Duration::from_millis(200), locks.lock("b")).await;
        assert!(b.is_ok());
        drop(a);
    }
}

/// 单元测试用：离线的 Inner（服务器地址不可达），已登录为 `me`。
#[cfg(test)]
pub(crate) fn test_inner() -> (Arc<Inner>, tempfile::TempDir) {
    test_inner_at("http://127.0.0.1:1")
}

/// 单元测试用：连到指定服务器地址（通常是 `stub::StubServer`）的 Inner，已登录为 `me`。
#[cfg(test)]
pub(crate) fn test_inner_at(server_url: &str) -> (Arc<Inner>, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let inner = test_inner_in(dir.path(), server_url);
    (inner, dir)
}

/// 单元测试用：在指定数据目录打开 Inner（可重复打开同一目录，模拟进程重启），已登录为 `me`。
#[cfg(test)]
pub(crate) fn test_inner_in(dir: &std::path::Path, server_url: &str) -> Arc<Inner> {
    let inner = Inner::new_arc(dir.to_string_lossy().to_string(), vec![7u8; 32], "test".into(), "t".into()).unwrap();
    inner
        .set_auth(Some(AuthState {
            server_url: server_url.into(),
            token: "t".into(),
            user_id: "me".into(),
            email: "me@x".into(),
        }))
        .unwrap();
    inner
}

/// 单元测试用：给 Inner 存入一个密钥均已发布的新账户。
#[cfg(test)]
pub(crate) fn published_account(inner: &Inner) {
    let mut acc = vodozemac::olm::Account::new();
    acc.mark_keys_as_published();
    inner.store.save_account(&acc).unwrap();
}

/// 单元测试用的极简 HTTP 服务端：每个请求交给处理函数 `(方法, 路径, 请求体) -> (状态码, 响应体)`，
/// 用来模拟 5xx / 429 / 超时前已落库等服务端行为。
#[cfg(test)]
pub(crate) mod stub {
    use std::sync::{Arc, Mutex};

    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    pub type Handler = Arc<dyn Fn(&str, &str, &[u8]) -> (u16, String) + Send + Sync>;

    const BINARY_MARK: &str = "\u{0}stub-binary:";

    /// 处理函数返回二进制响应体（例如附件密文）时用它包装：桩服务端会原样发送这些字节。
    pub fn binary(data: &[u8]) -> String {
        format!("{BINARY_MARK}{}", crate::util::b64e(data))
    }

    pub struct StubServer {
        pub url: String,
        /// 收到的请求 (方法, 路径)
        pub hits: Arc<Mutex<Vec<(String, String)>>>,
    }

    impl StubServer {
        pub fn count(&self, method: &str, path_prefix: &str) -> usize {
            self.hits.lock().unwrap().iter().filter(|(m, p)| m == method && p.starts_with(path_prefix)).count()
        }
    }

    pub async fn start(handler: Handler) -> StubServer {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let hits: Arc<Mutex<Vec<(String, String)>>> = Arc::default();
        let h2 = hits.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else { return };
                let handler = handler.clone();
                let hits = h2.clone();
                tokio::spawn(async move {
                    let mut buf = Vec::new();
                    let mut tmp = [0u8; 4096];
                    let head_end = loop {
                        let Ok(n) = sock.read(&mut tmp).await else { return };
                        if n == 0 {
                            return;
                        }
                        buf.extend_from_slice(&tmp[..n]);
                        if let Some(p) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                            break p + 4;
                        }
                    };
                    let head = String::from_utf8_lossy(&buf[..head_end]).to_string();
                    let mut first = head.lines().next().unwrap_or("").split_whitespace();
                    let method = first.next().unwrap_or("").to_string();
                    let path = first.next().unwrap_or("").to_string();
                    let len: usize = head
                        .lines()
                        .find_map(|l| {
                            let (k, v) = l.split_once(':')?;
                            k.eq_ignore_ascii_case("content-length").then(|| v.trim().parse().ok())?
                        })
                        .unwrap_or(0);
                    while buf.len() < head_end + len {
                        let Ok(n) = sock.read(&mut tmp).await else { return };
                        if n == 0 {
                            break;
                        }
                        buf.extend_from_slice(&tmp[..n]);
                    }
                    let body = buf[head_end..].to_vec();
                    hits.lock().unwrap().push((method.clone(), path.clone()));
                    // 处理函数可能故意阻塞（模拟慢服务端）：放到阻塞线程池，不能卡住运行时的工作线程（会连定时器一起卡住）
                    let (m2, p2) = (method.clone(), path.clone());
                    let Ok((status, resp)) = tokio::task::spawn_blocking(move || handler(&m2, &p2, &body)).await else { return };
                    let (ctype, bytes) = match resp.strip_prefix(BINARY_MARK) {
                        Some(b64) => ("application/octet-stream", crate::util::b64d(b64).unwrap_or_default()),
                        None => ("application/json", resp.into_bytes()),
                    };
                    let head = format!(
                        "HTTP/1.1 {status} X\r\nContent-Type: {ctype}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        bytes.len()
                    );
                    let _ = sock.write_all(head.as_bytes()).await;
                    let _ = sock.write_all(&bytes).await;
                    let _ = sock.shutdown().await;
                });
            }
        });
        StubServer { url, hits }
    }
}

#[cfg(test)]
pub(crate) fn test_group(gid: &str, members: &[&str]) -> GroupInfo {
    GroupInfo {
        id: gid.into(),
        name: "g".into(),
        members: members
            .iter()
            .map(|u| GroupMember { user_id: u.to_string(), role: GroupRole::Member, joined_at: 0 })
            .collect(),
        active: true,
        ..Default::default()
    }
}

#[cfg(test)]
mod event_tests {
    use super::*;
    use std::sync::mpsc;
    use std::time::Duration;

    struct Probe {
        tx: Mutex<mpsc::Sender<(String, String)>>,
        panic_first: AtomicBool,
        inner: Mutex<Option<std::sync::Weak<Inner>>>,
    }

    impl EventListener for Probe {
        fn on_event(&self, event: CoreEvent) {
            if self.panic_first.swap(false, Ordering::SeqCst) {
                panic!("平台回调抛异常");
            }
            // 在回调里同步等待核心的加密锁：旧实现在持锁时直接调用回调，这里会死锁
            if let CoreEvent::KeyChanged { .. } = &event {
                let inner = self.inner.lock().unwrap().as_ref().and_then(|w| w.upgrade());
                if let Some(inner) = inner {
                    let rt = tokio::runtime::Builder::new_current_thread().build().unwrap();
                    rt.block_on(async { drop(inner.crypto_lock.lock().await) });
                }
            }
            let name = std::thread::current().name().unwrap_or("").to_string();
            let _ = self.tx.lock().unwrap().send((format!("{event:?}"), name));
        }
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn events_delivered_outside_locks_and_panics_contained() {
        let (inner, _dir) = test_inner();
        let (tx, rx) = mpsc::channel();
        let probe = Arc::new(Probe { tx: Mutex::new(tx), panic_first: AtomicBool::new(true), inner: Mutex::new(None) });
        *probe.inner.lock().unwrap() = Some(Arc::downgrade(&inner));
        inner.set_listener(probe.clone());

        // 第一个事件回调 panic：不影响后续事件
        inner.emit(CoreEvent::FriendsChanged);
        inner.emit(CoreEvent::GroupsChanged);
        let (ev, thread) = rx.recv_timeout(Duration::from_secs(5)).expect("panic 之后的事件仍应投递");
        assert!(ev.contains("GroupsChanged"), "{ev}");
        assert_eq!(thread, "hushcore-events", "回调应在专门的事件线程执行");

        // 持有 crypto_lock 时发出 KeyChanged（与 key_changed 事件处理路径相同）；回调里再去拿锁不能死锁
        {
            let _g = inner.crypto_lock.lock().await;
            inner.on_identity_changed("bob", None).unwrap();
        }
        let (ev, _) = rx.recv_timeout(Duration::from_secs(5)).expect("回调在锁外执行，不应死锁");
        assert!(ev.contains("KeyChanged"), "{ev}");
    }

    struct Gate {
        release: Mutex<mpsc::Receiver<()>>,
        seen: Mutex<Vec<String>>,
    }

    impl EventListener for Gate {
        fn on_event(&self, event: CoreEvent) {
            // 第一个事件卡住，直到测试放行：模拟平台回调很慢
            let first = {
                let mut seen = self.seen.lock().unwrap();
                seen.push(format!("{event:?}").chars().take(24).collect());
                seen.len() == 1
            };
            if first {
                let _ = self.release.lock().unwrap().recv_timeout(Duration::from_secs(10));
            }
        }
    }

    /// C3：平台回调慢时，「请刷新」类事件在队列里合并；待投递事件有上限，超出时丢最早的可丢弃事件并补一个
    /// `ConversationsChanged`；不可丢弃的事件（登出、来电、实时消息）全部保留且保持顺序。
    #[tokio::test]
    async fn refresh_events_coalesced_and_queue_bounded() {
        let (inner, _dir) = test_inner();
        let (tx, rx) = mpsc::channel();
        let gate = Arc::new(Gate { release: Mutex::new(rx), seen: Mutex::new(Vec::new()) });
        inner.set_listener(gate.clone());
        inner.emit(CoreEvent::GroupsChanged); // 卡住投递线程
        tokio::time::sleep(Duration::from_millis(100)).await;

        for _ in 0..3000 {
            inner.emit(CoreEvent::ConversationsChanged);
            inner.emit(CoreEvent::FriendsChanged);
        }
        assert_eq!(inner.events.stats().0, 2, "3000 个刷新事件合并成各一个");

        let removed = |i: usize| CoreEvent::MessageRemoved { conv_id: "u:bob".into(), message_id: format!("m{i}") };
        for i in 0..events::EVENT_QUEUE_CAP + 500 {
            inner.emit(removed(i));
        }
        inner.emit(CoreEvent::LoggedOut { reason: "x".into() });
        inner.emit(CoreEvent::KeyChanged { user_id: "bob".into() });
        let (len, dropped) = inner.events.stats();
        assert!(len <= events::EVENT_QUEUE_CAP + 4, "队列有上限: {len}");
        assert!(dropped >= 498, "{dropped}");

        tx.send(()).unwrap();
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        while inner.events.stats().0 > 0 && std::time::Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
        let seen = gate.seen.lock().unwrap().clone();
        assert_eq!(seen.iter().filter(|e| e.starts_with("ConversationsChanged")).count(), 1);
        assert_eq!(seen.iter().filter(|e| e.starts_with("FriendsChanged")).count(), 1);
        // 不可丢弃的事件都在，且顺序不变
        let pos = |p: &str| seen.iter().position(|e| e.starts_with(p)).unwrap_or_else(|| panic!("缺少 {p}"));
        assert!(pos("LoggedOut") < pos("KeyChanged"));
        assert!(seen.len() <= events::EVENT_QUEUE_CAP + 8);
    }

    /// C3：送达回执攒批——同一发送者的多条消息合成一条回执内容、一次 POST；没有会话时只 claim 一次。
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn delivery_receipts_are_batched() {
        use crate::content::{Content, ContentBody};
        let mut alice = vodozemac::olm::Account::new();
        let keys = crate::crypto::olm::prepare_one_time_keys(&mut alice, 1);
        let claim = serde_json::json!({
            "user_id": "alice",
            "identity_key": crate::crypto::olm::identity_key_b64(&alice),
            "signing_key": crate::crypto::olm::signing_key_b64(&alice),
            "device_generation": 1,
            "one_time_key": {"key_id": keys[0].key_id, "key": keys[0].key, "signature": keys[0].signature},
        })
        .to_string();
        let posted = Arc::new(Mutex::new(Vec::<serde_json::Value>::new()));
        let p2 = posted.clone();
        let srv = stub::start(Arc::new(move |m, p, b| match (m, p) {
            ("POST", p) if p.starts_with("/v1/keys/claim/") => (200, claim.clone()),
            ("POST", "/v1/messages") => {
                p2.lock().unwrap().push(serde_json::from_slice(b).unwrap());
                (200, r#"{"results":[]}"#.into())
            }
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let (inner, _dir) = test_inner_at(&srv.url);
        published_account(&inner);
        for i in 0..120 {
            let mut c = Content::new(ContentBody::Text { text: format!("积压 {i}"), quote: None, mentions: vec![] }, None, 0);
            c.id = format!("m-{i}");
            let from = if i % 3 == 0 { "carol" } else { "alice" };
            inner
                .handle_incoming(secure::Incoming { from: from.into(), content: c, server_ts: 1_000 + i, via_group: None, olm_session_id: Some("s".into()), backlog: true })
                .await
                .unwrap();
        }
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while posted.lock().unwrap().is_empty() && std::time::Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        tokio::time::sleep(Duration::from_millis(600)).await;
        let posts = posted.lock().unwrap().clone();
        assert_eq!(posts.len(), 1, "120 条消息的回执应合成一次 POST");
        let msgs = posts[0]["messages"].as_array().unwrap();
        assert_eq!(msgs.len(), 2, "每个发送者一条回执内容");
        assert!(msgs.iter().all(|m| m["urgent"] == false));
        assert_eq!(srv.count("POST", "/v1/keys/claim/alice"), 1);
        assert_eq!(srv.count("POST", "/v1/keys/claim/carol"), 1);
    }
}
