//! hushcore：Hush 加密社交 App 的 Rust 客户端核心库。
//!
//! 通过 UniFFI（proc-macro）导出 Kotlin / Swift 绑定。所有业务逻辑（网络、加密、本地存储、
//! 会话/消息/群/好友/朋友圈/通话信令）都在这里实现一次，两端原生代码只负责 UI 与平台能力。

uniffi::setup_scaffolding!();

// api / ws 保持公开：集成测试（tests/e2e.rs）用到 `ws::connect` 与 `api::Envelope`。
// 其余是内部实现，不属于 Rust 公共 API，这样编译器能发现未使用的代码。
pub mod api;
mod client;
pub(crate) mod content;
pub(crate) mod crypto;
pub mod error;
pub mod logging;
pub(crate) mod store;
pub mod types;
pub(crate) mod util;
pub mod ws;

use std::future::Future;
use std::sync::{Arc, LazyLock};

pub use error::{HushError, Result};
pub use logging::{set_log_listener, LogLevel, LogListener};
pub use types::*;

use client::{direct_conv, Inner};

/// 库内全局 tokio 运行时：所有异步导出函数与后台任务都在此运行。
pub(crate) static RUNTIME: LazyLock<tokio::runtime::Runtime> = LazyLock::new(|| {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(4)
        .thread_name("hushcore")
        .enable_all()
        .build()
        .expect("创建 tokio 运行时失败")
});

/// 在全局运行时上执行 future，并在调用方（Kotlin 协程 / Swift async / 测试运行时）等待结果。
async fn run<T, F>(fut: F) -> Result<T>
where
    T: Send + 'static,
    F: Future<Output = Result<T>> + Send + 'static,
{
    RUNTIME.spawn(fut).await.map_err(|e| HushError::internal(format!("任务异常: {e}")))?
}

/// 调试统计（仅供测试：不导出到 FFI，且只在启用 `test-hooks` 特性时编译）。
#[cfg(feature = "test-hooks")]
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct DebugStats {
    pub olm_prekey_rx: u64,
    pub olm_normal_rx: u64,
    pub olm_prekey_tx: u64,
    pub olm_normal_tx: u64,
    pub undecryptable: u64,
    pub ws_oversized: u64,
}

/// 客户端入口对象。
#[derive(uniffi::Object)]
pub struct HushClient {
    inner: Arc<Inner>,
}

macro_rules! call {
    ($self:ident, |$i:ident| $body:expr) => {{
        let $i = $self.inner.clone();
        run(async move { $body }).await
    }};
}

#[uniffi::export]
impl HushClient {
    /// `data_dir`：数据目录（SQLite 与 files/ 缓存）；`db_key`：32 字节库密钥（平台安全存储）；
    /// `platform`：android / ios；`device_name`：设备名。
    #[uniffi::constructor]
    pub fn new(data_dir: String, db_key: Vec<u8>, platform: String, device_name: String) -> Result<Arc<Self>> {
        let inner = Inner::new_arc(data_dir, db_key, platform, device_name)?;
        Ok(Arc::new(HushClient { inner }))
    }

    pub fn set_listener(&self, listener: Arc<dyn EventListener>) {
        self.inner.set_listener(listener);
    }

    /// 是否允许对**公网**地址使用明文 http（2026-09-30 / B1，默认 false）。本机与内网地址（127.0.0.1、`10.0.2.2`、
    /// 192.168.x.x 等）始终允许 http，不需要这个开关；它只给「公网测试服务器还没配证书」这种调试场景用。
    /// 不持久化，需要的话每次构造后调用。
    pub fn set_allow_insecure_http(&self, allow: bool) {
        self.inner.set_allow_insecure_http(allow);
    }

    // ================= 账号 =================

    pub async fn request_code(&self, server_url: String, email: String) -> Result<()> {
        call!(self, |i| i.request_code(&server_url, &email).await)
    }

