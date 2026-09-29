//! 接收：处理解密后的内容（可见消息、回应、撤回、回执、输入状态、阅后即焚设置、通话信令、room_key、
//! session_reset）、挂起群消息、阅后即焚扫描。

use std::sync::Arc;

use super::secure::Incoming;
use super::{direct_conv, group_conv, parse_conv, timer_key, ConvRef, Inner};
use super::messaging::{describe_timer, CLOCK_SKEW_MS, DELETE_WINDOW_MS};
use crate::api::Envelope;
use crate::content::{valid_mentions, Content, ContentBody, ReceiptKind, TypingAction};
use crate::error::Result;
use crate::store::{MsgRow, StoredAttachment, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::now_ms;

impl Inner {
    pub(crate) fn store_incoming_placeholder(&self, conv_id: &str, env: &Envelope, backlog: bool) -> Result<()> {
        self.store.ensure_conv(conv_id, 0)?;
        let now = now_ms();
        let mut row = MsgRow {
            id: env.id.clone(),
            conv_id: conv_id.to_string(),
            sender_id: env.from.clone(),
            outgoing: false,
            sent_at: env.server_ts,
            server_ts: env.server_ts,
            sort_ts: if env.server_ts > 0 { env.server_ts } else { now },
            status: MessageStatus::Delivered,
            msg: StoredMessage::plain(StoredBody::Undecryptable, None),
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

    /// 内容所属会话：经 Megolm 的为该群；经 Olm 的若带 group_id 且发送者是群成员则为群，否则为单聊。
    fn conv_for(&self, inc: &Incoming) -> Result<String> {
        if let Some(g) = &inc.via_group {
            return Ok(group_conv(g));
        }
        if let Some(g) = &inc.content.group_id {
            if let Some(grp) = self.store.get_group(g)? {
                if grp.is_member(&inc.from) || !grp.active {
                    return Ok(group_conv(g));
                }
            }
        }
        Ok(direct_conv(&inc.from))
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
        let conv_id = self.conv_for(&inc)?;
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
                if let Some(mut t) = self.store.find_by_content(Some(&conv_id), &target_id, &from)? {
                    let in_window = inc.content.ts - t.sent_at <= DELETE_WINDOW_MS + CLOCK_SKEW_MS;
                    if !t.outgoing && !t.deleted && in_window {
                        self.mark_deleted(&mut t)?;
                    }
                }
            }
            ContentBody::Receipt { kind, ids } => self.on_receipt(&from, kind, &ids)?,
            ContentBody::Typing { action } => {
                if self.settings().typing_indicators {
                    self.emit(CoreEvent::Typing { conv_id, user_id: from, typing: action == TypingAction::Started });
                }
            }
            ContentBody::Timer { expire_secs } => {
                self.store.ensure_conv(&conv_id, 0)?;
                // 后写者胜：晚到的旧设置（对方改设置时尚未收到我们更新的设置）不覆盖本地
                let at = if inc.content.expire_set_at > 0 { inc.content.expire_set_at } else { inc.content.ts };
                if at >= self.timer_set_at(&conv_id) {
                    self.set_timer(&conv_id, expire_secs, at)?;
                    let name = self.display_name(&from);
                    self.insert_system(&conv_id, &format!("{name} 将阅后即焚设置为：{}", describe_timer(expire_secs)))?;
                } else {
                    tracing::info!("忽略 {from} 较早的阅后即焚设置（{at}）");
                }
            }
            ContentBody::Call { call_id, action, video, sdp, candidates } => {
                if inc.via_group.is_none() {
                    let signal = CallSignal { call_id, action, video, sdp, candidates: candidates.unwrap_or_default() };
                    let sent_ts = if inc.server_ts > 0 { inc.server_ts } else { inc.content.ts };
                    self.on_call_signal(&from, signal, sent_ts)?;
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
        let created = self.store.ensure_conv(conv_id, c.expire_secs)?;
        if conv_id.starts_with("u:") {
            if created {
                self.store.kv_set_json(&timer_key(conv_id), &c.expire_set_at)?;
            } else if let Some(cr) = self.store.get_conv(conv_id)? {
                // 与发送方的计时器同步，但只采用比本地更新的设置（消息带着发送方设置的变更时间）：
                // 双方同时改设置时不会被对方还没更新的消息悄悄改回去；同步时插入系统提示
                if cr.expire_secs != c.expire_secs && c.expire_set_at > self.timer_set_at(conv_id) {
                    self.set_timer(conv_id, c.expire_secs, c.expire_set_at)?;
                    let name = self.display_name(&inc.from);
                    self.insert_system(conv_id, &format!("已与 {name} 同步阅后即焚设置：{}", describe_timer(c.expire_secs)))?;
                }
            }
        }
        let now = now_ms();
        let mut row = MsgRow {
            id: c.id.clone(),
            conv_id: conv_id.to_string(),
            sender_id: inc.from.clone(),
            outgoing: false,
            sent_at: c.ts,
            server_ts: inc.server_ts,
            sort_ts: if inc.server_ts > 0 { inc.server_ts } else { now },
            status: MessageStatus::Delivered,
            msg: StoredMessage { body, quote: quote.map(Quote::from), forwarded: c.forwarded, mentions },
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

        // 送达回执（经 Olm 发给原发送者）
        let me = self.clone();
        let from = inc.from.clone();
        let rc = Content::new(ContentBody::Receipt { kind: ReceiptKind::Delivered, ids: vec![c.id.clone()] }, c.group_id.clone(), 0);
        let need_profile = self.store.get_profile(&from)?.is_none();
        let generation = self.session_gen();
        tokio::spawn(async move {
            if !me.same_session(generation) {
                return;
            }
            if let Err(e) = me.send_olm(&from, rc, false).await {
                tracing::warn!("送达回执发送失败: {e}");
            }
            if need_profile && me.same_session(generation) && me.get_user(&from).await.is_ok() {
                me.emit(CoreEvent::ConversationsChanged);
            }
        });
        Ok(())
    }

    fn on_receipt(&self, from: &str, kind: ReceiptKind, ids: &[String]) -> Result<()> {
        let target = match kind {
            ReceiptKind::Delivered => MessageStatus::Delivered,
            ReceiptKind::Read => MessageStatus::Read,
        };
        let me = self.my_id()?;
        for id in ids {
            // 回执引用的是我发出的内容 id
            let Some(mut m) = self.store.find_by_content(None, id, &me)? else { continue };
            if !m.outgoing {
                continue;
            }
            let allowed = match parse_conv(&m.conv_id)? {
                ConvRef::Direct(uid) => uid == from,
                ConvRef::Group(gid) => self.store.get_group(gid)?.map(|g| g.is_member(from)).unwrap_or(false),
            };
            if !allowed || m.status.rank() >= target.rank() {
                continue;
            }
            m.status = target;
            self.store.update_message(&m)?;
            self.emit(CoreEvent::MessageUpdated { conv_id: m.conv_id.clone(), message: self.msg_item(&m) });
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
