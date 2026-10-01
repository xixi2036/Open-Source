//! 接收：处理解密后的内容（可见消息、回应、撤回、回执、输入状态、阅后即焚设置、通话信令、room_key、
//! session_reset）、挂起群消息、阅后即焚扫描。

use std::collections::{HashMap, HashSet};
use std::sync::Arc;

use super::secure::Incoming;
use super::{direct_conv, group_conv, parse_conv, timer_key, ConvRef, Inner};
use super::messaging::{describe_timer, CLOCK_SKEW_MS, DELETE_WINDOW_MS};
use crate::api::Envelope;
use crate::content::{valid_mentions, Content, ContentBody, ReceiptKind, TypingAction, WireQuote};
use crate::error::Result;
use crate::store::{clamp_ts, MsgRow, StoredAttachment, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::now_ms;

/// 送达回执攒批：最后一条消息到达后等这么久统一发出
const RECEIPT_FLUSH_MS: u64 = 250;
/// 一条回执内容里最多带这么多消息 id
const RECEIPT_MAX_IDS: usize = 500;

/// 待发送的送达回执（C3）。
#[derive(Default)]
pub struct ReceiptQueue {
    /// (原发送者, 群 id) → 消息内容 id
    ids: HashMap<(String, Option<String>), Vec<String>>,
    /// 本地还没有资料的发送者：回执发完后顺便拉取一次
    profiles: HashSet<String>,
    /// 已有后台任务在等着发送
    scheduled: bool,
}

impl Inner {
    /// 给收到但无法呈现的信封存一条占位消息：`Undecryptable`（无法解密，id = 信封 id），或
    /// `System{[不支持的消息，请升级]}`（解密成功但本版本解析不了，A1）。计未读，不参与已读回执。
    pub(crate) fn store_incoming_notice(&self, conv_id: &str, id: &str, env: &Envelope, body: StoredBody, backlog: bool) -> Result<()> {
        self.store.ensure_conv(conv_id, 0)?;
        let now = now_ms();
        let server_ts = clamp_ts(env.server_ts);
        let mut row = MsgRow {
            id: id.to_string(),
            conv_id: conv_id.to_string(),
            sender_id: env.from.clone(),
            outgoing: false,
            sent_at: server_ts,
            server_ts,
            sort_ts: if server_ts > 0 { server_ts } else { now },
            status: MessageStatus::Delivered,
            msg: StoredMessage::plain(body, None),
            expire_secs: 0,
            expires_at: None,
            deleted: false,
            read: false,
            mentions_me: false,
        };
        if self.store.insert_incoming(&mut row)? {
            self.store.bump_conv(conv_id, row.sort_ts, 1)?;
            self.emit_added(&row, backlog);
            self.emit(CoreEvent::ConversationsChanged);
        }
        Ok(())
    }

    /// 内容所属会话（A4）：经 Megolm 收到的属于该群；经 Olm 收到的一律属于与发送者的单聊，不看 `Content.group_id`。
    /// 以前「带 group_id 且发送者是群成员（或群已不活跃）就归群」，于是成员私发给我一个人的内容会冒充群消息
    /// （别的成员根本看不到），非成员也能向我已退出的群注入消息。经 Olm 到达、又与群有关的只有两种，都不依赖这里：
    /// 回执按消息 id 定位（`on_receipt`），room_key 单独处理。
    fn conv_for(&self, inc: &Incoming) -> String {
        match &inc.via_group {
            Some(g) => group_conv(g),
            None => direct_conv(&inc.from),
        }
    }

    /// 核对引用（A7）：按 (id, author) 在本会话里找原消息，找到就用**本地**的预览文本（≤200 字），
    /// 找不到（对方伪造、张冠李戴，或原消息已被本地清掉）标「原消息不可用」。对方给的引用文本不入库。
    fn verified_quote(&self, conv_id: &str, q: WireQuote) -> Quote {
        let text = match self.store.find_by_content(Some(conv_id), &q.id, &q.author) {
            Ok(Some(m)) => super::messaging::quote_text(&m.msg.body),
            _ => "原消息不可用".to_string(),
        };
        let cut = |s: String| if s.len() > 128 { s.chars().take(128).collect() } else { s };
        Quote { id: cut(q.id), author: cut(q.author), text }
    }

    /// 处理一条已解密的内容。
    pub(crate) async fn handle_incoming(self: &Arc<Self>, inc: Incoming) -> Result<()> {
        if !crate::store::valid_content_id(&inc.content.id) {
            tracing::warn!("丢弃来自 {} 的内容：id 格式非法", inc.from);
            return Ok(());
        }
        // 内容级去重键带上发送者：不同发送者用同一个 id 不会互相吞掉（旧版本的键是裸 id，保留期内一并检查）
        let seen_key = format!("msg:{}:{}", inc.from, inc.content.id);
        if self.store.is_seen(&seen_key)? || self.store.is_seen(&inc.content.id)? {
            return Ok(());
        }
        let conv_id = self.conv_for(&inc);
        let from = inc.from.clone();
        match inc.content.body.clone() {
            ContentBody::Text { .. } | ContentBody::Attachment { .. } => self.on_visible_message(&conv_id, &inc)?,
            ContentBody::Reaction { target_id, target_author, emoji, remove } => {
                if let Some(t) = self.store.find_by_content(Some(&conv_id), &target_id, &target_author)? {
                    if !t.deleted {
                        // 回应挂在目标所在的本地行上（id 冲突时是派生 id `发送者:内容id`），不能用裸内容 id
                        if remove {
                            self.store.remove_reaction(&t.id, &from, &emoji)?;
                        } else {
                            self.store.set_reaction(&t.id, &from, &emoji)?;
                        }
                        self.emit(CoreEvent::MessageUpdated { conv_id: conv_id.clone(), message: self.msg_item(&t) });
                    }
                }
            }
            ContentBody::Delete { target_id } => {
                if let Some(t) = self.store.find_by_content(Some(&conv_id), &target_id, &from)? {
                    // A9：24 小时窗口按服务端时间算（撤回信封的 server_ts 对比原消息的 server_ts），
                    // 不看发送方自报的 Content.ts——否则把 ts 填成原消息之后一秒，多久以前的消息都能撤回
                    let recalled_at = if inc.server_ts > 0 { clamp_ts(inc.server_ts) } else { now_ms() };
                    let original_at = if t.server_ts > 0 { t.server_ts } else { t.sort_ts };
                    let in_window = recalled_at - original_at <= DELETE_WINDOW_MS + CLOCK_SKEW_MS;
                    if !t.outgoing && !t.deleted && in_window {
                        self.mark_deleted(&t.id)?;
                    }
                }
            }
            ContentBody::Receipt { kind, ids } => self.on_receipt(&from, kind, &ids)?,
            ContentBody::Typing { action } => {
                if self.settings().typing_indicators && action != TypingAction::Unknown {
                    self.emit(CoreEvent::Typing { conv_id, user_id: from, typing: action == TypingAction::Started });
                }
            }
            ContentBody::Timer { expire_secs } => {
                self.store.ensure_conv(&conv_id, 0)?;
                // 后写者胜：晚到的旧设置（对方改设置时尚未收到我们更新的设置）不覆盖本地。
                // A5：设置的变更时间以信封的服务端时间为准，不用发送方自报的时间——对方时钟偏快（或伪造成
                // i64::MAX）时，自报时间会让他的设置永远「最新」。信封没有服务端时间时才退回自报值，且不超过当前时间。
                let at = if inc.server_ts > 0 {
                    clamp_ts(inc.server_ts)
                } else {
                    let claimed = if inc.content.expire_set_at > 0 { inc.content.expire_set_at } else { inc.content.ts };
                    claimed.min(now_ms())
                };
                if at >= self.timer_set_at(&conv_id) {
                    self.set_timer(&conv_id, expire_secs, at)?;
                    let name = self.display_name(&from);
                    self.insert_system(&conv_id, &format!("{name} 将阅后即焚设置为：{}", describe_timer(expire_secs)))?;
                } else {
                    tracing::info!("忽略 {from} 较早的阅后即焚设置（{at}）");
                }
            }
            ContentBody::Call { call_id, action, video, sdp, candidates } => {
                match action {
                    Some(action) if inc.via_group.is_none() => {
                        let signal = CallSignal { call_id, action, video, sdp, candidates: candidates.unwrap_or_default() };
                        let sent_ts = if inc.server_ts > 0 { inc.server_ts } else { inc.content.ts };
                        self.on_call_signal(&from, signal, sent_ts)?;
                    }
                    Some(_) => {}
                    None => tracing::info!("忽略 {from} 的通话信令：本版本不认识的动作"),
                }
            }
            ContentBody::RoomKey { group_id, session_id, session_key } => {
                if inc.via_group.is_none() {
                    let pending = self.accept_room_key(&from, &group_id, &session_id, &session_key).await?;
                    self.store.mark_seen(&seen_key)?;
                    // 逐条处理挂起消息：单条出错只影响它自己
                    // 挂起的消息沿用本条 room_key 的 backlog 标记
                    for env in pending {
                        self.process_pending_group_message(&env, inc.backlog).await;
                    }
                    return Ok(());
                }
            }
            ContentBody::SessionReset {} => {
                if inc.via_group.is_none() {
                    self.on_session_reset(&from, inc.olm_session_id.as_deref()).await?;
                }
            }
            ContentBody::ProfileKey {} | ContentBody::GroupUpdate {} | ContentBody::Unknown => {}
        }
        self.store.mark_seen(&seen_key)?;
        Ok(())
    }

    /// 收到 room_key 后处理一条挂起的群消息。出错只记日志：网络类错误保留挂起记录，其他错误写入占位并删除挂起记录。
    async fn process_pending_group_message(self: &Arc<Self>, env: &Envelope, backlog: bool) {
        use super::secure::EnvelopeOutcome;
        let r: Result<bool> = async {
            match self.decrypt_envelope(env, backlog).await? {
                EnvelopeOutcome::Decrypted(i) => {
                    Box::pin(self.handle_incoming(*i)).await?;
                    Ok(true)
                }
                EnvelopeOutcome::Pending => Ok(false),
                _ => Ok(true),
            }
        }
        .await;
        let done = match r {
            Ok(done) => done,
            Err(e) if e.is_transient() => {
                tracing::warn!("处理挂起群消息 {} 暂时失败，保留: {e}", env.id);
                false
            }
            Err(e) => {
                tracing::warn!("处理挂起群消息 {} 失败，记为无法解密: {e}", env.id);
                let _ = self.undecryptable(env, &e.to_string(), backlog);
                true
            }
        };
        if done {
            if let Err(e) = self.store.delete_pending(&env.id) {
                tracing::warn!("删除挂起记录失败: {e}");
            }
        }
    }

    fn on_visible_message(self: &Arc<Self>, conv_id: &str, inc: &Incoming) -> Result<()> {
        let c = &inc.content;
        let (body, quote, mentions) = match &c.body {
            // 提及区间不合法（越界等）的项直接丢弃，不影响消息本身（K6）
            ContentBody::Text { text, quote, mentions } => {
                (StoredBody::Text { text: text.clone() }, quote.clone(), valid_mentions(text, mentions.clone()))
            }
            ContentBody::Attachment { attachments, caption, quote } => (
                StoredBody::Attachments {
                    items: attachments.iter().map(|w| StoredAttachment { wire: w.clone(), local_path: None }).collect(),
                    caption: caption.clone(),
                },
                quote.clone(),
                vec![],
            ),
            _ => return Ok(()),
        };
        let me = self.my_id()?;
        let mentions_me = mentions.iter().any(|m| m.user_id == me);
        let now = now_ms();
        // A6：服务端时间戳钳到当前时间附近
        let server_ts = clamp_ts(inc.server_ts);
        // A5：消息里自报的「设置变更时间」晚于这条消息本身的服务端时间（对方时钟偏快，或伪造成极大值）时不可信，
        // 不参与阅后即焚设置的同步
        let sent_ref = if server_ts > 0 { server_ts } else { now };
        let set_at = if c.expire_set_at > sent_ref + CLOCK_SKEW_MS { 0 } else { c.expire_set_at.max(0) };
        let created = self.store.ensure_conv(conv_id, c.expire_secs)?;
        if conv_id.starts_with("u:") {
            if created {
                self.store.kv_set_json(&timer_key(conv_id), &set_at)?;
            } else if let Some(cr) = self.store.get_conv(conv_id)? {
                // 与发送方的计时器同步，但只采用比本地更新的设置（消息带着发送方设置的变更时间）：
                // 双方同时改设置时不会被对方还没更新的消息悄悄改回去；同步时插入系统提示
                if cr.expire_secs != c.expire_secs && set_at > self.timer_set_at(conv_id) {
                    self.set_timer(conv_id, c.expire_secs, set_at)?;
                    let name = self.display_name(&inc.from);
                    self.insert_system(conv_id, &format!("已与 {name} 同步阅后即焚设置：{}", describe_timer(c.expire_secs)))?;
                }
            }
        }
        let mut row = MsgRow {
            id: c.id.clone(),
            conv_id: conv_id.to_string(),
            sender_id: inc.from.clone(),
            outgoing: false,
            sent_at: c.ts,
            server_ts,
            sort_ts: if server_ts > 0 { server_ts } else { now },
            status: MessageStatus::Delivered,
            msg: StoredMessage { body, quote: quote.map(|q| self.verified_quote(conv_id, q)), forwarded: c.forwarded, mentions },
            expire_secs: c.expire_secs,
            expires_at: None,
            deleted: false,
            read: false,
            mentions_me,
        };
        if !self.store.insert_incoming(&mut row)? {
            return Ok(());
        }
        self.store.bump_conv(conv_id, row.sort_ts, 1)?;
        self.emit_added(&row, inc.backlog);
        self.emit(CoreEvent::ConversationsChanged);

        // 送达回执（经 Olm 发给原发送者）：攒批发送（C3）
        let need_profile = self.store.get_profile(&inc.from)?.is_none();
        self.queue_delivery_receipt(&inc.from, inc.via_group.clone(), c.id.clone(), need_profile);
        Ok(())
    }

    /// 把一条送达回执放进待发队列，并确保有一个后台任务稍后统一发出（C3）。
    ///
    /// 以前每收到一条消息就派生一个任务、各发一次 `POST /v1/messages`：3000 条积压就是 3000 次请求，
    /// 还没有 Olm 会话时会并发 claim 十几次。现在按（发送者, 群）攒成一条回执内容，一批请求发完所有发送者的回执。
    fn queue_delivery_receipt(self: &Arc<Self>, from: &str, group_id: Option<String>, content_id: String, need_profile: bool) {
        let spawn = {
            let mut q = self.receipts.lock().unwrap_or_else(|e| e.into_inner());
            q.ids.entry((from.to_string(), group_id)).or_default().push(content_id);
            if need_profile {
                q.profiles.insert(from.to_string());
            }
            !std::mem::replace(&mut q.scheduled, true)
        };
        if !spawn {
            return;
        }
        let me = self.clone();
        let generation = self.session_gen();
        tokio::spawn(async move {
            loop {
                tokio::time::sleep(std::time::Duration::from_millis(RECEIPT_FLUSH_MS)).await;
                let (ids, profiles) = {
                    let mut q = me.receipts.lock().unwrap_or_else(|e| e.into_inner());
                    if q.ids.is_empty() && q.profiles.is_empty() {
                        q.scheduled = false;
                        return;
                    }
                    (std::mem::take(&mut q.ids), std::mem::take(&mut q.profiles))
                };
                // 期间登出或换了账号：旧账号欠的回执作废，不能以新账号的身份发出
                if !me.same_session(generation) {
                    let mut q = me.receipts.lock().unwrap_or_else(|e| e.into_inner());
                    *q = ReceiptQueue::default();
                    return;
                }
                me.flush_delivery_receipts(ids).await;
                let mut fetched = false;
                for uid in profiles {
                    if !me.same_session(generation) {
                        break;
                    }
                    fetched |= me.get_user(&uid).await.is_ok();
                }
                if fetched {
                    me.emit(CoreEvent::ConversationsChanged);
                }
            }
        });
    }

    async fn flush_delivery_receipts(&self, ids: HashMap<(String, Option<String>), Vec<String>>) {
        let mut items: Vec<(String, Content, bool)> = Vec::new();
        for ((uid, group_id), ids) in ids {
            for chunk in ids.chunks(RECEIPT_MAX_IDS) {
                let rc = Content::new(ContentBody::Receipt { kind: ReceiptKind::Delivered, ids: chunk.to_vec() }, group_id.clone(), 0);
                items.push((uid.clone(), rc, false));
            }
        }
        // 服务端单次 POST /v1/messages 最多 1000 条
        while !items.is_empty() {
            let batch: Vec<_> = items.drain(..items.len().min(200)).collect();
            match self.send_olm_batch(batch).await {
                Ok(results) => {
                    for e in results.into_iter().filter_map(|r| r.err()) {
                        tracing::warn!("送达回执发送失败: {e}");
                    }
                }
                Err(e) => tracing::warn!("送达回执发送失败: {e}"),
            }
        }
    }

    fn on_receipt(&self, from: &str, kind: ReceiptKind, ids: &[String]) -> Result<()> {
        let target = match kind {
            ReceiptKind::Delivered => MessageStatus::Delivered,
            ReceiptKind::Read => MessageStatus::Read,
            ReceiptKind::Unknown => return Ok(()),
        };
        let me = self.my_id()?;
        for id in ids {
            // 回执引用的是我发出的内容 id
            let Some(m) = self.store.find_by_content(None, id, &me)? else { continue };
            if !m.outgoing {
                continue;
            }
            let allowed = match parse_conv(&m.conv_id)? {
                ConvRef::Direct(uid) => uid == from,
                ConvRef::Group(gid) => self.store.get_group(gid)?.map(|g| g.is_member(from)).unwrap_or(false),
            };
            if !allowed {
                continue;
            }
            // 只改状态一列，且基于库里最新的行（A3）：并发的撤回、下载写回不会被覆盖
            let updated = self.store.modify_message(&m.id, |r| {
                let raise = r.status.rank() < target.rank();
                if raise {
                    r.status = target;
                }
                raise
            })?;
            if let Some(m) = updated {
                self.emit(CoreEvent::MessageUpdated { conv_id: m.conv_id.clone(), message: self.msg_item(&m) });
            }
        }
        Ok(())
    }

    /// 阅后即焚扫描：删除到期消息。
    pub(crate) fn sweep_expired(&self) -> Result<usize> {
        let expired = self.store.expired_messages(now_ms())?;
        for (id, conv) in &expired {
            if let Some(row) = self.store.get_message(id)? {
                self.remove_local_files(&row);
            }
            self.store.delete_message(id)?;
            self.emit(CoreEvent::MessageRemoved { conv_id: conv.clone(), message_id: id.clone() });
        }
        if !expired.is_empty() {
            self.emit(CoreEvent::ConversationsChanged);
        }
        Ok(expired.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::{test_group, test_inner};

    fn text(t: &str) -> ContentBody {
        ContentBody::Text { text: t.into(), quote: None, mentions: vec![] }
    }

    fn inc(from: &str, gid: &str, id: &str, body: ContentBody) -> Incoming {
        let mut c = Content::new(body, Some(gid.into()), 0);
        c.id = id.into();
        Incoming { from: from.into(), content: c, server_ts: 1, via_group: Some(gid.into()), olm_session_id: None, backlog: false }
    }

    /// 不同发送者使用相同的消息 id：两条都保留；撤回、回应按 (id, 发送者) 定位，不会串到别人的消息上。
    #[tokio::test]
    async fn colliding_ids_from_different_senders() {
        let (inner, _dir) = test_inner();
        inner.store.put_group(&test_group("g1", &["me", "alice", "bob", "carol"]), true).unwrap();
        let conv = "g:g1";
        inner.handle_incoming(inc("alice", "g1", "same-id", text("alice 的消息"))).await.unwrap();
        inner.handle_incoming(inc("bob", "g1", "same-id", text("bob 撞了 id"))).await.unwrap();
        // 内容级去重按发送者：bob 的同 id 内容不会被当成 alice 那条的重复
        let msgs = inner.store.messages(conv, None, 10).unwrap();
        assert_eq!(msgs.len(), 2, "两条都应保留");
        let alice_row = inner.store.find_by_content(Some(conv), "same-id", "alice").unwrap().unwrap();
        let bob_row = inner.store.find_by_content(Some(conv), "same-id", "bob").unwrap().unwrap();
        assert_ne!(alice_row.id, bob_row.id);

        // bob 撤回自己的 same-id：只删 bob 的
        inner.handle_incoming(inc("bob", "g1", "del-1", ContentBody::Delete { target_id: "same-id".into() })).await.unwrap();
        assert!(inner.store.get_message(&bob_row.id).unwrap().unwrap().deleted);
        assert!(!inner.store.get_message(&alice_row.id).unwrap().unwrap().deleted, "alice 的消息不能被 bob 撤回");

        // carol 回应 alice 的 same-id
        inner
            .handle_incoming(inc(
                "carol",
                "g1",
                "r-1",
                ContentBody::Reaction { target_id: "same-id".into(), target_author: "alice".into(), emoji: "👍".into(), remove: false },
            ))
            .await
            .unwrap();
        assert_eq!(inner.store.reactions(&alice_row.id).unwrap().len(), 1);
        assert!(inner.store.reactions(&bob_row.id).unwrap().is_empty());

        // 非法 id（含冒号，企图伪造派生 id）被丢弃
        inner.handle_incoming(inc("bob", "g1", "alice:x", text("伪造"))).await.unwrap();
        assert_eq!(inner.store.messages(conv, None, 10).unwrap().len(), 2);
    }

    /// 回应存放在派生 id（`bob:same-id`）的消息：回应必须挂在 bob 那一行上，而不是裸 id 对应的 alice 那一行。
    #[tokio::test]
    async fn reaction_to_message_stored_under_derived_id() {
        let (inner, _dir) = test_inner();
        inner.store.put_group(&test_group("g1", &["me", "alice", "bob", "carol"]), true).unwrap();
        let conv = "g:g1";
        inner.handle_incoming(inc("alice", "g1", "same-id", text("alice 的消息"))).await.unwrap();
        inner.handle_incoming(inc("bob", "g1", "same-id", text("bob 撞了 id"))).await.unwrap();
        let alice_row = inner.store.find_by_content(Some(conv), "same-id", "alice").unwrap().unwrap();
        let bob_row = inner.store.find_by_content(Some(conv), "same-id", "bob").unwrap().unwrap();
        assert_eq!(bob_row.id, "bob:same-id");
        let react = |id: &str, remove: bool| {
            inc("carol", "g1", id, ContentBody::Reaction { target_id: "same-id".into(), target_author: "bob".into(), emoji: "🎉".into(), remove })
        };
        inner.handle_incoming(react("r-1", false)).await.unwrap();
        assert_eq!(inner.store.reactions(&bob_row.id).unwrap().len(), 1, "回应应挂在 bob 的消息上");
        assert!(inner.store.reactions(&alice_row.id).unwrap().is_empty(), "不能串到 alice 的消息上");
        inner.handle_incoming(react("r-2", true)).await.unwrap();
        assert!(inner.store.reactions(&bob_row.id).unwrap().is_empty(), "撤销回应同样按 bob 那一行");
    }

    fn direct(from: &str, id: &str, ts: i64, expire_secs: u32, expire_set_at: i64, body: ContentBody) -> Incoming {
        let mut c = Content::new(body, None, expire_secs);
        c.id = id.into();
        c.ts = ts;
        c.expire_set_at = expire_set_at;
        Incoming { from: from.into(), content: c, server_ts: ts, via_group: None, olm_session_id: Some("s".into()), backlog: false }
    }

    /// 阅后即焚「后写者胜」：对方尚未收到我们新设置时发来的消息不会把设置悄悄改回去；更新的设置才会同步，并有系统提示。
    #[tokio::test]
    async fn disappearing_timer_last_writer_wins() {
        let (inner, _dir) = test_inner();
        let conv = "u:bob";
        let expire = |i: &Inner| i.store.get_conv(conv).unwrap().unwrap().expire_secs;
        // 首条消息建立会话，采用对方的设置
        inner.handle_incoming(direct("bob", "m1", 1_000, 0, 0, text("hi"))).await.unwrap();
        assert_eq!(expire(&inner), 0);
        // 我们在 t=2000 设为 1 小时（本地记录）
        inner.set_timer(conv, 3600, 2_000).unwrap();
        // bob 还没收到，发来 expire=0 的消息（他的设置从未变更过）：不改
        inner.handle_incoming(direct("bob", "m2", 2_500, 0, 0, text("还是 0"))).await.unwrap();
        assert_eq!(expire(&inner), 3600, "旧设置的消息不能静默关闭计时器");
        // 晚到的旧 Timer（t=1500）也不改
        inner.handle_incoming(direct("bob", "t1", 1_500, 60, 1_500, ContentBody::Timer { expire_secs: 60 })).await.unwrap();
        assert_eq!(expire(&inner), 3600);
        // 更新的 Timer 生效
        inner.handle_incoming(direct("bob", "t2", 3_000, 0, 3_000, ContentBody::Timer { expire_secs: 0 })).await.unwrap();
        assert_eq!(expire(&inner), 0);
        // 携带更新设置时间的普通消息：同步并插入系统提示
        inner.handle_incoming(direct("bob", "m3", 4_100, 300, 4_000, text("5 分钟"))).await.unwrap();
        assert_eq!(expire(&inner), 300);
        assert_eq!(inner.timer_set_at(conv), 4_000);
        let notes = inner
            .store
            .messages(conv, None, 50)
            .unwrap()
            .iter()
            .filter(|m| matches!(&m.msg.body, StoredBody::System { text } if text.contains("同步阅后即焚")))
            .count();
        assert_eq!(notes, 1, "同步时应提示");
        // 我们发出的内容带上本地设置时间
        let row = inner.store.messages(conv, None, 50).unwrap().into_iter().find(|m| m.id == "m3").unwrap();
        let mut out = row.clone();
        out.outgoing = true;
        assert_eq!(inner.content_for(&out).unwrap().expire_set_at, 4_000);
    }

    /// 阅后即焚扫描：删除到期消息及其本地附件文件，并发出 MessageRemoved；未到期的保留。
    #[tokio::test]
    async fn sweep_removes_expired_messages_and_files() {
        use std::sync::mpsc;
        struct Tx(std::sync::Mutex<mpsc::Sender<CoreEvent>>);
        impl EventListener for Tx {
            fn on_event(&self, e: CoreEvent) {
                let _ = self.0.lock().unwrap().send(e);
            }
        }
        let (inner, _dir) = test_inner();
        let (tx, rx) = mpsc::channel();
        inner.set_listener(Arc::new(Tx(std::sync::Mutex::new(tx))));
        let file = inner.attachment_path("old", 0, "a.bin");
        std::fs::write(&file, b"secret").unwrap();
        let wire = crate::content::WireAttachment {
            id: "att".into(),
            key: String::new(),
            digest: String::new(),
            size: 6,
            mime: "application/octet-stream".into(),
            file_name: "a.bin".into(),
            kind: AttachmentKind::File,
            width: 0,
            height: 0,
            duration_ms: 0,
            thumbnail: None,
            format: 1,
        };
        let row = |id: &str, expires_at: i64, body: StoredBody| MsgRow {
            id: id.into(),
            conv_id: "u:bob".into(),
            sender_id: "bob".into(),
            outgoing: false,
            sent_at: 1,
            server_ts: 1,
            sort_ts: 1,
            status: MessageStatus::Read,
            msg: StoredMessage::plain(body, None),
            expire_secs: 10,
            expires_at: Some(expires_at),
            deleted: false,
            read: true,
            mentions_me: false,
        };
        let att = StoredBody::Attachments {
            items: vec![StoredAttachment { wire, local_path: Some(file.to_string_lossy().to_string()) }],
            caption: None,
        };
        inner.store.insert_message(&mut row("old", now_ms() - 1, att)).unwrap();
        inner.store.insert_message(&mut row("new", now_ms() + 60_000, StoredBody::Text { text: "还没到期".into() })).unwrap();

        assert_eq!(inner.sweep_expired().unwrap(), 1);
        assert!(inner.store.get_message("old").unwrap().is_none());
        assert!(inner.store.get_message("new").unwrap().is_some());
        assert!(!file.exists(), "到期消息的本地附件应一并删除");
        let mut removed = false;
        while let Ok(e) = rx.recv_timeout(std::time::Duration::from_secs(2)) {
            if matches!(e, CoreEvent::MessageRemoved { ref message_id, .. } if message_id == "old") {
                removed = true;
                break;
            }
        }
        assert!(removed, "应发出 MessageRemoved");
    }
}
