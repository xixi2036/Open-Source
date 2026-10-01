//! 通话信令（经 Olm）与通话记录。

use super::{direct_conv, Inner};
use crate::content::{Content, ContentBody};
use crate::error::Result;
use crate::store::{MsgRow, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::now_ms;

/// offer 超过这么久才送达就不再响铃
pub const OFFER_MAX_AGE_MS: i64 = 60_000;
/// 通话跟踪最长保留时间（超过即视为已结束，不再写通话记录）
pub const CALL_TRACK_MAX_AGE_MS: i64 = 2 * 3600 * 1000;

/// 通话结束原因（C-B）。`action` 是结束通话的信令，`local` 表示它由本端发出。
pub fn end_reason(t: &CallTrack, action: CallAction, _local: bool) -> CallEndReason {
    if t.answered_at.is_some() {
        return CallEndReason::Completed;
    }
    match action {
        // 呼出时对方忙线；呼入时本端忙线自动回 busy，对用户而言是一个未接来电
        CallAction::Busy => {
            if t.outgoing {
                CallEndReason::Busy
            } else {
                CallEndReason::Missed
            }
        }
        CallAction::Reject => CallEndReason::Rejected,
        // 明确拒接只走 Reject 信令；接通前的 Hangup 都是取消或响铃超时
        _ => {
            if t.outgoing {
                // 主叫取消，或被叫响铃超时自动挂断：对主叫而言都是未接通的呼叫
                CallEndReason::Cancelled
            } else {
                // 主叫取消，或本端响铃超时：未接来电
                CallEndReason::Missed
            }
        }
    }
}

pub fn is_stale_offer(signal: &CallSignal, sent_ts: i64, now: i64) -> bool {
    signal.action == CallAction::Offer && sent_ts > 0 && now - sent_ts > OFFER_MAX_AGE_MS
}

#[derive(Debug, Clone)]
pub struct CallTrack {
    pub peer: String,
    pub outgoing: bool,
    pub video: bool,
    pub started_at: i64,
    pub answered_at: Option<i64>,
}

impl Inner {
    pub(crate) async fn send_call_signal(&self, uid: &str, signal: CallSignal) -> Result<()> {
        let urgent = !matches!(signal.action, CallAction::Ice);
        let c = Content::new(
            ContentBody::Call {
                call_id: signal.call_id.clone(),
                action: Some(signal.action),
                video: signal.video,
                sdp: signal.sdp.clone(),
                candidates: if signal.candidates.is_empty() { None } else { Some(signal.candidates.clone()) },
            },
            None,
            0,
        );
        if let Err(e) = self.send_olm(uid, c, urgent).await {
            // 呼叫信令都没发出去：记一条「通话失败」
            if signal.action == CallAction::Offer {
                let now = now_ms();
                let t = CallTrack { peer: uid.to_string(), outgoing: true, video: signal.video, started_at: now, answered_at: None };
                if let Err(e2) = self.write_call_log(&t, CallEndReason::Failed, now) {
                    tracing::warn!("写入通话失败记录失败: {e2}");
                }
            }
            return Err(e);
        }
        self.track_call(uid, &signal, true)?;
        Ok(())
    }

    /// 收到通话信令。`sent_ts` 为服务端时间戳（缺失时用发送方时间）。
    /// 超过 60 秒的 offer（例如离线期间积压、重连后才送达）不再触发来电事件，只记录通话（对方挂断后记为未接来电）。
    pub(crate) fn on_call_signal(&self, from: &str, signal: CallSignal, sent_ts: i64) -> Result<()> {
        self.track_call(from, &signal, false)?;
        if is_stale_offer(&signal, sent_ts, now_ms()) {
            tracing::info!("忽略过期的来电 offer（{} 秒前）call_id={}", (now_ms() - sent_ts) / 1000, signal.call_id);
            return Ok(());
        }
        self.emit(CoreEvent::CallSignalReceived { from_user_id: from.to_string(), signal });
        Ok(())
    }

    /// 维护通话状态；结束时在会话中写入 CallLog。`local` 表示信令由本端发出。
    fn track_call(&self, peer: &str, s: &CallSignal, local: bool) -> Result<()> {
        let now = now_ms();
        let finished = {
            let mut calls = self.calls.lock().unwrap_or_else(|e| e.into_inner());
            match s.action {
                CallAction::Offer => {
                    calls.entry(s.call_id.clone()).or_insert(CallTrack {
                        peer: peer.to_string(),
                        outgoing: local,
                        video: s.video,
                        started_at: now,
                        answered_at: None,
                    });
                    None
                }
                // 只有通话的对端才能改变该通话的状态：先核对 peer，第三方知道（或撞上）call_id 也无法结束别人的通话
                CallAction::Answer => {
                    if let Some(t) = calls.get_mut(&s.call_id).filter(|t| t.peer == peer) {
                        t.answered_at.get_or_insert(now);
                    }
                    None
                }
                CallAction::Ice => None,
                CallAction::Hangup | CallAction::Busy | CallAction::Reject => {
                    if calls.get(&s.call_id).is_some_and(|t| t.peer == peer) {
                        calls.remove(&s.call_id).map(|t| {
                            let reason = end_reason(&t, s.action, local);
                            (t, reason)
                        })
                    } else {
                        None
                    }
                }
            }
        };
        if let Some((t, reason)) = finished {
            self.write_call_log(&t, reason, now)?;
        }
        Ok(())
    }

    /// 清理开始超过 `CALL_TRACK_MAX_AGE_MS` 仍未收到结束信令的通话跟踪（对方崩溃等），避免无限增长。
    pub(crate) fn prune_stale_calls(&self, now: i64) {
        let mut calls = self.calls.lock().unwrap_or_else(|e| e.into_inner());
        calls.retain(|_, t| now - t.started_at < CALL_TRACK_MAX_AGE_MS);
    }

    fn write_call_log(&self, t: &CallTrack, reason: CallEndReason, now: i64) -> Result<()> {
        let conv = direct_conv(&t.peer);
        self.store.ensure_conv(&conv, 0)?;
        let me = self.my_id()?;
        let duration = t.answered_at.map(|a| ((now - a).max(0) / 1000) as u32).unwrap_or(0);
        // missed 与 reason 保持一致：只有未接来电计未读（本端拒接的来电不再算未接）
        let missed = reason == CallEndReason::Missed;
        let mut row = MsgRow {
            id: crate::util::new_id(),
            conv_id: conv.clone(),
            sender_id: if t.outgoing { me } else { t.peer.clone() },
            outgoing: t.outgoing,
            sent_at: t.started_at,
            server_ts: 0,
            sort_ts: now,
            status: if t.outgoing { MessageStatus::Sent } else { MessageStatus::Read },
            msg: StoredMessage::plain(
                StoredBody::CallLog { video: t.video, duration_secs: duration, missed, outgoing: t.outgoing, reason: Some(reason) },
                None,
            ),
            expire_secs: 0,
            expires_at: None,
            deleted: false,
            read: !missed,
            mentions_me: false,
        };
        self.store.insert_message(&mut row)?;
        self.store.bump_conv(&conv, row.sort_ts, if missed { 1 } else { 0 })?;
        self.emit_added(&row, false);
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::test_inner;
    use crate::content::Content;
    use std::sync::{mpsc, Arc, Mutex};
    use std::time::Duration;

    struct Tx(Mutex<mpsc::Sender<CoreEvent>>);
    impl EventListener for Tx {
        fn on_event(&self, e: CoreEvent) {
            let _ = self.0.lock().unwrap().send(e);
        }
    }

    fn offer(call_id: &str) -> ContentBody {
        ContentBody::Call { call_id: call_id.into(), action: Some(CallAction::Offer), video: false, sdp: Some("v=0".into()), candidates: None }
    }

    #[tokio::test]
    async fn stale_offer_does_not_ring() {
        let (inner, _dir) = test_inner();
        let (tx, rx) = mpsc::channel();
        inner.set_listener(Arc::new(Tx(Mutex::new(tx))));
        let inc = |call: &str, server_ts: i64| super::super::secure::Incoming {
            from: "alice".into(),
            content: Content::new(offer(call), None, 0),
            server_ts,
            via_group: None,
            olm_session_id: Some("s".into()),
            backlog: false,
        };
        inner.handle_incoming(inc("old", now_ms() - 5 * 60_000)).await.unwrap();
        inner.handle_incoming(inc("fresh", now_ms() - 1_000)).await.unwrap();
        let mut rang = vec![];
        while let Ok(e) = rx.recv_timeout(Duration::from_millis(500)) {
            if let CoreEvent::CallSignalReceived { signal, .. } = e {
                rang.push(signal.call_id);
            }
        }
        assert_eq!(rang, vec!["fresh".to_string()], "过期 offer 不应触发来电");
        // 过期 offer 仍被跟踪：对方挂断后记为未接来电
        assert!(inner.calls.lock().unwrap().contains_key("old"));
    }

    fn sig(call_id: &str, action: CallAction) -> CallSignal {
        CallSignal { call_id: call_id.into(), action, video: false, sdp: None, candidates: vec![] }
    }

    fn call_logs(inner: &Inner, peer: &str) -> usize {
        inner
            .store
            .messages(&direct_conv(peer), None, 100)
            .unwrap()
            .iter()
            .filter(|m| matches!(m.msg.body, StoredBody::CallLog { .. }))
            .count()
    }

    /// 第三方用同一个 call_id 发挂断：不能结束 alice 的通话；alice 自己挂断才写通话记录。
    #[tokio::test]
    async fn hangup_from_other_peer_ignored() {
        let (inner, _dir) = test_inner();
        inner.on_call_signal("alice", sig("c1", CallAction::Offer), now_ms()).unwrap();
        inner.on_call_signal("mallory", sig("c1", CallAction::Answer), now_ms()).unwrap();
        inner.on_call_signal("mallory", sig("c1", CallAction::Hangup), now_ms()).unwrap();
        {
            let calls = inner.calls.lock().unwrap();
            let t = calls.get("c1").expect("第三方挂断不能移除跟踪");
            assert!(t.answered_at.is_none(), "第三方不能把通话标记为已接听");
        }
        assert_eq!(call_logs(&inner, "mallory"), 0);
        inner.on_call_signal("alice", sig("c1", CallAction::Hangup), now_ms()).unwrap();
        assert!(!inner.calls.lock().unwrap().contains_key("c1"));
        assert_eq!(call_logs(&inner, "alice"), 1, "对端挂断写入一条通话记录");
    }

    #[tokio::test]
    async fn stale_call_tracks_pruned() {
        let (inner, _dir) = test_inner();
        inner.on_call_signal("alice", sig("old", CallAction::Offer), now_ms()).unwrap();
        inner.on_call_signal("alice", sig("new", CallAction::Offer), now_ms()).unwrap();
        inner.calls.lock().unwrap().get_mut("old").unwrap().started_at -= CALL_TRACK_MAX_AGE_MS + 1;
        inner.prune_stale_calls(now_ms());
        let calls = inner.calls.lock().unwrap();
        assert!(!calls.contains_key("old"));
        assert!(calls.contains_key("new"));
    }

    fn reasons(inner: &Inner, peer: &str) -> Vec<(CallEndReason, bool)> {
        inner
            .store
            .messages(&direct_conv(peer), None, 100)
            .unwrap()
            .iter()
            .filter_map(|m| match m.msg.body.to_api() {
                MessageBody::CallLog { reason, missed, .. } => Some((reason, missed)),
                _ => None,
            })
            .collect()
    }

    /// 各种结束方式写入的结束原因（C-B）；missed 与 reason 一致。
    #[tokio::test]
    async fn call_log_reasons() {
        let (inner, _dir) = test_inner();
        let now = now_ms();
        // 呼入：对方接通后挂断 → Completed
        inner.on_call_signal("a", sig("c1", CallAction::Offer), now).unwrap();
        inner.track_call("a", &sig("c1", CallAction::Answer), true).unwrap();
        inner.on_call_signal("a", sig("c1", CallAction::Hangup), now).unwrap();
        // 呼入：对方取消 → Missed
        inner.on_call_signal("b", sig("c2", CallAction::Offer), now).unwrap();
        inner.on_call_signal("b", sig("c2", CallAction::Hangup), now).unwrap();
        // 呼入：本端拒接 → Rejected（不再算未接）
        inner.on_call_signal("c", sig("c3", CallAction::Offer), now).unwrap();
        inner.track_call("c", &sig("c3", CallAction::Reject), true).unwrap();
        // 呼出：对方忙线 → Busy
        inner.track_call("d", &sig("c4", CallAction::Offer), true).unwrap();
        inner.on_call_signal("d", sig("c4", CallAction::Busy), now).unwrap();
        // 呼出：接通前本端挂断 → Cancelled
        inner.track_call("e", &sig("c5", CallAction::Offer), true).unwrap();
        inner.track_call("e", &sig("c5", CallAction::Hangup), true).unwrap();
        // 呼出：对方拒接 → Rejected
        inner.track_call("f", &sig("c6", CallAction::Offer), true).unwrap();
        inner.on_call_signal("f", sig("c6", CallAction::Reject), now).unwrap();
        // 呼入：本端忙线 → Missed
        inner.on_call_signal("g", sig("c7", CallAction::Offer), now).unwrap();
        inner.track_call("g", &sig("c7", CallAction::Busy), true).unwrap();
        assert_eq!(reasons(&inner, "a"), vec![(CallEndReason::Completed, false)]);
        assert_eq!(reasons(&inner, "b"), vec![(CallEndReason::Missed, true)]);
        assert_eq!(reasons(&inner, "c"), vec![(CallEndReason::Rejected, false)]);
        assert_eq!(reasons(&inner, "d"), vec![(CallEndReason::Busy, false)]);
        assert_eq!(reasons(&inner, "e"), vec![(CallEndReason::Cancelled, false)]);
        assert_eq!(reasons(&inner, "f"), vec![(CallEndReason::Rejected, false)]);
        assert_eq!(reasons(&inner, "g"), vec![(CallEndReason::Missed, true)]);
        // 呼入：本端响铃超时自动挂断 → Missed（不是 Rejected）
        inner.on_call_signal("h", sig("c8", CallAction::Offer), now).unwrap();
        inner.track_call("h", &sig("c8", CallAction::Hangup), true).unwrap();
        // 呼出：被叫响铃超时自动挂断 → Cancelled
        inner.track_call("i", &sig("c9", CallAction::Offer), true).unwrap();
        inner.on_call_signal("i", sig("c9", CallAction::Hangup), now).unwrap();
        assert_eq!(reasons(&inner, "h"), vec![(CallEndReason::Missed, true)]);
        assert_eq!(reasons(&inner, "i"), vec![(CallEndReason::Cancelled, false)]);
        assert_eq!(inner.store.get_conv("u:h").unwrap().unwrap().unread, 1);
        assert_eq!(inner.store.get_conv("u:b").unwrap().unwrap().unread, 1);
        assert_eq!(inner.store.get_conv("u:c").unwrap().unwrap().unread, 0);
    }

    /// 呼叫信令发送失败：记一条 Failed 通话记录。
    #[tokio::test]
    async fn offer_send_failure_logs_failed() {
        let (inner, _dir) = test_inner(); // 服务器不可达
        assert!(inner.send_call_signal("h", sig("c8", CallAction::Offer)).await.is_err());
        assert_eq!(reasons(&inner, "h"), vec![(CallEndReason::Failed, false)]);
        assert!(!inner.calls.lock().unwrap().contains_key("c8"));
    }

    #[test]
    fn stale_offer_rule() {
        let s = |a| CallSignal { call_id: "c".into(), action: a, video: false, sdp: None, candidates: vec![] };
        assert!(is_stale_offer(&s(CallAction::Offer), 1, 1 + OFFER_MAX_AGE_MS + 1));
        assert!(!is_stale_offer(&s(CallAction::Offer), 1, 1 + OFFER_MAX_AGE_MS));
        assert!(!is_stale_offer(&s(CallAction::Hangup), 1, 1 + 10 * OFFER_MAX_AGE_MS), "只针对 offer");
        assert!(!is_stale_offer(&s(CallAction::Offer), 0, 10 * OFFER_MAX_AGE_MS), "时间未知时不判过期");
    }
}
