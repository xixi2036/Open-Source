//! 后台任务：WebSocket 收发循环（自动重连、指数退避）、服务端事件、阅后即焚扫描。

use std::sync::Arc;
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde_json::{json, Value};
use tokio::sync::watch;
use tokio::task::JoinHandle;
use tokio_tungstenite::tungstenite::Message;

use super::secure::EnvelopeOutcome;
use super::Inner;
use crate::api::{Envelope, FetchResp};
use crate::error::{HushError, Result};
use crate::types::*;
use crate::ws::{self, ConnectError};

/// 心跳需短于常见 NAT 空闲回收时间（Android 模拟器约 20 秒）
const PING_INTERVAL: Duration = Duration::from_secs(10);
const IDLE_TIMEOUT: Duration = Duration::from_secs(35);
const BACKOFF_MIN_MS: u64 = 1_000;
const BACKOFF_MAX_MS: u64 = 30_000;
/// 被同账号新连接替换（4000）后，等待这么久再重试一次
const REPLACED_RETRY_MS: u64 = 3_000;
/// 处理失败（网络类）的信封/事件：首次延迟这么久后经 HTTP 重新拉取，之后逐次翻倍，最长 60 秒
const REDELIVERY_BASE_MS: u64 = 2_000;
const REDELIVERY_MAX_MS: u64 = 60_000;
/// HTTP 拉取每页条数
const POLL_LIMIT: usize = 100;
/// 去重记录保留时长
const SEEN_RETENTION_MS: i64 = 30 * 24 * 3600 * 1000;
/// 扫描器每隔这么多秒做一次清理（去重记录、过期通话跟踪）
const SEEN_PRUNE_EVERY_TICKS: u64 = 3600;

pub struct RunHandle {
    stop: watch::Sender<bool>,
    tasks: Vec<JoinHandle<()>>,
}

#[cfg(feature = "test-hooks")]
impl RunHandle {
    pub fn debug_abort_ws(&self) {
        if let Some(t) = self.tasks.first() {
            t.abort();
        }
    }
}

/// 一次 WS 连接结束的原因。
enum ConnEnd {
    /// 本地要求停止
    Stopped,
    /// 服务端通知登出（4001 / logged_out 帧）
    LoggedOut(String),
    /// 4000：同一账号建立了新连接，本连接被替换。不是登出，不清数据
    Replaced,
    /// 连接断开（网络、空闲超时、服务端正常关闭等），按退避重连
    Dropped,
    /// 收到超过读帧上限的帧：连接已不可用，且重连后服务端会重发同一帧，需要先经 HTTP 拉取处理掉
    Oversized,
}

/// 帧处理结果。
enum FrameAction {
    Ack(Vec<String>),
    None,
    LoggedOut(String),
    /// 积压推送完毕，之后是实时推送
    QueueEmpty,
}

/// 解析一个信封。格式错误时返回 `Err(Some(id))`：无法处理但能取到 id，应直接 ack（避免每次重连都重投）；
/// 取不到 id 时返回 `Err(None)`，只能忽略。
fn parse_envelope(raw: &Value) -> std::result::Result<Envelope, Option<String>> {
    serde_json::from_value(raw.clone()).map_err(|e| match raw.get("id").and_then(|i| i.as_str()) {
        Some(id) => {
            tracing::warn!("信封 {id} 格式错误，已确认丢弃: {e}");
            Some(id.to_string())
        }
        None => {
            tracing::warn!("信封格式错误且缺少 id，忽略: {e}");
            None
        }
    })
}

fn jitter(ms: u64) -> u64 {
    use rand::Rng;
    let j = rand::thread_rng().gen_range(0..=ms / 4);
    ms - ms / 8 + j
}

impl Inner {
    pub(crate) fn start(self: &Arc<Self>) -> Result<()> {
        if !self.is_logged_in() {
            return Err(HushError::NotLoggedIn);
        }
        let mut run = self.run.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(r) = run.take() {
            // 只有 WS 任务（tasks[0]）仍在运行才算已启动；它已退出（被挤下线后停止重连、异常退出等）时整体重启
            if r.tasks.first().map(|t| !t.is_finished()).unwrap_or(false) {
                *run = Some(r);
                return Ok(());
            }
            let _ = r.stop.send(true);
            for t in r.tasks {
                t.abort();
            }
        }
        let (tx, rx) = watch::channel(false);
        let me = self.clone();
        let rx1 = rx.clone();
        let ws_task = crate::RUNTIME.spawn(async move { me.ws_loop(rx1).await });
        let me = self.clone();
        let sweep_task = crate::RUNTIME.spawn(async move { me.sweeper(rx).await });
        *run = Some(RunHandle { stop: tx, tasks: vec![ws_task, sweep_task] });
        Ok(())
    }

    /// 通知后台任务退出（不等待）。
    pub(crate) fn signal_stop(&self) {
        if let Some(r) = self.run.lock().unwrap_or_else(|e| e.into_inner()).as_ref() {
            let _ = r.stop.send(true);
        }
    }

    pub(crate) fn stop(&self) {
        let h = self.run.lock().unwrap_or_else(|e| e.into_inner()).take();
        if let Some(h) = h {
            let _ = h.stop.send(true);
            for t in h.tasks {
                t.abort();
            }
        }
        self.set_connection_state(ConnectionState::Disconnected);
    }