    pub async fn verify_code(&self, server_url: String, email: String, code: String) -> Result<MyAccount> {
        call!(self, |i| i.verify_code(&server_url, &email, &code).await)
    }

    pub fn is_logged_in(&self) -> bool {
        self.inner.is_logged_in()
    }

    pub fn my_account(&self) -> Option<MyAccount> {
        self.inner.my_account()
    }

    pub async fn logout(&self) -> Result<()> {
        call!(self, |i| i.logout().await)
    }

    // ================= 连接 =================

    /// 启动后台 WS 循环（自动重连、指数退避）、OTK 补充与阅后即焚定时器。
    pub fn start(&self) -> Result<()> {
        self.inner.start()
    }

    pub fn stop(&self) {
        self.inner.stop()
    }

    pub fn connection_state(&self) -> ConnectionState {
        self.inner.connection_state()
    }

    /// 立即通过 HTTP 拉取一次积压消息（前台恢复 / 推送唤醒时可调用），返回处理条数。
    pub async fn sync_now(&self) -> Result<u32> {
        call!(self, |i| i.poll_http().await.map(|n| n as u32))
    }

    // ================= 资料 =================

    pub async fn update_profile(&self, name: Option<String>, about: Option<String>, avatar_path: Option<String>) -> Result<Profile> {
        call!(self, |i| i.update_profile(name, about, avatar_path).await)
    }

    pub async fn get_user(&self, user_id: String) -> Result<Profile> {
        call!(self, |i| i.get_user(&user_id).await)
    }

    pub async fn search_user_by_email(&self, email: String) -> Result<Option<Profile>> {
        call!(self, |i| i.search_user_by_email(&email).await)
    }

    /// 本地缓存的资料（不发请求）。
    pub fn cached_profile(&self, user_id: String) -> Option<Profile> {
        self.inner.store.get_profile(&user_id).ok().flatten()
    }

    pub async fn fetch_avatar(&self, avatar_id: String) -> Result<String> {
        call!(self, |i| i.fetch_media(&avatar_id).await)
    }

    // ================= 好友 =================

    pub async fn send_friend_request(&self, user_id: String, message: String) -> Result<()> {
        call!(self, |i| i.send_friend_request(&user_id, &message).await)
    }

    pub async fn list_friend_requests(&self) -> Result<FriendRequests> {
        call!(self, |i| i.list_friend_requests().await)
    }

    pub async fn accept_friend_request(&self, id: String) -> Result<()> {
        call!(self, |i| i.accept_friend_request(&id).await)
    }

    pub async fn reject_friend_request(&self, id: String) -> Result<()> {
        call!(self, |i| i.reject_friend_request(&id).await)
    }

    pub async fn refresh_friends(&self) -> Result<Vec<Profile>> {
        call!(self, |i| i.refresh_friends().await)
    }

    pub fn friends(&self) -> Result<Vec<Profile>> {
        self.inner.friends()
    }

    pub async fn remove_friend(&self, user_id: String) -> Result<()> {
        call!(self, |i| i.remove_friend(&user_id).await)
    }

    pub async fn block(&self, user_id: String) -> Result<()> {
        call!(self, |i| i.block(&user_id).await)
    }

    pub async fn unblock(&self, user_id: String) -> Result<()> {
        call!(self, |i| i.unblock(&user_id).await)
    }

    pub fn blocked(&self) -> Result<Vec<String>> {
        self.inner.store.blocked()
    }

    /// `hush-user:v1:<user_id>`；未登录返回空串。
    pub fn my_qr_payload(&self) -> String {
        self.inner.my_id().map(|u| format!("hush-user:v1:{u}")).unwrap_or_default()
    }

    // ================= 会话与消息 =================

    pub fn conversations(&self, archived: bool) -> Result<Vec<ConversationSummary>> {
        self.inner.conversations(archived)
    }

    pub fn conversation(&self, conv_id: String) -> Result<Option<ConversationSummary>> {
        self.inner.conversation(&conv_id)
    }

