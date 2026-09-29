//! 会话消息的发送侧：文字、重发、回应、撤回、已读回执、输入状态、阅后即焚设置、会话标记。
//! 接收处理见 `receive.rs`，附件见 `attachments.rs`。

use std::collections::HashMap;

use super::{parse_conv, timer_key, ConvRef, Inner};
use crate::content::{valid_mentions, Content, ContentBody, ReceiptKind, TypingAction, WireQuote};
use crate::error::{HushError, Result};
use crate::store::{MsgRow, StoredAttachment, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::now_ms;

pub(super) const DELETE_WINDOW_MS: i64 = 24 * 3600 * 1000;
/// 撤回时间校验的时钟容差
pub(super) const CLOCK_SKEW_MS: i64 = 5 * 60 * 1000;

fn quote_text(body: &StoredBody) -> String {
    let t = body.preview_text();
    if t.chars().count() > 200 {
        t.chars().take(200).collect()
    } else {
        t
    }
}

pub fn describe_timer(secs: u32) -> String {
    match secs {
        0 => "关闭".into(),
        s if s % 86400 == 0 => format!("{} 天", s / 86400),
        s if s % 3600 == 0 => format!("{} 小时", s / 3600),
        s if s % 60 == 0 => format!("{} 分钟", s / 60),
        s => format!("{s} 秒"),
    }
}

impl Inner {
    // ================= 发送 =================

    /// 把内容发往会话（单聊走 Olm，群聊走 Megolm）。
    pub(crate) async fn deliver(&self, conv_id: &str, content: Content, urgent: bool) -> Result<i64> {
        match parse_conv(conv_id)? {
            ConvRef::Direct(uid) => self.send_olm(uid, content, urgent).await,
            ConvRef::Group(gid) => self.send_group(gid, content, urgent).await,
        }
    }

    pub(super) fn group_of(conv_id: &str) -> Option<String> {
        conv_id.strip_prefix("g:").map(|s| s.to_string())
    }

    fn build_quote(&self, conv_id: &str, quote_id: Option<String>) -> Result<Option<Quote>> {
        let Some(qid) = quote_id.filter(|q| !q.is_empty()) else {
            return Ok(None);
        };
        let q = self.store.get_message(&qid)?.ok_or_else(|| HushError::not_found(format!("被引用消息 {qid}")))?;
        if q.conv_id != conv_id {
            return Err(HushError::invalid("只能引用同一会话内的消息"));
        }
        let author = if q.outgoing { self.my_id()? } else { q.sender_id.clone() };
        Ok(Some(Quote { id: q.content_id().to_string(), author, text: quote_text(&q.msg.body) }))
    }

    /// 插入一条待发送的本地消息。
    pub(super) fn insert_outgoing(&self, conv_id: &str, content: &Content, msg: StoredMessage) -> Result<MsgRow> {
        let me = self.my_id()?;
        let mut row = MsgRow {
            id: content.id.clone(),
            conv_id: conv_id.to_string(),
            sender_id: me,
            outgoing: true,
            sent_at: content.ts,
            server_ts: 0,
            sort_ts: content.ts,
            status: MessageStatus::Sending,
            msg,
            expire_secs: content.expire_secs,
            // 发送方发送即开始计时
            expires_at: (content.expire_secs > 0).then(|| content.ts + content.expire_secs as i64 * 1000),
            deleted: false,
            read: true,
            mentions_me: false,
        };
        self.store.insert_message(&mut row)?;
        self.store.bump_conv(conv_id, row.sort_ts, 0)?;
        self.emit_added(&row, false);
        self.emit(CoreEvent::ConversationsChanged);
        Ok(row)
    }

    /// 发送并更新状态。服务端接受后（K3）把排序时间改为服务端返回的 server_ts（单聊取 `/v1/messages` 结果，
    /// 群聊取 `/v1/groups/{gid}/messages` 结果），双方的消息顺序都以服务端时间为准；失败的保持本地时间。
    pub(super) async fn finish_send(&self, mut row: MsgRow, content: Content) -> Result<MessageItem> {
        let conv_id = row.conv_id.clone();
        let res = self.deliver(&conv_id, content, true).await;
        match &res {
            Ok(ts) => {
                // 只改 server_ts / sort_ts / 状态这几列：期间可能已收到回执（状态更高），不能被整行写回覆盖
                if let Some(sort_ts) = self.store.mark_sent(&row.id, *ts)? {
                    self.store.bump_conv(&conv_id, sort_ts, 0)?;
                }
                if let Some(cur) = self.store.get_message(&row.id)? {
                    row = cur;
                }
            }
            Err(_) => {
                // 期间可能已收到回执，重新读取
                if let Some(cur) = self.store.get_message(&row.id)? {
                    row = cur;
                }
                row.status = MessageStatus::Failed;
                self.store.update_message(&row)?;
            }
        }
        let item = self.msg_item(&row);
        self.emit(CoreEvent::MessageUpdated { conv_id: conv_id.clone(), message: item.clone() });
        if res.is_ok() {
            // 排序时间可能变了：会话列表的最后一条 / 时间随之更新
            self.emit(CoreEvent::ConversationsChanged);
        }
        res.map(|_| item)
    }

    pub(super) fn content_for(&self, row: &MsgRow) -> Result<Content> {
        let quote = row.msg.quote.as_ref().map(WireQuote::from);
        let body = match &row.msg.body {
            StoredBody::Text { text } => ContentBody::Text { text: text.clone(), quote, mentions: row.msg.mentions.clone() },
            StoredBody::Attachments { items, caption } => {
                if items.iter().any(|a| a.wire.id.is_empty()) {
                    return Err(HushError::invalid("附件尚未上传"));
                }
                ContentBody::Attachment { attachments: items.iter().map(|a| a.wire.clone()).collect(), caption: caption.clone(), quote }
            }
            _ => return Err(HushError::invalid("该消息不可重发")),
        };
        Ok(Content {
            v: crate::content::CONTENT_VERSION,
            id: row.content_id().to_string(),
            ts: row.sent_at,
            group_id: Self::group_of(&row.conv_id),
            expire_secs: row.expire_secs,
            expire_set_at: self.timer_set_at(&row.conv_id),
            forwarded: row.msg.forwarded,
            body,
        })
    }

    /// 会话阅后即焚设置最后一次变更的时间（0 = 未知 / 从未显式设置）。
    pub(crate) fn timer_set_at(&self, conv_id: &str) -> i64 {
        self.store.kv_get_json::<i64>(&timer_key(conv_id)).ok().flatten().unwrap_or(0)
    }

    pub(super) fn set_timer(&self, conv_id: &str, secs: u32, at: i64) -> Result<()> {
        self.store.set_conv_expire(conv_id, secs)?;
        self.store.kv_set_json(&timer_key(conv_id), &at)
    }

    pub(crate) async fn send_text(&self, conv_id: &str, text: String, quote_id: Option<String>) -> Result<MessageItem> {
        self.send_text_with_mentions(conv_id, text, quote_id, vec![]).await
    }

    /// 发送文字（K6：可带 @提及）。区间不合法（越界、长度 0、user_id 为空）的提及直接丢弃，不报错。
    /// 发送即清空该会话的草稿（K5）。
    pub(crate) async fn send_text_with_mentions(
        &self,
        conv_id: &str,
        text: String,
        quote_id: Option<String>,
        mentions: Vec<Mention>,
    ) -> Result<MessageItem> {
        if text.trim().is_empty() {
            return Err(HushError::invalid("消息为空"));
        }
        let conv = self.ensure_conv(conv_id)?;
        let quote = self.build_quote(conv_id, quote_id)?;
        let wq = quote.as_ref().map(WireQuote::from);
        let mentions = valid_mentions(&text, mentions);
        let body = ContentBody::Text { text: text.clone(), quote: wq, mentions: mentions.clone() };
        let mut content = Content::new(body, Self::group_of(conv_id), conv.expire_secs);
        content.expire_set_at = self.timer_set_at(conv_id);
        let msg = StoredMessage { body: StoredBody::Text { text }, quote, forwarded: false, mentions };
        self.clear_draft_on_send(conv_id);
        let row = self.insert_outgoing(conv_id, &content, msg)?;
        self.finish_send(row, content).await
    }

    /// 用户在会话里发送消息时清空草稿（K5）。失败只记日志。
    pub(super) fn clear_draft_on_send(&self, conv_id: &str) {
        if let Err(e) = self.store.set_draft(conv_id, None, now_ms()) {
            tracing::warn!("清除草稿失败: {e}");
        }
    }

    /// 保存 / 清除草稿（K5）。`text` 为 None 或空白时清除；会话不存在时创建（有草稿的会话出现在列表中）。
    pub(crate) fn set_draft(&self, conv_id: &str, text: Option<String>) -> Result<()> {
        let has_text = text.as_deref().is_some_and(|t| !t.trim().is_empty());
        if has_text {
            self.ensure_conv(conv_id)?;
        } else {
            parse_conv(conv_id)?;
        }
        if self.store.set_draft(conv_id, text.as_deref(), now_ms())? {
            self.emit(CoreEvent::ConversationsChanged);
        }
        Ok(())
    }

    /// 转发（K4）：把文字或附件消息作为新消息发到多个会话。附件复用原消息的密文 id、密钥与 digest
    /// （不重新加密、不重新上传）；引用、回应、@提及不转发。按 `conv_ids`（去重后）的顺序返回新消息；
    /// 单个目标发送失败不影响其他目标，那一条的状态为 failed（可 `resend`）。
    pub(crate) async fn forward_message(&self, message_id: &str, conv_ids: Vec<String>) -> Result<Vec<MessageItem>> {
        let src = self.store.get_message(message_id)?.ok_or_else(|| HushError::not_found("消息"))?;
        if src.deleted {
            return Err(HushError::invalid("该消息不可转发"));
        }
        match &src.msg.body {
            StoredBody::Text { .. } => {}
            StoredBody::Attachments { items, .. } if !items.is_empty() && items.iter().all(|a| !a.wire.id.is_empty()) => {}
            StoredBody::Attachments { .. } => return Err(HushError::invalid("附件尚未上传，不能转发")),
            _ => return Err(HushError::invalid("该消息不可转发")),
        }
        let mut targets: Vec<String> = Vec::new();
        for c in conv_ids {
            parse_conv(&c)?;
            if !targets.contains(&c) {
                targets.push(c);
            }
        }
        self.my_id()?;
        // 先在本地插入全部新消息（发送中），再依次发送
        let mut pending = Vec::with_capacity(targets.len());
        for conv_id in &targets {
            let conv = self.ensure_conv(conv_id)?;
            let mut content = Content::new(ContentBody::Text { text: String::new(), quote: None, mentions: vec![] }, Self::group_of(conv_id), conv.expire_secs);
            content.expire_set_at = self.timer_set_at(conv_id);
            content.forwarded = true;
            let body = match &src.msg.body {
                StoredBody::Text { text } => {
                    content.body = ContentBody::Text { text: text.clone(), quote: None, mentions: vec![] };
                    StoredBody::Text { text: text.clone() }
                }
                StoredBody::Attachments { items, caption } => {
                    let items: Vec<StoredAttachment> = items
                        .iter()
                        .enumerate()
                        .map(|(i, a)| StoredAttachment {
                            wire: a.wire.clone(),
                            local_path: self.link_local_copy(a.local_path.as_deref(), &content.id, i as u32, &a.wire.file_name),
                        })
                        .collect();
                    content.body = ContentBody::Attachment {
                        attachments: items.iter().map(|a| a.wire.clone()).collect(),
                        caption: caption.clone(),
                        quote: None,
                    };
                    StoredBody::Attachments { items, caption: caption.clone() }
                }
                _ => unreachable!("上面已检查"),
            };
            let msg = StoredMessage { body, quote: None, forwarded: true, mentions: vec![] };
            let row = self.insert_outgoing(conv_id, &content, msg)?;
            pending.push((row, content));
        }
        let mut out = Vec::with_capacity(pending.len());
        for (row, content) in pending {
            let id = row.id.clone();
            match self.finish_send(row, content).await {
                Ok(item) => out.push(item),
                Err(e) => {
                    tracing::warn!("转发到 {} 失败: {e}", id);
                    let row = self.store.get_message(&id)?.ok_or_else(|| HushError::internal("转发的消息丢失"))?;
                    out.push(self.msg_item(&row));
                }
            }
        }
        Ok(out)
    }

    /// 转发附件时给新消息准备本地明文副本：原消息已有本地文件（在 files/ 下）时硬链接（失败则复制）到新消息自己的
    /// 缓存路径——不共用同一路径，否则删除其中一条消息会删掉另一条的文件。没有本地文件时返回 None（需要时再下载）。
    fn link_local_copy(&self, src: Option<&str>, new_id: &str, index: u32, file_name: &str) -> Option<String> {
        let src = std::path::Path::new(src?);
        if !src.starts_with(&self.files_dir) || !src.exists() {
            return None;
        }
        let dst = self.attachment_path(new_id, index, file_name);
        let ok = std::fs::hard_link(src, &dst).is_ok() || std::fs::copy(src, &dst).is_ok();
        ok.then(|| dst.to_string_lossy().to_string())
    }

    /// 删除会话（K1）：本地消息、附件缓存文件、草稿与会话行。群会话不退群，群里有新消息时会话重新出现。
    pub(crate) fn delete_conversation(&self, conv_id: &str) -> Result<()> {
        parse_conv(conv_id)?;
        for m in self.store.messages(conv_id, None, u32::MAX)? {
            self.remove_local_files(&m);
        }
        self.store.delete_conversation(conv_id)?;
        self.store.kv_del(&timer_key(conv_id))?;
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }

    /// 重发失败的消息（文字 / 已上传的附件）。
    pub(crate) async fn resend(&self, message_id: &str) -> Result<MessageItem> {
        let mut row = self.store.get_message(message_id)?.ok_or_else(|| HushError::not_found("消息"))?;
        if !row.outgoing || row.status != MessageStatus::Failed {
            return Err(HushError::invalid("只能重发发送失败的消息"));
        }
        let content = match self.content_for(&row) {
            Ok(c) => c,
            Err(_) if matches!(row.msg.body, StoredBody::Attachments { .. }) => {
                return self.upload_and_send(row).await;
            }
            Err(e) => return Err(e),
        };
        row.status = MessageStatus::Sending;
        self.store.update_message(&row)?;
        self.emit(CoreEvent::MessageUpdated { conv_id: row.conv_id.clone(), message: self.msg_item(&row) });
        self.finish_send(row, content).await
    }

    pub(crate) async fn react(&self, message_id: &str, emoji: String, remove: bool) -> Result<()> {
        let row = self.store.get_message(message_id)?.ok_or_else(|| HushError::not_found("消息"))?;
        if !matches!(row.msg.body, StoredBody::Text { .. } | StoredBody::Attachments { .. }) || row.deleted {
            return Err(HushError::invalid("该消息不可回应"));
        }
        let me = self.my_id()?;
        let author = if row.outgoing { me.clone() } else { row.sender_id.clone() };
        if remove {
            self.store.remove_reaction(message_id, &me, &emoji)?;
        } else {
            self.store.set_reaction(message_id, &me, &emoji)?;
        }
        self.emit(CoreEvent::MessageUpdated { conv_id: row.conv_id.clone(), message: self.msg_item(&row) });
        let c = Content::new(
            ContentBody::Reaction { target_id: row.content_id().to_string(), target_author: author, emoji, remove },
            Self::group_of(&row.conv_id),
            0,
        );
        self.deliver(&row.conv_id, c, true).await?;
        Ok(())
    }

    pub(super) fn mark_deleted(&self, row: &mut MsgRow) -> Result<()> {
        self.remove_local_files(row);
        row.deleted = true;
        row.msg = StoredMessage::plain(StoredBody::System { text: "此消息已撤回".into() }, None);
        self.store.update_message(row)?;
        self.emit(CoreEvent::MessageUpdated { conv_id: row.conv_id.clone(), message: self.msg_item(row) });
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }

    pub(crate) async fn delete_for_everyone(&self, message_id: &str) -> Result<()> {
        let mut row = self.store.get_message(message_id)?.ok_or_else(|| HushError::not_found("消息"))?;
        if !row.outgoing || row.deleted {
            return Err(HushError::invalid("只能撤回自己发送的消息"));
        }
        if now_ms() - row.sent_at > DELETE_WINDOW_MS {
            return Err(HushError::invalid("超过 24 小时，无法撤回"));
        }
        let c = Content::new(ContentBody::Delete { target_id: row.content_id().to_string() }, Self::group_of(&row.conv_id), 0);
        self.deliver(&row.conv_id, c, true).await?;
        self.mark_deleted(&mut row)
    }

    pub(crate) fn delete_local(&self, message_id: &str) -> Result<()> {
        let row = self.store.get_message(message_id)?.ok_or_else(|| HushError::not_found("消息"))?;
        self.remove_local_files(&row);
        self.store.delete_message(message_id)?;
        self.emit(CoreEvent::MessageRemoved { conv_id: row.conv_id, message_id: message_id.to_string() });
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }

    pub(crate) fn clear_conversation(&self, conv_id: &str) -> Result<()> {
        for m in self.store.messages(conv_id, None, u32::MAX)? {
            self.remove_local_files(&m);
        }
        for id in self.store.clear_conversation(conv_id)? {
            self.emit(CoreEvent::MessageRemoved { conv_id: conv_id.to_string(), message_id: id });
        }
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }

    pub(crate) async fn mark_read(&self, conv_id: &str) -> Result<()> {
        let now = now_ms();
        let unread = self.store.unread_incoming(conv_id)?;
        let mut by_sender: HashMap<String, Vec<String>> = HashMap::new();
        for mut m in unread {
            m.read = true;
            m.status = MessageStatus::Read;
            if m.expire_secs > 0 && m.expires_at.is_none() {
                // 接收方已读开始计时
                m.expires_at = Some(now + m.expire_secs as i64 * 1000);
            }
            self.store.update_message(&m)?;
            let is_real = matches!(m.msg.body, StoredBody::Text { .. } | StoredBody::Attachments { .. }) && !m.sender_id.is_empty();
            if is_real {
                by_sender.entry(m.sender_id.clone()).or_default().push(m.content_id().to_string());
            }
            self.emit(CoreEvent::MessageUpdated { conv_id: conv_id.to_string(), message: self.msg_item(&m) });
        }
        self.store.clear_unread(conv_id)?;
        self.emit(CoreEvent::ConversationsChanged);
        if self.settings().read_receipts && !by_sender.is_empty() {
            let group = Self::group_of(conv_id);
            let items: Vec<(String, Content, bool)> = by_sender
                .into_iter()
                .map(|(uid, ids)| (uid, Content::new(ContentBody::Receipt { kind: ReceiptKind::Read, ids }, group.clone(), 0), false))
                .collect();
            for r in self.send_olm_batch(items).await? {
                if let Err(e) = r {
                    tracing::warn!("已读回执发送失败: {e}");
                }
            }
        }
        Ok(())
    }

    pub(crate) async fn set_typing(&self, conv_id: &str, typing: bool) -> Result<()> {
        if !self.settings().typing_indicators {
            return Ok(());
        }
        let action = if typing { TypingAction::Started } else { TypingAction::Stopped };
        let c = Content::new(ContentBody::Typing { action }, Self::group_of(conv_id), 0);
        self.deliver(conv_id, c, false).await?;
        Ok(())
    }

    pub(crate) async fn set_disappearing(&self, conv_id: &str, secs: u32) -> Result<()> {
        self.ensure_conv(conv_id)?;
        let mut c = Content::new(ContentBody::Timer { expire_secs: secs }, Self::group_of(conv_id), secs);
        c.expire_set_at = c.ts;
        let at = c.ts;
        self.deliver(conv_id, c, true).await?;
        self.set_timer(conv_id, secs, at)?;
        self.insert_system(conv_id, &format!("你将阅后即焚设置为：{}", describe_timer(secs)))?;
        Ok(())
    }

    pub(crate) fn set_conv_flag(&self, conv_id: &str, flag: &str, v: bool) -> Result<()> {
        self.ensure_conv(conv_id)?;
        self.store.set_conv_flag(conv_id, flag, v)?;
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::client::secure::Incoming;
    use crate::client::test_inner;

    fn text(t: &str) -> ContentBody {
        ContentBody::Text { text: t.into(), quote: None, mentions: vec![] }
    }

    /// 进程在发送途中被杀：重启后「发送中」的消息变为「发送失败」，可以重发。
    #[tokio::test]
    async fn interrupted_send_becomes_failed_after_restart() {
        let (inner, dir) = test_inner();
        let c = Content::new(text("半路被杀"), None, 0);
        let row = inner
            .insert_outgoing("u:bob", &c, StoredMessage::plain(StoredBody::Text { text: "半路被杀".into() }, None))
            .unwrap();
        assert_eq!(row.status, MessageStatus::Sending);
        drop(inner);
        let inner = Arc::new(
            Inner::new(dir.path().to_string_lossy().to_string(), vec![7u8; 32], "test".into(), "t".into()).unwrap(),
        );
        assert_eq!(inner.store.get_message(&c.id).unwrap().unwrap().status, MessageStatus::Failed);
        // resend 接受它（离线服务器导致再次失败，但不是「只能重发发送失败的消息」）
        let e = inner.resend(&c.id).await.unwrap_err();
        assert!(!matches!(e, HushError::InvalidInput { .. }), "{e}");
    }

    struct Tx(std::sync::Mutex<std::sync::mpsc::Sender<CoreEvent>>);
    impl EventListener for Tx {
        fn on_event(&self, e: CoreEvent) {
            let _ = self.0.lock().unwrap().send(e);
        }
    }

    fn events(inner: &Inner) -> std::sync::mpsc::Receiver<CoreEvent> {
        let (tx, rx) = std::sync::mpsc::channel();
        inner.set_listener(Arc::new(Tx(std::sync::Mutex::new(tx))));
        rx
    }

    fn drain(rx: &std::sync::mpsc::Receiver<CoreEvent>) -> Vec<CoreEvent> {
        let mut v = vec![];
        while let Ok(e) = rx.recv_timeout(std::time::Duration::from_millis(200)) {
            v.push(e);
        }
        v
    }

    fn group_inc(from: &str, gid: &str, id: &str, server_ts: i64, body: ContentBody) -> Incoming {
        let mut c = Content::new(body, Some(gid.into()), 0);
        c.id = id.into();
        Incoming { from: from.into(), content: c, server_ts, via_group: Some(gid.into()), olm_session_id: None, backlog: false }
    }

    fn wire(id: &str) -> crate::content::WireAttachment {
        crate::content::WireAttachment {
            id: id.into(),
            key: "a2V5".into(),
            digest: "ZGlnZXN0".into(),
            size: 5,
            mime: "image/png".into(),
            file_name: "p.png".into(),
            kind: AttachmentKind::Image,
            width: 1,
            height: 1,
            duration_ms: 0,
            thumbnail: None,
            format: 2,
        }
    }

    /// K1：删除群会话 → 消息、附件缓存文件、草稿与会话行都删掉，群信息保留（不退群）；
    /// 群里有新消息时会话重新出现，只有新消息。
    #[tokio::test]
    async fn delete_group_conversation_and_reappear() {
        let (inner, _dir) = test_inner();
        inner.store.put_group(&crate::client::test_group("g1", &["me", "bob"]), true).unwrap();
        let conv = "g:g1";
        inner.handle_incoming(group_inc("bob", "g1", "m1", 10, text("第一条"))).await.unwrap();
        inner
            .handle_incoming(group_inc("bob", "g1", "m2", 20, ContentBody::Attachment { attachments: vec![wire("att")], caption: None, quote: None }))
            .await
            .unwrap();
        // 模拟已下载的附件缓存
        let cached = inner.attachment_path("m2", 0, "p.png");
        std::fs::write(&cached, b"plain").unwrap();
        let mut row = inner.store.get_message("m2").unwrap().unwrap();
        if let StoredBody::Attachments { items, .. } = &mut row.msg.body {
            items[0].local_path = Some(cached.to_string_lossy().to_string());
        }
        inner.store.update_message(&row).unwrap();
        inner.set_draft(conv, Some("没发的话".into())).unwrap();
        let rx = events(&inner);

        inner.delete_conversation(conv).unwrap();
        assert!(inner.conversation(conv).unwrap().is_none());
        assert!(inner.conversations(false).unwrap().iter().all(|c| c.id != conv));
        assert!(inner.store.messages(conv, None, 50).unwrap().is_empty());
        assert!(!cached.exists(), "附件缓存文件一并删除");
        assert!(inner.store.get_draft(conv).unwrap().is_none());
        assert!(inner.store.get_group("g1").unwrap().unwrap().active, "不退群");
        assert!(drain(&rx).iter().any(|e| matches!(e, CoreEvent::ConversationsChanged)));
        // 删除不存在的会话是空操作；非法 id 报错
        inner.delete_conversation(conv).unwrap();
        assert!(inner.delete_conversation("x").is_err());

        inner.handle_incoming(group_inc("bob", "g1", "m3", 30, text("新消息"))).await.unwrap();
        let c = inner.conversation(conv).unwrap().expect("群有新消息时会话重新出现");
        assert_eq!((c.unread_count, c.title.as_str()), (1, "g"));
        let msgs = inner.store.messages(conv, None, 50).unwrap();
        assert_eq!(msgs.iter().map(|m| m.id.as_str()).collect::<Vec<_>>(), vec!["m3"]);
    }

    /// K5：草稿保存在会话摘要里，按草稿时间参与排序；发送消息（即使发送失败）清空草稿。
    #[tokio::test]
    async fn draft_roundtrip_and_cleared_on_send() {
        let (inner, _dir) = test_inner();
        inner.insert_system("u:old", "旧会话").unwrap();
        tokio::time::sleep(std::time::Duration::from_millis(5)).await;
        inner.set_draft("u:bob", Some("写了一半".into())).unwrap();
        let s = inner.conversation("u:bob").unwrap().expect("有草稿的会话出现在列表中");
        assert_eq!(s.draft.as_deref(), Some("写了一半"));
        assert_eq!(inner.conversations(false).unwrap()[0].id, "u:bob", "按草稿时间排在前面");
        assert!(s.last_activity_ts >= inner.conversation("u:old").unwrap().unwrap().last_activity_ts);
        inner.set_draft("u:bob", None).unwrap();
        assert!(inner.conversation("u:bob").unwrap().unwrap().draft.is_none());
        inner.set_draft("u:bob", Some("再写".into())).unwrap();
        // 离线：发送失败，但草稿已清空（发送即清空）
        assert!(inner.send_text("u:bob", "再写".into(), None).await.is_err());
        assert!(inner.conversation("u:bob").unwrap().unwrap().draft.is_none());
        assert!(inner.set_draft("bad", Some("x".into())).is_err());
    }

    /// K6：收到带 @提及的消息——不合法的区间被丢弃；提及我时 `MessageAdded.mentions_me` 为 true、
    /// `unread_mentions` 计数，`mark_read` 清零；撤回的不计。
    #[tokio::test]
    async fn incoming_mentions_counted_and_cleared() {
        let (inner, _dir) = test_inner();
        inner.set_settings(&Settings { read_receipts: false, ..Settings::default() }).unwrap();
        inner.store.put_group(&crate::client::test_group("g1", &["me", "bob", "carol"]), true).unwrap();
        let rx = events(&inner);
        let m = |u: &str, s: u32, l: u32| Mention { user_id: u.into(), start: s, length: l };
        // "😀@我 @carol 看" ：😀 占 2 个 UTF-16 码元
        let t = "😀@我 @carol 看";
        let body = ContentBody::Text { text: t.into(), quote: None, mentions: vec![m("me", 2, 2), m("carol", 5, 6), m("me", 30, 2), m("", 0, 1)] };
        inner.handle_incoming(group_inc("bob", "g1", "m1", 10, body)).await.unwrap();
        inner
            .handle_incoming(group_inc("bob", "g1", "m2", 20, ContentBody::Text { text: "@carol".into(), quote: None, mentions: vec![m("carol", 0, 6)] }))
            .await
            .unwrap();
        inner
            .handle_incoming(group_inc("bob", "g1", "m3", 30, ContentBody::Text { text: "@我".into(), quote: None, mentions: vec![m("me", 0, 2)] }))
            .await
            .unwrap();
        let evs = drain(&rx);
        let flag = |id: &str| {
            evs.iter()
                .find_map(|e| match e {
                    CoreEvent::MessageAdded { message, mentions_me, .. } if message.id == id => Some(*mentions_me),
                    _ => None,
                })
                .unwrap()
        };
        assert!(flag("m1") && !flag("m2") && flag("m3"));
        let item = inner.msg_item(&inner.store.get_message("m1").unwrap().unwrap());
        assert_eq!(item.mentions, vec![m("me", 2, 2), m("carol", 5, 6)], "越界 / 空 user_id 的区间被丢弃");
        let s = inner.conversation("g:g1").unwrap().unwrap();
        assert_eq!((s.unread_count, s.unread_mentions), (3, 2));
        // 对方撤回提及我的一条：不再计数
        inner.handle_incoming(group_inc("bob", "g1", "d1", 40, ContentBody::Delete { target_id: "m3".into() })).await.unwrap();
        assert_eq!(inner.conversation("g:g1").unwrap().unwrap().unread_mentions, 1);
        inner.mark_read("g:g1").await.unwrap();
        let s = inner.conversation("g:g1").unwrap().unwrap();
        assert_eq!((s.unread_count, s.unread_mentions), (0, 0));
    }

    /// K6（发送侧）：不合法的提及区间丢弃、不报错；发出的内容与本地消息都带着合法的提及，重发时保留。
    #[tokio::test]
    async fn send_with_mentions_validates_ranges() {
        let (inner, _dir) = test_inner();
        let m = |u: &str, s: u32, l: u32| Mention { user_id: u.into(), start: s, length: l };
        // 离线：发送失败，但本地消息已插入
        let r = inner.send_text_with_mentions("u:bob", "@Bob 你好".into(), None, vec![m("bob", 0, 4), m("bob", 3, 99)]).await;
        assert!(r.is_err());
        let row = inner.store.messages("u:bob", None, 10).unwrap().pop().unwrap();
        assert_eq!(row.status, MessageStatus::Failed);
        assert_eq!(inner.msg_item(&row).mentions, vec![m("bob", 0, 4)]);
        assert!(!row.mentions_me, "自己发出的不算提及我");
        match inner.content_for(&row).unwrap().body {
            ContentBody::Text { mentions, .. } => assert_eq!(mentions, vec![m("bob", 0, 4)], "重发时保留提及"),
            other => panic!("{other:?}"),
        }
    }

    /// K4：转发附件消息复用原密文（id / key / digest 不变、不重新上传），本地副本用各自的路径；
    /// 引用与提及不转发；带 forwarded 标记（内容与本地消息）；每个目标一条，失败的状态为 failed。
    #[tokio::test]
    async fn forward_reuses_ciphertext_and_marks_forwarded() {
        let (inner, _dir) = test_inner();
        inner.store.put_group(&crate::client::test_group("g1", &["me", "bob"]), true).unwrap();
        inner
            .handle_incoming(group_inc("bob", "g1", "src", 10, ContentBody::Attachment { attachments: vec![wire("att-1")], caption: Some("看图".into()), quote: None }))
            .await
            .unwrap();
        let cached = inner.attachment_path("src", 0, "p.png");
        std::fs::write(&cached, b"plain").unwrap();
        let mut row = inner.store.get_message("src").unwrap().unwrap();
        if let StoredBody::Attachments { items, .. } = &mut row.msg.body {
            items[0].local_path = Some(cached.to_string_lossy().to_string());
        }
        inner.store.update_message(&row).unwrap();

        let out = inner.forward_message("src", vec!["u:carol".into(), "u:dave".into(), "u:carol".into()]).await.unwrap();
        assert_eq!(out.len(), 2, "目标去重");
        assert_eq!(out.iter().map(|m| m.conv_id.as_str()).collect::<Vec<_>>(), vec!["u:carol", "u:dave"]);
        for item in &out {
            assert!(item.forwarded && item.is_outgoing);
            assert_eq!(item.status, MessageStatus::Failed, "离线：每个目标各自失败，不影响其他目标");
            assert_ne!(item.id, "src");
            let r = inner.store.get_message(&item.id).unwrap().unwrap();
            let StoredBody::Attachments { items, caption } = &r.msg.body else { panic!() };
            assert_eq!(caption.as_deref(), Some("看图"));
            assert_eq!(items[0].wire, wire("att-1"), "复用原密文 id / key / digest");
            let p = items[0].local_path.as_ref().expect("有本地副本");
            assert_ne!(p, &cached.to_string_lossy().to_string(), "不与原消息共用缓存路径");
            assert_eq!(std::fs::read(p).unwrap(), b"plain");
            let c = inner.content_for(&r).unwrap();
            assert!(c.forwarded);
            assert!(matches!(&c.body, ContentBody::Attachment { attachments, quote: None, .. } if attachments[0].id == "att-1"));
        }
        // 删除一条转发消息不影响原消息的缓存
        inner.delete_local(&out[0].id).unwrap();
        assert!(cached.exists());

        // 文字：引用与提及不转发
        let m = Mention { user_id: "me".into(), start: 0, length: 2 };
        inner
            .handle_incoming(group_inc("bob", "g1", "t1", 20, ContentBody::Text { text: "@我 好".into(), quote: None, mentions: vec![m] }))
            .await
            .unwrap();
        let out = inner.forward_message("t1", vec!["g:g1".into()]).await.unwrap();
        let r = inner.store.get_message(&out[0].id).unwrap().unwrap();
        assert!(r.msg.mentions.is_empty() && r.msg.quote.is_none() && r.msg.forwarded);
        assert!(matches!(&r.msg.body, StoredBody::Text { text } if text == "@我 好"));

        // 不可转发：系统消息、撤回的、不存在的；非法会话 id
        let sys = inner.insert_system("g:g1", "系统").unwrap();
        assert!(inner.forward_message(&sys.id, vec!["u:carol".into()]).await.is_err());
        assert!(matches!(inner.forward_message("nope", vec!["u:carol".into()]).await, Err(HushError::NotFound { .. })));
        assert!(inner.forward_message("t1", vec!["bad".into()]).await.is_err());
        assert!(inner.forward_message("t1", vec![]).await.unwrap().is_empty());
    }

    /// 设置记录损坏时按隐私优先：不重新打开已读回执与输入状态。
    #[test]
    fn corrupt_settings_fall_back_to_private_defaults() {
        let (inner, _dir) = test_inner();
        assert!(inner.settings().read_receipts, "没有记录时用默认值");
        inner.store.kv_set("settings", b"not encrypted json").unwrap();
        let s = inner.settings();
        assert!(!s.read_receipts && !s.typing_indicators);
    }

}