    async fn sweeper(self: Arc<Self>, mut stop: watch::Receiver<bool>) {
        let mut tick = tokio::time::interval(Duration::from_secs(1));
        let mut n: u64 = 0;
        loop {
            tokio::select! {
                _ = stop.changed() => return,
                _ = tick.tick() => {
                    // 启动时先清理一次，之后每小时一次（移动端进程很少连续存活一小时）
                    if n.is_multiple_of(SEEN_PRUNE_EVERY_TICKS) {
                        self.periodic_cleanup();
                    }
                    n += 1;
                    if let Err(e) = self.sweep_expired() {
                        tracing::warn!("阅后即焚扫描失败: {e}");
                    }
                }
            }
        }
    }

    /// 定期清理：过期的去重记录与长时间没有结束信令的通话跟踪。
    pub(crate) fn periodic_cleanup(&self) {
        if let Err(e) = self.store.prune_seen(crate::util::now_ms() - SEEN_RETENTION_MS) {
            tracing::warn!("清理去重记录失败: {e}");
        }
        self.prune_stale_calls(crate::util::now_ms());
        self.prune_upload_temp_files(Duration::from_secs(3600));
    }

    /// 删除上传 / 下载途中进程被杀而遗留的临时文件：媒体上传的 `files/media/.upload-*`，
    /// 以及聊天附件加解密用的 `files/.enc-*`、`files/.dl-*`、`files/.dec-*`。
    pub(crate) fn prune_upload_temp_files(&self, older_than: Duration) {
        let dirs = [(self.files_dir.join("media"), &[".upload-"][..]), (self.files_dir.clone(), &super::attachments::TEMP_PREFIXES[..])];
        for (dir, prefixes) in dirs {
            let Ok(rd) = std::fs::read_dir(dir) else { continue };
            for e in rd.flatten() {
                let name = e.file_name().to_string_lossy().to_string();
                if !prefixes.iter().any(|p| name.starts_with(p)) {
                    continue;
                }
                let stale = e.metadata().and_then(|m| m.modified()).ok().and_then(|t| t.elapsed().ok()).is_some_and(|age| age >= older_than);
                if stale {
                    let _ = std::fs::remove_file(e.path());
                }
            }
        }
    }

    async fn ws_loop(self: Arc<Self>, mut stop: watch::Receiver<bool>) {
        // 先处理上次进程残留在收件箱里的信封（C-H）：其中已解密的直接用保存的明文，不再解密
        self.process_inbox_residue().await;
        let mut backoff = BACKOFF_MIN_MS;
        let mut failures = 0u32;
        let mut replaced = 0u32;
        loop {
            let mut wait_override = None;
            if *stop.borrow() {
                break;
            }
            let Ok(auth) = self.auth() else { break };
            self.set_connection_state(ConnectionState::Connecting);
            let max_frame = self.ws_frame_limit();
            match ws::connect_with_limit(&auth.server_url, &auth.token, max_frame).await {
                Ok(stream) => {
                    self.set_connection_state(ConnectionState::Connected);
                    backoff = BACKOFF_MIN_MS;
                    failures = 0;
                    self.spawn_initial_sync();
                    let end = self.run_connection(stream, &mut stop).await;
                    if !matches!(end, ConnEnd::Replaced) {
                        replaced = 0;
                    }
                    match end {
                        ConnEnd::Stopped => break,
                        ConnEnd::Replaced => {
                            // 4000 只说明同一账号有了更新的连接（可能是本进程的旧连接竞态，也可能是另一个进程）。
                            // 不清密钥、不发 LoggedOut；退避后重试一次，若再次被替换就停止重连，等平台调用 start()。
                            replaced += 1;
                            if replaced >= 2 {
                                tracing::warn!("连续两次被同账号的新连接替换（4000），停止重连");
                                break;
                            }
                            tracing::warn!("连接被同账号的新连接替换（4000），稍后重试一次");
                            wait_override = Some(jitter(REPLACED_RETRY_MS));
                        }
                        ConnEnd::LoggedOut(reason) => {
                            self.on_logged_out(&reason);
                            return;
                        }
                        ConnEnd::Dropped => {}
                        ConnEnd::Oversized => {
                            // 超限帧会在每次重连时被服务端重发，WS 永远越不过它：改走 HTTP 拉取（逐条解析、处理并 ack）
                            Self::stat_inc(&self.stats.ws_oversized);
                            self.set_connection_state(ConnectionState::Disconnected);
                            match self.drain_http().await {
                                Ok(n) => tracing::info!("超限帧后经 HTTP 处理 {n} 条"),
                                Err(e) => tracing::warn!("超限帧后 HTTP 拉取失败: {e}"),
                            }
                        }
                    }
                }
                Err(ConnectError::Unauthorized) => {
                    // 握手 401：token 已失效（C-G）
                    self.on_session_expired(&auth.token);
                    if !self.is_logged_in() {
                        return;
                    }
                    // token 已换（期间重新登录）：用新 token 重连
                }
                Err(ConnectError::Other(e)) => {
                    failures += 1;
                    tracing::warn!("WebSocket 连接失败: {e}");
                    // 连续失败时用 HTTP 拉取兜底
                    if failures >= 3 {
                        if let Err(e) = self.poll_http().await {
                            tracing::debug!("HTTP 拉取失败: {e}");
                        }
                    }
                }
            }
            if *stop.borrow() {
                break;
            }
            self.set_connection_state(ConnectionState::Disconnected);
            let wait = match wait_override {
                Some(w) => w,
                None => {
                    let w = jitter(backoff);
                    backoff = (backoff * 2).min(BACKOFF_MAX_MS);
                    w
                }
            };
            tokio::select! {
                _ = stop.changed() => break,
                _ = tokio::time::sleep(Duration::from_millis(wait)) => {}
            }
        }
        self.set_connection_state(ConnectionState::Disconnected);
    }