    pub fn direct_conversation_id(&self, user_id: String) -> String {
        direct_conv(&user_id)
    }

    pub fn messages(&self, conv_id: String, before_ts: Option<i64>, limit: u32) -> Result<Vec<MessageItem>> {
        self.inner.messages(&conv_id, before_ts, limit)
    }

    pub fn search_messages(&self, query: String) -> Result<Vec<MessageItem>> {
        self.inner.search_messages(&query)
    }

    /// `search_messages` 的异步版本（2026-09-30 / C2）：在核心的阻塞线程池上执行，不占用调用方线程。
    /// 结果与同步版本相同（最多 200 条，从新到旧）。
    pub async fn search_messages_async(&self, query: String) -> Result<Vec<MessageItem>> {
        let i = self.inner.clone();
        RUNTIME
            .spawn_blocking(move || i.search_messages(&query))
            .await
            .map_err(|e| HushError::internal(format!("任务异常: {e}")))?
    }

    pub async fn send_text(&self, conv_id: String, text: String, quote_id: Option<String>) -> Result<MessageItem> {
        call!(self, |i| i.send_text(&conv_id, text, quote_id).await)
    }

    /// 发送带 @提及的文字（v3 / K6）。`mentions` 的 `start` / `length` 为 UTF-16 偏移；越界等不合法的项直接丢弃。
    pub async fn send_text_with_mentions(
        &self,
        conv_id: String,
        text: String,
        quote_id: Option<String>,
        mentions: Vec<Mention>,
    ) -> Result<MessageItem> {
        call!(self, |i| i.send_text_with_mentions(&conv_id, text, quote_id, mentions).await)
    }

    /// 转发文字或附件消息到多个会话（v3 / K4）。附件复用已上传的密文，不重新上传；引用、回应、@提及不转发。
    /// 按 `conv_ids`（去重后）的顺序返回新消息，单个目标失败时该条状态为 failed，不影响其他目标。
    pub async fn forward_message(&self, message_id: String, conv_ids: Vec<String>) -> Result<Vec<MessageItem>> {
        call!(self, |i| i.forward_message(&message_id, conv_ids).await)
    }

    pub async fn send_attachment(
        &self,
        conv_id: String,
        file_path: String,
        mime: String,
        kind: AttachmentKind,
        caption: Option<String>,
        meta: AttachmentMeta,
    ) -> Result<MessageItem> {
        call!(self, |i| i.send_attachment(&conv_id, &file_path, mime, kind, caption, meta).await)
    }

    /// 重发状态为 failed 的消息。
    pub async fn resend(&self, message_id: String) -> Result<MessageItem> {
        call!(self, |i| i.resend(&message_id).await)
    }

    pub async fn download_attachment(&self, message_id: String, index: u32) -> Result<String> {
        call!(self, |i| i.download_attachment(&message_id, index).await)
    }

    pub async fn react(&self, message_id: String, emoji: String, remove: bool) -> Result<()> {
        call!(self, |i| i.react(&message_id, emoji, remove).await)
    }

    pub async fn delete_for_everyone(&self, message_id: String) -> Result<()> {
        call!(self, |i| i.delete_for_everyone(&message_id).await)
    }

    pub fn delete_local(&self, message_id: String) -> Result<()> {
        self.inner.delete_local(&message_id)
    }

    pub fn clear_conversation(&self, conv_id: String) -> Result<()> {
        self.inner.clear_conversation(&conv_id)
    }

    /// 删除会话（v3 / K1）：本机的全部消息、附件缓存文件、草稿与会话行。群会话不退群，群里有新消息时重新出现。
    pub fn delete_conversation(&self, conv_id: String) -> Result<()> {
        self.inner.delete_conversation(&conv_id)
    }

