//! 事件队列（2026-09-30 审查 C3）：`emit` 只入队，由 `hushcore-events` 线程按顺序在所有内部锁之外投递给平台。
//!
//! - **刷新类事件去抖**：`ConversationsChanged` / `FriendsChanged` / `GroupsChanged` 只是「请重新读取」，不进队列，
//!   只记一个「待刷新」标记。投递规则：队列里没有别的事件时立即投递（单条实时消息没有额外延迟，顺序也和以前一样，
//!   先 `MessageAdded` 后 `ConversationsChanged`）；事件源源不断时（处理积压），同一种刷新事件最多每
//!   [`REFRESH_INTERVAL`] 投递一次，最后一定补投一次。3000 条积压原来是 3000 个 `ConversationsChanged`，现在是几个。
//! - **上限**：待投递事件超过 [`EVENT_QUEUE_CAP`] 时，丢弃最早的一个可丢弃事件（积压的 `MessageAdded`、
//!   `MessageUpdated`、`MessageRemoved`、`Typing`），并保证之后有一个 `ConversationsChanged`，平台据此重新读取。
//!   其他事件（实时消息、来电、登出、连接状态等）从不丢弃。

use std::collections::VecDeque;
use std::sync::{Arc, Condvar, Mutex, RwLock};
use std::time::{Duration, Instant};

use crate::error::{HushError, Result};
use crate::types::{CoreEvent, EventListener};

/// 待投递事件上限（只约束可丢弃事件）。
pub(crate) const EVENT_QUEUE_CAP: usize = 4096;
/// 事件不断时，同一种刷新事件的最小投递间隔。
pub(crate) const REFRESH_INTERVAL: Duration = Duration::from_millis(100);

/// 一种刷新事件的去抖状态。
#[derive(Default, Clone, Copy)]
struct Refresh {
    /// 有尚未投递的刷新请求（及它最早是什么时候提出的）
    dirty_since: Option<Instant>,
    last_delivered: Option<Instant>,
}

const REFRESH_KINDS: usize = 3;

fn refresh_kind(e: &CoreEvent) -> Option<usize> {
    match e {
        CoreEvent::ConversationsChanged => Some(0),
        CoreEvent::FriendsChanged => Some(1),
        CoreEvent::GroupsChanged => Some(2),
        _ => None,
    }
}

fn refresh_event(kind: usize) -> CoreEvent {
    match kind {
        0 => CoreEvent::ConversationsChanged,
        1 => CoreEvent::FriendsChanged,
        _ => CoreEvent::GroupsChanged,
    }
}

#[derive(Default)]
struct State {
    items: VecDeque<CoreEvent>,
    refresh: [Refresh; REFRESH_KINDS],
    closed: bool,
    /// 因超限丢弃的事件数（调试 / 测试）
    dropped: u64,
}

fn droppable(e: &CoreEvent) -> bool {
    matches!(
        e,
        CoreEvent::MessageAdded { backlog: true, .. } | CoreEvent::MessageUpdated { .. } | CoreEvent::MessageRemoved { .. } | CoreEvent::Typing { .. }
    )
}

#[derive(Default)]
struct Shared {
    state: Mutex<State>,
    cv: Condvar,
}

/// 事件队列的发送端。释放（`Inner` 释放）后投递线程退出。
pub(crate) struct EventQueue(Arc<Shared>);

impl Drop for EventQueue {
    fn drop(&mut self) {
        self.0.state.lock().unwrap_or_else(|e| e.into_inner()).closed = true;
        self.0.cv.notify_all();
    }
}

impl EventQueue {
    /// 启动投递线程。回调在任何内部锁之外执行；平台回调抛异常（UniFFI 转为 panic）时用 catch_unwind 兜住，
    /// 只记日志，不影响后续事件与核心任务。
    pub fn start(listener: Arc<RwLock<Option<Arc<dyn EventListener>>>>) -> Result<Self> {
        let shared = Arc::new(Shared::default());
        let s2 = shared.clone();
        std::thread::Builder::new()
            .name("hushcore-events".into())
            .spawn(move || {
                while let Some(ev) = pop(&s2) {
                    let l = listener.read().unwrap_or_else(|e| e.into_inner()).clone();
                    let Some(l) = l else { continue };
                    if std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| l.on_event(ev))).is_err() {
                        tracing::warn!("平台 on_event 回调异常（已忽略）");
                    }
                }
            })
            .map_err(|e| HushError::internal(format!("启动事件线程失败: {e}")))?;
        Ok(EventQueue(shared))
    }

    pub fn push(&self, e: CoreEvent) {
        let mut s = self.0.state.lock().unwrap_or_else(|e| e.into_inner());
        match refresh_kind(&e) {
            Some(k) => {
                s.refresh[k].dirty_since.get_or_insert_with(Instant::now);
            }
            None => {
                if droppable(&e) && s.items.len() >= EVENT_QUEUE_CAP {
                    if let Some(i) = s.items.iter().position(droppable) {
                        s.items.remove(i);
                        s.dropped += 1;
                        // 有事件被丢弃：平台要靠一次刷新把状态补齐
                        s.refresh[0].dirty_since.get_or_insert_with(Instant::now);
                    }
                }
                s.items.push_back(e);
            }
        }
        drop(s);
        self.0.cv.notify_one();
    }

    /// (待投递事件数, 因超限丢弃的事件数)
    #[cfg(test)]
    pub fn stats(&self) -> (usize, u64) {
        let s = self.0.state.lock().unwrap();
        (s.items.len() + s.refresh.iter().filter(|r| r.dirty_since.is_some()).count(), s.dropped)
    }
}

/// 取下一个要投递的事件；队列关闭且已投递完时返回 None。
fn pop(shared: &Shared) -> Option<CoreEvent> {
    let mut s = shared.state.lock().unwrap_or_else(|e| e.into_inner());
    loop {
        if s.closed {
            // 关闭后把剩下的投递完（不再等去抖间隔）
            if let Some(e) = s.items.pop_front() {
                return Some(e);
            }
            let k = (0..REFRESH_KINDS).find(|k| s.refresh[*k].dirty_since.is_some())?;
            s.refresh[k].dirty_since = None;
            return Some(refresh_event(k));
        }
        let now = Instant::now();
        let idle = s.items.is_empty();
        // 距离能投递下一个刷新事件还要等多久
        let mut wait: Option<Duration> = None;
        for k in 0..REFRESH_KINDS {
            let r = s.refresh[k];
            let Some(since) = r.dirty_since else { continue };
            // 间隔限制：距上次投递满 REFRESH_INTERVAL
            let allowed_at = r.last_delivered.map_or(now, |t| t + REFRESH_INTERVAL);
            // 队列里还有别的事件时先投递它们（保持「先变化、后刷新」的顺序），但刷新请求最多被推迟 REFRESH_INTERVAL
            let due_at = if idle { allowed_at } else { allowed_at.max(since + REFRESH_INTERVAL) };
            if due_at <= now {
                s.refresh[k] = Refresh { dirty_since: None, last_delivered: Some(now) };
                return Some(refresh_event(k));
            }
            let d = due_at - now;
            wait = Some(wait.map_or(d, |w| w.min(d)));
        }
        if let Some(e) = s.items.pop_front() {
            return Some(e);
        }
        s = match wait {
            Some(d) => shared.cv.wait_timeout(s, d).unwrap_or_else(|e| e.into_inner()).0,
            None => shared.cv.wait(s).unwrap_or_else(|e| e.into_inner()),
        };
    }
}