    /// 单次连接的收发循环。
    async fn run_connection(self: &Arc<Self>, stream: ws::WsStream, stop: &mut watch::Receiver<bool>) -> ConnEnd {
        let (mut sink, mut source) = stream.split();
        let mut ping = tokio::time::interval(PING_INTERVAL);
        ping.tick().await;
        let mut last_rx = tokio::time::Instant::now();
        // 连接建立后、收到 queue_empty 之前推送的都是积压（C-F）
        let mut backlog = true;
        loop {
            tokio::select! {
                _ = stop.changed() => {
                    let _ = sink.send(Message::Close(None)).await;
                    return ConnEnd::Stopped;
                }
                _ = self.reconnect.notified() => {
                    tracing::info!("主动重连以重新接收未确认的数据");
                    let _ = sink.send(Message::Close(None)).await;
                    return ConnEnd::Dropped;
                }
                _ = ping.tick() => {
                    if last_rx.elapsed() > IDLE_TIMEOUT {
                        tracing::warn!("WebSocket 空闲超时，重连");
                        return ConnEnd::Dropped;
                    }
                    if sink.send(Message::Text(r#"{"t":"ping"}"#.into())).await.is_err() {
                        return ConnEnd::Dropped;
                    }
                }
                msg = source.next() => {
                    last_rx = tokio::time::Instant::now();
                    match msg {
                        Some(Ok(Message::Text(t))) => {
                            match self.handle_frame(t.as_str(), backlog).await {
                                FrameAction::QueueEmpty => backlog = false,
                                FrameAction::Ack(ids) if !ids.is_empty() => {
                                    let frame = json!({"t": "ack", "ids": ids}).to_string();
                                    if sink.send(Message::Text(frame.into())).await.is_err() {
                                        return ConnEnd::Dropped;
                                    }
                                }
                                FrameAction::LoggedOut(r) => return ConnEnd::LoggedOut(r),
                                _ => {}
                            }
                        }
                        Some(Ok(Message::Ping(p))) => {
                            let _ = sink.send(Message::Pong(p)).await;
                        }
                        Some(Ok(Message::Close(frame))) => {
                            if let Some(f) = frame {
                                match u16::from(f.code) {
                                    // 4001：在别处登录 / token 作废；4000：同一账号建立了新连接（被挤下线）
                                    4001 => return ConnEnd::LoggedOut("logged_in_elsewhere".into()),
                                    4000 => return ConnEnd::Replaced,
                                    _ => {}
                                }
                            }
                            return ConnEnd::Dropped;
                        }
                        Some(Ok(_)) => {}
                        Some(Err(tokio_tungstenite::tungstenite::Error::Capacity(e))) => {
                            tracing::warn!("服务端帧超过读取上限（{e}），改用 HTTP 拉取");
                            return ConnEnd::Oversized;
                        }
                        Some(Err(e)) => {
                            tracing::warn!("WebSocket 读取错误: {e}");
                            return ConnEnd::Dropped;
                        }
                        None => return ConnEnd::Dropped,
                    }
                }
            }
        }
    }

    async fn handle_frame(self: &Arc<Self>, text: &str, backlog: bool) -> FrameAction {
        let v: Value = match serde_json::from_str(text) {
            Ok(v) => v,
            Err(e) => {
                tracing::warn!("无法解析服务端帧: {e}");
                return FrameAction::None;
            }
        };
        match v.get("t").and_then(|t| t.as_str()).unwrap_or("") {
            "envelope" => {
                let Some(d) = v.get("d") else { return FrameAction::None };
                let env = match parse_envelope(d) {
                    Ok(e) => e,
                    Err(ack) => return ack.map(|id| FrameAction::Ack(vec![id])).unwrap_or(FrameAction::None),
                };
                if self.process_envelope(&env, backlog).await {
                    FrameAction::Ack(vec![env.id])
                } else {
                    FrameAction::None
                }
            }
            "event" => {
                let Some(d) = v.get("d") else { return FrameAction::None };
                if d.get("type").and_then(|t| t.as_str()) == Some("logged_out") {
                    let reason = d.get("reason").and_then(|r| r.as_str()).unwrap_or("logged_out").to_string();
                    return FrameAction::LoggedOut(reason);
                }
                let id = d.get("id").and_then(|i| i.as_str()).map(|s| s.to_string());
                match self.process_event(d).await {
                    Ok(()) => FrameAction::Ack(id.into_iter().collect()),
                    Err(e) if e.is_transient() => FrameAction::None,
                    Err(e) => {
                        tracing::warn!("处理服务端事件失败: {e}");
                        FrameAction::Ack(id.into_iter().collect())
                    }
                }
            }
            "queue_empty" => {
                self.spawn_replenish();
                FrameAction::QueueEmpty
            }
            _ => FrameAction::None,
        }
    }

    /// 处理信封，返回是否应 ack（网络类临时错误不 ack，等待重投）。`backlog` 见 `CoreEvent::MessageAdded`。
    ///
    /// 收件箱（C-H）：信封先原样落库；解密时推进后的棘轮状态与明文在同一事务写入该行；处理完成后
    /// 在同一事务内记 seen 并删除该行。进程在解密之后、处理完成之前被杀时，重启后（`start()` 先处理残留行）
    /// 或服务端重投时直接用保存的明文继续，不会再次解密（棘轮已推进，再解必然失败）。
    pub(crate) async fn process_envelope(self: &Arc<Self>, env: &Envelope, backlog: bool) -> bool {
        // 信封级去重：至少一次投递下同一信封可能重投，Olm 棘轮不能重复解密，必须在解密前拦截。
        // WS 与 sync_now 可能同时拿到同一信封：按 id 加锁，使「查 seen → 解密处理 → 记 seen」成为一个临界区，
        // 后到者等前者处理完，看到 seen 后直接 ack。
        let seen_key = format!("env:{}", env.id);
        let _guard = self.processing.lock(&seen_key).await;
        match self.store.is_seen(&seen_key) {
            Ok(true) => {
                let _ = self.store.inbox_delete(&env.id);
                return true;
            }
            Ok(false) => {}
            Err(e) => {
                tracing::warn!("去重检查失败: {e}");
                return false;
            }
        }
        // 信封先落库（已在收件箱中则保留原行及其明文）
        if let Err(e) = self.store.inbox_put(env) {
            tracing::warn!("信封写入收件箱失败，稍后重试: {e}");
            return false;
        }
        self.hook_record_envelope(env);
        let r: Result<bool> = async {
            let outcome = match self.store.inbox_plaintext(&env.id)? {
                Some(p) => {
                    tracing::info!("信封 {} 已解密过，使用收件箱中保存的明文继续处理", env.id);
                    self.outcome_from_inbox(env, p, backlog)?
                }
                None => {
                    let o = self.decrypt_envelope(env, backlog).await?;
                    if matches!(o, EnvelopeOutcome::Decrypted(_)) && self.hook_crash_after_decrypt() {
                        // 测试注入：模拟进程在解密（棘轮与明文已落盘）之后、处理之前被杀
                        return Ok(false);
                    }
                    o
                }
            };
            if let EnvelopeOutcome::Decrypted(inc) = outcome {
                self.handle_incoming(*inc).await?;
            }
            Ok(true)
        }
        .await;
        let ack = match r {
            Ok(false) => return false,
            Ok(true) => true,
            Err(e) if e.is_transient() => {
                // 网络、服务端 5xx / 429 等临时错误：不 ack、不记 seen，稍后重投时重试（已解密的明文留在收件箱）
                tracing::warn!("处理信封暂时失败，稍后重试: {e}");
                if !matches!(e, HushError::NotLoggedIn) {
                    self.schedule_redelivery();
                }
                false
            }
            Err(e) => {
                tracing::warn!("处理信封失败（已确认丢弃）: {e}");
                true
            }
        };
        if ack {
            if let Err(e) = self.store.finish_envelope(&seen_key, &env.id) {
                tracing::warn!("记录信封已处理失败: {e}");
            }
            self.redelivery_attempts.store(0, std::sync::atomic::Ordering::SeqCst);
        }
        ack
    }

    /// 处理上次进程残留在收件箱里的信封（按积压处理）。处理成功的记 seen，服务端重投时直接 ack；
    /// 临时失败的留在收件箱，等服务端重投或下次启动。
    pub(crate) async fn process_inbox_residue(self: &Arc<Self>) -> usize {
        let envs = match self.store.inbox_list() {
            Ok(v) => v,
            Err(e) => {
                tracing::warn!("读取收件箱失败: {e}");
                return 0;
            }
        };
        if envs.is_empty() {
            return 0;
        }
        tracing::info!("处理收件箱中残留的 {} 个信封", envs.len());
        let mut done = 0;
        for env in &envs {
            if !self.is_logged_in() {
                break;
            }
            if self.process_envelope(env, true).await {
                done += 1;
            }
        }
        done
    }

    /// 有信封/事件因网络类错误没 ack：服务端只会在重连后重发，所以短暂延迟后主动经 HTTP 拉取一次；
    /// HTTP 也失败则要求 WS 重连。多次失败时延迟逐次翻倍（2s → 60s），避免紧密循环。
    pub(crate) fn schedule_redelivery(self: &Arc<Self>) {
        use std::sync::atomic::Ordering;
        if self.redelivery_scheduled.swap(true, Ordering::SeqCst) {
            return;
        }
        let n = self.redelivery_attempts.fetch_add(1, Ordering::SeqCst).min(5);
        let delay = (REDELIVERY_BASE_MS << n).min(REDELIVERY_MAX_MS);
        let me = self.clone();
        let generation = self.session_gen();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(delay)).await;
            me.redelivery_scheduled.store(false, Ordering::SeqCst);
            if !me.same_session(generation) {
                return;
            }
            match me.poll_http().await {
                Ok(k) => tracing::info!("重投拉取处理 {k} 条"),
                Err(e) => {
                    tracing::warn!("重投拉取失败，要求 WS 重连: {e}");
                    // notify_waiters 不留存许可：此时若没有活动连接（正在退避），下一条连接本来就会收到重发，
                    // 用 notify_one 会让下一条健康连接一建立就被关掉
                    me.reconnect.notify_waiters();
                }
            }
        });
    }

    /// 处理服务端事件（按事件 id 去重并串行化，重投或 WS/HTTP 同时拿到时只处理一次）。
    async fn process_event(self: &Arc<Self>, d: &Value) -> Result<()> {
        let Some(id) = d.get("id").and_then(|i| i.as_str()).filter(|s| !s.is_empty()) else {
            return self.process_event_inner(d).await;
        };
        let key = format!("evt:{id}");
        let _guard = self.processing.lock(&key).await;
        if self.store.is_seen(&key)? {
            return Ok(());
        }
        let r = self.process_event_inner(d).await;
        // 临时错误（网络、服务端 5xx / 429）不记 seen，稍后重新拉取
        if matches!(&r, Err(e) if e.is_transient()) {
            self.schedule_redelivery();
        } else {
            let _ = self.store.mark_seen(&key);
        }
        r
    }

    async fn process_event_inner(self: &Arc<Self>, d: &Value) -> Result<()> {
        let typ = d.get("type").and_then(|t| t.as_str()).unwrap_or("");
        let str_field = |k: &str| d.get(k).and_then(|v| v.as_str()).unwrap_or("").to_string();
        match typ {
            "friend_request" => {
                let req: FriendRequest = serde_json::from_value(d.get("request").cloned().unwrap_or_default())?;
                self.store.put_profile(&req.from)?;
                self.emit(CoreEvent::FriendRequestReceived { request: req });
            }
            "friend_accepted" => {
                let uid = str_field("user_id");
                self.store.add_friend(&uid)?;
                if self.store.get_profile(&uid)?.is_none() {
                    let _ = self.get_user(&uid).await;
                }
                if let Err(e) = self.refresh_friends().await {
                    tracing::warn!("刷新好友失败: {e}");
                    self.emit(CoreEvent::FriendsChanged);
                }
            }
            "friend_removed" => {
                self.store.remove_friend(&str_field("user_id"))?;
                self.emit(CoreEvent::FriendsChanged);
            }
            "profile_updated" => {
                let p: Profile = serde_json::from_value(d.get("profile").cloned().unwrap_or_default())?;
                self.store.put_profile(&p)?;
                self.emit(CoreEvent::FriendsChanged);
                self.emit(CoreEvent::ConversationsChanged);
            }
            "group_updated" => {
                let g: GroupInfo = serde_json::from_value(d.get("group").cloned().unwrap_or_default())?;
                self.apply_group(g).await?;
            }
            "group_removed" => self.on_group_removed(&str_field("group_id")).await?,
            "key_changed" => {
                let uid = str_field("user_id");
                let _g = self.crypto_lock.lock().await;
                self.on_identity_changed(&uid, None)?;
            }
            "moment_notification" => {
                let n: MomentNotification = serde_json::from_value(d.get("notification").cloned().unwrap_or_default())?;
                self.store.put_profile(&n.from)?;
                self.emit(CoreEvent::MomentNotification { notification: n });
            }
            other => tracing::debug!("忽略未知事件 {other}"),
        }
        Ok(())
    }

    fn spawn_initial_sync(self: &Arc<Self>) {
        let me = self.clone();
        let generation = self.session_gen();
        tokio::spawn(async move {
            // 每一步之前确认仍是同一次登录：登出 / 换号后不再把旧账号的数据写进刚清空的库
            if !me.same_session(generation) {
                return;
            }
            me.spawn_replenish();
            if let Err(e) = me.refresh_friends().await {
                tracing::warn!("同步好友失败: {e}");
            }
            if !me.same_session(generation) {
                return;
            }
            if let Err(e) = me.refresh_groups().await {
                tracing::warn!("同步群组失败: {e}");
            }
            if me.same_session(generation) {
                let _ = me.refresh_blocks().await;
            }
        });
    }

    /// HTTP 轮询兜底：GET /v1/messages → 处理 → POST ack。返回确认条数。
    pub(crate) async fn poll_http(self: &Arc<Self>) -> Result<usize> {
        Ok(self.poll_http_page().await?.1)
    }

    /// 连续拉取直到拿到空页（或本页没有任何进展），最多 20 页。
    /// 服务端每页还受 8MB 负载上限约束，条数不足 `POLL_LIMIT` 的短页并不代表队列已空。
    pub(crate) async fn drain_http(self: &Arc<Self>) -> Result<usize> {
        let mut total = 0;
        for _ in 0..20 {
            let (fetched, acked) = self.poll_http_page().await?;
            total += acked;
            if fetched == 0 || acked == 0 {
                break;
            }
        }
        Ok(total)
    }

    /// 拉取一页，返回 (拉到的条数, 确认的条数)。信封逐条解析：格式错误但带 id 的直接 ack，不影响同页其他数据。
    async fn poll_http_page(self: &Arc<Self>) -> Result<(usize, usize)> {
        let api = self.api()?;
        let r: FetchResp = api.get(&format!("/v1/messages?limit={POLL_LIMIT}")).await?;
        let fetched = r.events.len() + r.messages.len();
        let mut acks = Vec::new();
        for ev in &r.events {
            if ev.get("type").and_then(|t| t.as_str()) == Some("logged_out") {
                continue;
            }
            let ok = !matches!(self.process_event(ev).await, Err(e) if e.is_transient());
            if let (true, Some(id)) = (ok, ev.get("id").and_then(|i| i.as_str())) {
                acks.push(id.to_string());
            }
        }
        for raw in &r.messages {
            let env = match parse_envelope(raw) {
                Ok(e) => e,
                Err(ack) => {
                    acks.extend(ack);
                    continue;
                }
            };
            // HTTP 补拉到的都按积压处理（C-F）
            if self.process_envelope(&env, true).await {
                acks.push(env.id.clone());
            }
        }
        let n = acks.len();
        if !acks.is_empty() {
            let _: crate::api::Empty = api.post("/v1/messages/ack", &json!({ "ids": acks })).await?;
        }
        Ok((fetched, n))
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicU16, Ordering};
    use std::sync::Arc;

    use vodozemac::olm::Account;

    use super::*;
    use crate::client::{stub, test_inner_at, test_inner_in};
    use crate::content::{Content, ContentBody};
    use crate::crypto::olm;
    use crate::store::StoredBody;
    use crate::util::b64e;

    /// K2：先缓存带邮箱的好友资料（`/v1/users/{id}`）→ 邮箱搜索同一人（结果不带邮箱）→ 缓存与返回值里邮箱仍在；
    /// `profile_updated` 事件、好友申请来源也不能改写邮箱；只有 `/v1/friends` 与删好友能改。
    #[tokio::test]
    async fn email_cache_survives_non_authoritative_sources() {
        let srv = stub::start(std::sync::Arc::new(|m, p, _b| match (m, p) {
            ("GET", "/v1/users/bob") => (200, r#"{"id":"bob","name":"Bob","about":"","avatar_id":"","updated_at":1,"email":"bob@x"}"#.into()),
            ("GET", p) if p.starts_with("/v1/users/search") => (200, r#"{"id":"bob","name":"Bob 改名","about":"","avatar_id":"","updated_at":2}"#.into()),
            ("GET", "/v1/friends") => (200, r#"[]"#.into()),
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let (inner, _dir) = test_inner_at(&srv.url);
        let email = |i: &Inner| i.store.get_profile("bob").unwrap().unwrap().email;
        inner.get_user("bob").await.unwrap();
        assert_eq!(email(&inner).as_deref(), Some("bob@x"));
        let found = inner.search_user_by_email("bob@x").await.unwrap().unwrap();
        assert_eq!(found.email.as_deref(), Some("bob@x"), "返回值带上缓存的邮箱");
        assert_eq!(found.name, "Bob 改名", "其余字段以搜索结果为准");
        assert_eq!(email(&inner).as_deref(), Some("bob@x"), "邮箱搜索不能清掉缓存的邮箱");
        // profile_updated 事件（不带邮箱 / 带了别的邮箱）
        for ev in [
            serde_json::json!({"type":"profile_updated","profile":{"id":"bob","name":"B2","updated_at":3}}),
            serde_json::json!({"type":"profile_updated","profile":{"id":"bob","name":"B3","updated_at":4,"email":"evil@x"}}),
            serde_json::json!({"type":"friend_request","request":{"id":"r","from":{"id":"bob","name":"B4"},"to":{"id":"me"},"status":"pending"}}),
        ] {
            inner.process_event(&ev).await.unwrap();
            assert_eq!(email(&inner).as_deref(), Some("bob@x"), "{ev}");
        }
        assert_eq!(inner.store.get_profile("bob").unwrap().unwrap().name, "B4");
        // 权威来源：好友列表里没有 bob（不再是好友）→ 清掉
        inner.store.add_friend("bob").unwrap();
        inner.refresh_friends().await.unwrap();
        assert_eq!(email(&inner), None);
    }

    /// alice 发给 me 的一条真实 PreKey 信封（me 的账户已存入本地）。返回 (信封, alice 身份的 JSON)。
    fn prekey_envelope(inner: &Inner, id: &str) -> (Envelope, String) {
        let mut mine = Account::new();
        mine.generate_one_time_keys(1);
        let otk = *mine.one_time_keys().values().next().unwrap();
        mine.mark_keys_as_published();
        inner.store.save_account(&mine).unwrap();
        let alice = Account::new();
        let mut s = olm::create_outbound(&alice, mine.curve25519_key(), otk).unwrap();
        let c = Content::new(ContentBody::Text { text: "你好".into(), quote: None, mentions: vec![] }, None, 0);
        let (t, bytes) = olm::encrypt(&mut s, &c.to_bytes()).unwrap();
        assert_eq!(t, 0, "新会话的第一条应为 PreKey");
        let env = Envelope {
            id: id.into(),
            kind: "olm".into(),
            from: "alice".into(),
            msg_type: t,
            group_id: None,
            session_id: None,
            body: b64e(bytes),
            server_ts: 1,
        };
        let ident = serde_json::json!({
            "user_id": "alice",
            "identity_key": olm::identity_key_b64(&alice),
            "signing_key": olm::signing_key_b64(&alice),
            "device_generation": 1,
        })
        .to_string();
        (env, ident)
    }

    /// 身份接口按 `status` 应答：200 返回 alice 的身份，其余返回对应错误（503 为无 JSON 的网关错误）。
    async fn identity_stub(status: Arc<AtomicU16>, ident: Arc<std::sync::Mutex<String>>) -> stub::StubServer {
        stub::start(Arc::new(move |_m, path, _b| {
            if !path.starts_with("/v1/keys/identity/") {
                return (404, r#"{"error":"not_found","message":"stub"}"#.into());
            }
            match status.load(Ordering::SeqCst) {
                200 => (200, ident.lock().unwrap().clone()),
                429 => (429, r#"{"error":"rate_limited","message":"慢一点"}"#.into()),
                404 => (404, r#"{"error":"not_found","message":"用户不存在"}"#.into()),
                s => (s, "Service Unavailable".into()),
            }
        }))
        .await
    }

    fn msgs(inner: &Inner) -> Vec<crate::store::MsgRow> {
        inner.store.messages("u:alice", None, 10).unwrap()
    }

    /// 取对方身份时服务端 503 / 429：信封不 ack、不记 seen、不写占位；恢复后重投能正常解密。
    #[tokio::test]
    async fn transient_server_error_keeps_envelope_for_redelivery() {
        let status = Arc::new(AtomicU16::new(503));
        let ident = Arc::new(std::sync::Mutex::new(String::new()));
        let srv = identity_stub(status.clone(), ident.clone()).await;
        let (inner, _dir) = test_inner_at(&srv.url);
        let (env, id_json) = prekey_envelope(&inner, "e1");
        *ident.lock().unwrap() = id_json;

        assert!(!inner.process_envelope(&env, false).await, "503 不应 ack");
        assert!(!inner.store.is_seen("env:e1").unwrap());
        assert!(msgs(&inner).is_empty(), "不应写入消息或占位");

        status.store(429, Ordering::SeqCst);
        assert!(!inner.process_envelope(&env, false).await, "429 rate_limited 不应 ack");
        assert!(msgs(&inner).is_empty());
        assert_eq!(srv.count("GET", "/v1/keys/identity/"), 2);

        status.store(200, Ordering::SeqCst);
        assert!(inner.process_envelope(&env, false).await, "服务端恢复后重投应处理成功");
        let m = msgs(&inner);
        assert_eq!(m.len(), 1);
        assert!(matches!(m[0].msg.body, StoredBody::Text { ref text } if text == "你好"));
        assert!(inner.store.is_seen("env:e1").unwrap());
    }

    /// 服务端按字节截断的短页（条数 < POLL_LIMIT）不代表队列已空：继续拉取直到空页。
    #[tokio::test]
    async fn drain_http_continues_past_short_pages() {
        let pages = Arc::new(std::sync::Mutex::new(vec![
            r#"{"messages":[{"id":"m1","kind":5}],"events":[]}"#,
            r#"{"messages":[{"id":"m2","kind":5},{"id":"m3","kind":5}],"events":[]}"#,
            r#"{"messages":[],"events":[]}"#,
        ]));
        let acked = Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
        let (p2, a2) = (pages.clone(), acked.clone());
        let srv = stub::start(Arc::new(move |m, path, body| match (m, path) {
            ("GET", p) if p.starts_with("/v1/messages") => {
                let mut v = p2.lock().unwrap();
                (200, if v.is_empty() { r#"{"messages":[],"events":[]}"# } else { v.remove(0) }.into())
            }
            ("POST", "/v1/messages/ack") => {
                let j: serde_json::Value = serde_json::from_slice(body).unwrap();
                a2.lock().unwrap().extend(j["ids"].as_array().unwrap().iter().map(|i| i.as_str().unwrap().to_string()));
                (200, "{}".into())
            }
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let (inner, _dir) = test_inner_at(&srv.url);
        assert_eq!(inner.drain_http().await.unwrap(), 3);
        assert_eq!(*acked.lock().unwrap(), vec!["m1", "m2", "m3"]);
        assert_eq!(srv.count("GET", "/v1/messages"), 3, "拿到空页才停止");
    }

    #[test]
    fn malformed_envelopes() {
        assert_eq!(parse_envelope(&serde_json::json!({"id": "x1", "kind": 5})).unwrap_err(), Some("x1".into()));
        assert_eq!(parse_envelope(&serde_json::json!({"kind": "olm"})).unwrap_err(), None);
        let ok = serde_json::json!({"id": "e", "kind": "olm", "from": "u", "type": 1, "body": "AA=="});
        assert_eq!(parse_envelope(&ok).unwrap().id, "e");
    }

    /// 上传媒体：数据直接交给请求，成功后缓存到 media/<id>，不留临时文件。
    #[tokio::test]
    async fn upload_media_caches_without_leftovers() {
        let srv = stub::start(Arc::new(|_m, _p, body| (200, format!(r#"{{"id":"m1","size":{}}}"#, body.len())))).await;
        let (inner, _dir) = test_inner_at(&srv.url);
        let id = inner.upload_media(&inner.api().unwrap(), b"png-bytes".to_vec()).await.unwrap();
        assert_eq!(id, "m1");
        assert_eq!(std::fs::read(inner.media_path("m1")).unwrap(), b"png-bytes");
        let leftovers = std::fs::read_dir(inner.files_dir.join("media")).unwrap().filter(|e| {
            e.as_ref().unwrap().file_name().to_string_lossy().starts_with(".upload-")
        });
        assert_eq!(leftovers.count(), 0);
    }

    #[test]
    fn stale_upload_temp_files_pruned() {
        let (inner, _dir) = crate::client::test_inner();
        let media = inner.files_dir.join("media");
        std::fs::write(media.join(".upload-x"), b"t").unwrap();
        std::fs::write(media.join("keep"), b"k").unwrap();
        let files = &inner.files_dir;
        for n in [".enc-1", ".dl-1", ".dec-1", "abc_0_keep.bin"] {
            std::fs::write(files.join(n), b"t").unwrap();
        }
        inner.prune_upload_temp_files(Duration::from_secs(3600));
        assert!(media.join(".upload-x").exists(), "未过期的临时文件（可能正在上传）保留");
        assert!(files.join(".enc-1").exists());
        inner.prune_upload_temp_files(Duration::ZERO);
        assert!(!media.join(".upload-x").exists());
        for n in [".enc-1", ".dl-1", ".dec-1"] {
            assert!(!files.join(n).exists(), "{n} 应被清理");
        }
        assert!(media.join("keep").exists(), "缓存文件不受影响");
        assert!(files.join("abc_0_keep.bin").exists(), "附件缓存不受影响");
    }

    /// 永久错误（对方不存在）：ack，并留下「无法解密」占位，而不是静默丢弃。
    #[tokio::test]
    async fn permanent_identity_error_leaves_placeholder() {
        let status = Arc::new(AtomicU16::new(404));
        let srv = identity_stub(status, Arc::default()).await;
        let (inner, _dir) = test_inner_at(&srv.url);
        let (env, _) = prekey_envelope(&inner, "e2");
        assert!(inner.process_envelope(&env, false).await, "永久错误应 ack");
        let m = msgs(&inner);
        assert_eq!(m.len(), 1);
        assert!(matches!(m[0].msg.body, StoredBody::Undecryptable));
    }

    /// 身份接口恒为 200 的桩；返回 (桩, 数据目录, 首个 Inner, 信封)。
    async fn crash_setup(id: &str) -> (stub::StubServer, tempfile::TempDir, Arc<Inner>, Envelope) {
        let ident = Arc::new(std::sync::Mutex::new(String::new()));
        let srv = identity_stub(Arc::new(AtomicU16::new(200)), ident.clone()).await;
        let dir = tempfile::tempdir().unwrap();
        let inner = test_inner_in(dir.path(), &srv.url);
        let (env, id_json) = prekey_envelope(&inner, id);
        *ident.lock().unwrap() = id_json;
        // 模拟进程在解密（棘轮推进、明文写入收件箱，同一事务）之后、落成消息之前被杀
        inner.hooks.crash_after_decrypt.store(1, Ordering::SeqCst);
        assert!(!inner.process_envelope(&env, false).await, "「崩溃」时不 ack");
        assert!(msgs(&inner).is_empty());
        assert_eq!(inner.store.inbox_count().unwrap(), 1);
        assert!(inner.store.inbox_plaintext(id).unwrap().is_some(), "明文已与棘轮状态一起落盘");
        assert!(!inner.store.is_seen(&format!("env:{id}")).unwrap());
        (srv, dir, inner, env)
    }

    fn undecryptable(inner: &Inner) -> u64 {
        inner.stats.undecryptable.load(Ordering::Relaxed)
    }

    /// C-H：解密后崩溃，重启后服务端重投同一信封：用收件箱里的明文继续，消息不丢、不出现「无法解密」。
    #[tokio::test]
    async fn crash_after_decrypt_then_redelivery() {
        let (_srv, dir, first, env) = crash_setup("e-crash-1").await;
        drop(first);
        let inner = test_inner_in(dir.path(), "http://127.0.0.1:1"); // 「重启」：同一数据目录，服务端此时不可达也无妨
        assert!(inner.process_envelope(&env, true).await, "重投应处理成功并 ack");
        let m = msgs(&inner);
        assert_eq!(m.len(), 1);
        assert!(matches!(m[0].msg.body, StoredBody::Text { ref text } if text == "你好"));
        assert_eq!(undecryptable(&inner), 0);
        assert_eq!(inner.store.inbox_count().unwrap(), 0, "处理完删除收件箱行");
        assert!(inner.store.is_seen("env:e-crash-1").unwrap());
        // 之后再重投：按 seen 直接 ack，不重复
        assert!(inner.process_envelope(&env, true).await);
        assert_eq!(msgs(&inner).len(), 1);
    }

    /// C-H：重启后 start() 先处理残留收件箱（不等服务端重投）；随后服务端重投时直接 ack。
    #[tokio::test]
    async fn crash_after_decrypt_then_startup_residue() {
        let (_srv, dir, first, env) = crash_setup("e-crash-2").await;
        drop(first);
        let inner = test_inner_in(dir.path(), "http://127.0.0.1:1");
        assert_eq!(inner.process_inbox_residue().await, 1);
        let m = msgs(&inner);
        assert_eq!(m.len(), 1);
        assert!(matches!(m[0].msg.body, StoredBody::Text { ref text } if text == "你好"));
        assert_eq!(undecryptable(&inner), 0);
        assert_eq!(inner.store.inbox_count().unwrap(), 0);
        assert!(inner.process_envelope(&env, false).await);
        assert_eq!(msgs(&inner).len(), 1);
        assert_eq!(inner.process_inbox_residue().await, 0);
    }

    /// 对照：若没有保存的明文（旧实现的情形），重投的信封因棘轮已推进而无法解密。证明上面两个测试确实覆盖了崩溃窗口。
    #[tokio::test]
    async fn crash_window_without_saved_plaintext_is_undecryptable() {
        let (_srv, _dir, inner, env) = crash_setup("e-crash-3").await;
        inner.store.inbox_delete("e-crash-3").unwrap();
        assert!(inner.process_envelope(&env, true).await);
        assert!(matches!(msgs(&inner)[0].msg.body, StoredBody::Undecryptable));
        assert_eq!(undecryptable(&inner), 1);
    }

    struct Events(std::sync::Mutex<std::sync::mpsc::Sender<CoreEvent>>);
    impl EventListener for Events {
        fn on_event(&self, e: CoreEvent) {
            let _ = self.0.lock().unwrap().send(e);
        }
    }

    /// C-F：MessageAdded 带上 backlog 标记（积压 / HTTP 补拉为 true，实时为 false）。
    #[tokio::test]
    async fn message_added_carries_backlog_flag() {
        let ident = Arc::new(std::sync::Mutex::new(String::new()));
        let srv = identity_stub(Arc::new(AtomicU16::new(200)), ident.clone()).await;
        for backlog in [true, false] {
            let (inner, _dir) = test_inner_at(&srv.url);
            let (tx, rx) = std::sync::mpsc::channel();
            inner.set_listener(Arc::new(Events(std::sync::Mutex::new(tx))));
            let (env, id_json) = prekey_envelope(&inner, "e-b");
            *ident.lock().unwrap() = id_json;
            assert!(inner.process_envelope(&env, backlog).await);
            let got = loop {
                match rx.recv_timeout(Duration::from_secs(3)).expect("应有 MessageAdded") {
                    CoreEvent::MessageAdded { backlog, .. } => break backlog,
                    _ => continue,
                }
            };
            assert_eq!(got, backlog);
        }
    }
}