    /// 保存草稿（v3 / K5，字段级加密）。`None` 或空白清除；发送消息时自动清除。
    pub fn set_draft(&self, conv_id: String, text: Option<String>) -> Result<()> {
        self.inner.set_draft(&conv_id, text)
    }

    pub async fn mark_read(&self, conv_id: String) -> Result<()> {
        call!(self, |i| i.mark_read(&conv_id).await)
    }

    pub async fn set_typing(&self, conv_id: String, typing: bool) -> Result<()> {
        call!(self, |i| i.set_typing(&conv_id, typing).await)
    }

    pub async fn set_disappearing(&self, conv_id: String, secs: u32) -> Result<()> {
        call!(self, |i| i.set_disappearing(&conv_id, secs).await)
    }

    pub fn set_pinned(&self, conv_id: String, pinned: bool) -> Result<()> {
        self.inner.set_conv_flag(&conv_id, "pinned", pinned)
    }

    pub fn set_muted(&self, conv_id: String, muted: bool) -> Result<()> {
        self.inner.set_conv_flag(&conv_id, "muted", muted)
    }

    pub fn set_archived(&self, conv_id: String, archived: bool) -> Result<()> {
        self.inner.set_conv_flag(&conv_id, "archived", archived)
    }

    // ================= 群 =================

    pub async fn create_group(&self, name: String, member_ids: Vec<String>) -> Result<GroupInfo> {
        call!(self, |i| i.create_group(&name, member_ids).await)
    }

    pub async fn refresh_groups(&self) -> Result<Vec<GroupInfo>> {
        call!(self, |i| i.refresh_groups().await)
    }

    /// 本地已知的全部群（含已退出的，`active=false`）。
    pub fn groups(&self) -> Result<Vec<GroupInfo>> {
        self.inner.groups()
    }

    pub fn group_info(&self, group_id: String) -> Result<Option<GroupInfo>> {
        self.inner.store.get_group(&group_id)
    }

    pub async fn update_group(
        &self,
        group_id: String,
        name: Option<String>,
        announcement: Option<String>,
        avatar_path: Option<String>,
    ) -> Result<GroupInfo> {
        call!(self, |i| i.update_group(&group_id, name, announcement, avatar_path).await)
    }

    pub async fn add_members(&self, group_id: String, ids: Vec<String>) -> Result<GroupInfo> {
        call!(self, |i| i.add_members(&group_id, ids).await)
    }

    pub async fn remove_member(&self, group_id: String, user_id: String) -> Result<()> {
        call!(self, |i| i.remove_member(&group_id, &user_id).await)
    }

    pub async fn leave_group(&self, group_id: String) -> Result<()> {
        call!(self, |i| i.leave_group(&group_id).await)
    }

    pub async fn set_admin(&self, group_id: String, user_id: String, admin: bool) -> Result<GroupInfo> {
        call!(self, |i| i.set_admin(&group_id, &user_id, admin).await)
    }

    // ================= 安全码 =================

    pub async fn safety_number(&self, user_id: String) -> Result<SafetyNumber> {
        call!(self, |i| i.safety_number(&user_id).await)
    }

    pub fn set_verified(&self, user_id: String, verified: bool) -> Result<()> {
        if !self.inner.store.set_verified(&user_id, verified)? {
            return Err(HushError::not_found("尚未获取对方身份密钥"));
        }
        Ok(())
    }

    /// 扫码核验；一致时自动标记为已核验。
    pub fn verify_safety_qr(&self, user_id: String, payload: String) -> Result<bool> {
        self.inner.verify_safety_qr(&user_id, &payload)
    }

    // ================= 朋友圈 =================

    pub async fn moments_feed(&self, before: Option<i64>) -> Result<Vec<Moment>> {
        call!(self, |i| i.moments_feed(before).await)
    }

    pub async fn user_moments(&self, user_id: String, before: Option<i64>) -> Result<Vec<Moment>> {
        call!(self, |i| i.user_moments(&user_id, before).await)
    }

    /// 单条动态（朋友圈通知点击进入详情）。已删除或不可见时返回 `NotFound`。
    pub async fn moment(&self, id: String) -> Result<Moment> {
        call!(self, |i| i.moment(&id).await)
    }

    pub async fn post_moment(&self, text: String, image_paths: Vec<String>, visibility: Visibility) -> Result<Moment> {
        call!(self, |i| i.post_moment(text, image_paths, visibility).await)
    }

    pub async fn delete_moment(&self, id: String) -> Result<()> {
        call!(self, |i| i.delete_moment(&id).await)
    }

    pub async fn like_moment(&self, id: String, like: bool) -> Result<()> {
        call!(self, |i| i.like_moment(&id, like).await)
    }

    pub async fn comment_moment(&self, id: String, text: String, reply_to: Option<String>) -> Result<Comment> {
        call!(self, |i| i.comment_moment(&id, &text, reply_to).await)
    }

    pub async fn delete_comment(&self, id: String) -> Result<()> {
        call!(self, |i| i.delete_comment(&id).await)
    }

    pub async fn moment_notifications(&self) -> Result<Vec<MomentNotification>> {
        call!(self, |i| i.moment_notifications(None).await)
    }

    pub async fn fetch_media(&self, attachment_id: String) -> Result<String> {
        call!(self, |i| i.fetch_media(&attachment_id).await)
    }

    // ================= 通话 =================

    pub async fn turn_credentials(&self) -> Result<TurnCredentials> {
        call!(self, |i| i.turn_credentials().await)
    }

    pub async fn send_call_signal(&self, user_id: String, signal: CallSignal) -> Result<()> {
        call!(self, |i| i.send_call_signal(&user_id, signal).await)
    }

    // ================= 设置 =================

    pub fn settings(&self) -> Settings {
        self.inner.settings()
    }

    pub fn set_settings(&self, settings: Settings) -> Result<()> {
        self.inner.set_settings(&settings)
    }
}

/// 测试辅助：不导出到 FFI，且只在启用 `test-hooks` 特性（tests/e2e.rs 自动启用）时编译，发布构建不包含。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
impl HushClient {
    pub fn debug_stats(&self) -> DebugStats {
        use std::sync::atomic::Ordering::Relaxed;
        let s = &self.inner.stats;
        DebugStats {
            olm_prekey_rx: s.olm_prekey_rx.load(Relaxed),
            olm_normal_rx: s.olm_normal_rx.load(Relaxed),
            olm_prekey_tx: s.olm_prekey_tx.load(Relaxed),
            olm_normal_tx: s.olm_normal_tx.load(Relaxed),
            undecryptable: s.undecryptable.load(Relaxed),
            ws_oversized: s.ws_oversized.load(Relaxed),
        }
    }

    pub fn debug_outbound_session_id(&self, group_id: &str) -> Option<String> {
        self.inner.store.outbound_session_id(group_id).ok().flatten()
    }

    pub fn debug_has_inbound_session(&self, sender: &str, session_id: &str) -> bool {
        self.inner.store.has_inbound(sender, session_id).unwrap_or(false)
    }

    pub fn debug_pending_count(&self) -> i64 {
        self.inner.store.pending_count().unwrap_or(0)
    }

    /// 最近处理的一个信封（JSON）。
    pub fn debug_last_envelope(&self) -> Option<api::Envelope> {
        self.inner.hooks.last_envelope.lock().unwrap().clone()
    }

    /// 模拟服务端重投一个信封（按积压处理），返回是否 ack。
    pub async fn debug_redeliver(&self, env: api::Envelope) -> bool {
        let i = self.inner.clone();
        RUNTIME.spawn(async move { i.process_envelope(&env, true).await }).await.unwrap_or(false)
    }

    /// 收件箱中尚未处理完的信封数。
    pub fn debug_inbox_count(&self) -> i64 {
        self.inner.store.inbox_count().unwrap_or(-1)
    }

    /// 让接下来 n 个信封在解密（棘轮推进、明文写入收件箱）之后、落成消息之前「崩溃」：直接返回、不 ack。
    pub fn debug_crash_after_decrypt(&self, n: u32) {
        self.inner.hooks.crash_after_decrypt.store(n, std::sync::atomic::Ordering::SeqCst);
    }

    /// 把某用户塞进群出站会话的已分发名单（模拟「删除出站会话」被并发的发送写回旧会话）。
    pub fn debug_add_shared_with(&self, group_id: &str, user_id: &str) -> bool {
        match self.inner.store.get_outbound(group_id) {
            Ok(Some(mut o)) => {
                o.shared_with.insert(user_id.to_string());
                self.inner.store.save_outbound(group_id, &o).is_ok()
            }
            _ => false,
        }
    }

    /// 经 HTTP 取回队列里的信封但不处理、不 ack（用来构造并发重投）。
    pub async fn debug_fetch_envelopes(&self) -> Vec<api::Envelope> {
        let i = self.inner.clone();
        RUNTIME
            .spawn(async move {
                let Ok(a) = i.api() else { return vec![] };
                let r: api::FetchResp = a.get("/v1/messages?limit=100").await.unwrap_or_default();
                r.messages.into_iter().filter_map(|v| serde_json::from_value(v).ok()).collect()
            })
            .await
            .unwrap_or_default()
    }

    /// 当前已发布 fallback key 的 (公钥, 创建时间, 是否已被使用)。
    pub fn debug_fallback_meta(&self) -> Option<(String, i64, bool)> {
        let m: Option<client::FallbackMeta> = self.inner.store.kv_get_json("fallback_meta").ok().flatten();
        m.map(|m| (m.public_key, m.created_at, m.used))
    }

    /// 把 fallback key 的创建时间往前拨（模拟已满 7 天）。
    pub fn debug_age_fallback(&self, ms: i64) {
        if let Ok(Some(mut m)) = self.inner.store.kv_get_json::<client::FallbackMeta>("fallback_meta") {
            m.created_at -= ms;
            let _ = self.inner.store.kv_set_json("fallback_meta", &m);
        }
    }

    /// 立即执行一次 OTK 补充 / fallback 轮换检查。
    pub async fn debug_replenish(&self) -> Result<()> {
        let i = self.inner.clone();
        run(async move { i.replenish_keys().await }).await
    }

    /// 终止后台 WS 任务（模拟任务异常退出），用来验证 start() 会重新拉起。
    pub fn debug_kill_ws_task(&self) {
        if let Some(r) = self.inner.run.lock().unwrap().as_ref() {
            r.debug_abort_ws();
        }
    }

    /// 让接下来 n 次获取对端身份直接返回网络错误（验证未 ack 信封的重投）。
    pub fn debug_fail_identity_fetch(&self, n: u32) {
        self.inner.hooks.fail_identity_fetch.store(n, std::sync::atomic::Ordering::SeqCst);
    }

    /// 让接下来 n 次 room_key 发送直接失败（验证重试）。
    pub fn debug_fail_room_key_posts(&self, n: u32) {
        self.inner.hooks.fail_room_key_posts.store(n, std::sync::atomic::Ordering::SeqCst);
    }

    /// 当前登录 token（测试用：模拟同账号的另一条连接）。
    pub fn debug_token(&self) -> Option<String> {
        self.inner.auth().ok().map(|a| a.token)
    }

    /// 调整 WS 读帧上限（下次连接生效），用来模拟服务端推来超限帧。
    pub fn debug_set_ws_max_frame(&self, bytes: usize) {
        self.inner.hooks.ws_max_frame.store(bytes, std::sync::atomic::Ordering::Relaxed);
    }

    pub fn debug_raw_message_body(&self, id: &str) -> Option<Vec<u8>> {
        self.inner.store.raw_message_body(id).ok().flatten()
    }
}
