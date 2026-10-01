//! 日志出口（2026-09-30 审查 D-P0）：把核心内部的 `tracing` 日志转发给平台层的回调。
//!
//! 默认不安装任何 subscriber（行为与之前一致，日志直接丢弃）。平台调用 [`set_log_listener`] 后安装一个
//! 全局 subscriber，只转发本库（target 以 `hushcore` 开头）的 error / warn / info / debug 日志。
//! 回调在专门的 `hushcore-log` 线程上执行（不在任何内部锁内）；队列有上限，平台处理不过来时丢弃新日志。
//!
//! 约定：核心的日志文本不包含消息明文、密钥与 token，只有用户 / 群 / 消息 id、错误码与错误描述。

use std::fmt::Write as _;
use std::sync::mpsc::{sync_channel, SyncSender};
use std::sync::{Arc, OnceLock, RwLock};

use tracing::field::{Field, Visit};
use tracing::span;

/// 日志级别。
#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum LogLevel {
    Error,
    Warn,
    Info,
    Debug,
}

/// 平台层实现的日志回调。`module` 是 Rust 模块路径（例如 `hushcore::client::sync`）。
#[uniffi::export(foreign)]
pub trait LogListener: Send + Sync {
    fn on_log(&self, level: LogLevel, module: String, message: String);
}

struct Record {
    level: LogLevel,
    module: String,
    message: String,
}

/// 待投递日志的上限：平台回调太慢时丢弃新日志，不阻塞核心。
const LOG_QUEUE_CAP: usize = 1024;

static LISTENER: RwLock<Option<Arc<dyn LogListener>>> = RwLock::new(None);
static QUEUE: OnceLock<SyncSender<Record>> = OnceLock::new();

/// 设置日志回调（全局，进程内所有 `HushClient` 共用；再次调用替换之前的回调）。
/// 首次调用时安装 tracing subscriber；进程里已经有别的全局 subscriber 时（例如宿主自己装了一个）不生效。
#[uniffi::export]
pub fn set_log_listener(listener: Arc<dyn LogListener>) {
    *LISTENER.write().unwrap_or_else(|e| e.into_inner()) = Some(listener);
    QUEUE.get_or_init(|| {
        let (tx, rx) = sync_channel::<Record>(LOG_QUEUE_CAP);
        let spawned = std::thread::Builder::new().name("hushcore-log".into()).spawn(move || {
            while let Ok(r) = rx.recv() {
                let l = LISTENER.read().unwrap_or_else(|e| e.into_inner()).clone();
                let Some(l) = l else { continue };
                // 平台回调抛异常（UniFFI 转为 panic）不能让日志线程退出
                let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| l.on_log(r.level, r.module, r.message)));
            }
        });
        if spawned.is_ok() {
            let _ = tracing::subscriber::set_global_default(Forwarder);
        }
        tx
    });
}

struct Forwarder;

#[derive(Default)]
struct MessageVisitor {
    message: String,
    fields: String,
}

impl Visit for MessageVisitor {
    fn record_debug(&mut self, field: &Field, value: &dyn std::fmt::Debug) {
        if field.name() == "message" {
            let _ = write!(self.message, "{value:?}");
        } else {
            let _ = write!(self.fields, " {}={value:?}", field.name());
        }
    }
}

impl tracing::Subscriber for Forwarder {
    fn enabled(&self, meta: &tracing::Metadata<'_>) -> bool {
        *meta.level() <= tracing::Level::DEBUG && meta.target().starts_with("hushcore")
    }

    fn new_span(&self, _attrs: &span::Attributes<'_>) -> span::Id {
        span::Id::from_u64(1)
    }

    fn record(&self, _span: &span::Id, _values: &span::Record<'_>) {}

    fn record_follows_from(&self, _span: &span::Id, _follows: &span::Id) {}

    fn event(&self, event: &tracing::Event<'_>) {
        let Some(tx) = QUEUE.get() else { return };
        let meta = event.metadata();
        let level = match *meta.level() {
            tracing::Level::ERROR => LogLevel::Error,
            tracing::Level::WARN => LogLevel::Warn,
            tracing::Level::INFO => LogLevel::Info,
            _ => LogLevel::Debug,
        };
        let mut v = MessageVisitor::default();
        event.record(&mut v);
        v.message.push_str(&v.fields);
        let module = meta.module_path().unwrap_or_else(|| meta.target()).to_string();
        // 队列满时丢弃（try_send 不阻塞调用方，调用方可能持有内部锁）
        let _ = tx.try_send(Record { level, module, message: v.message });
    }

    fn enter(&self, _span: &span::Id) {}

    fn exit(&self, _span: &span::Id) {}
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;
    use std::time::Duration;

    struct Collect(Mutex<std::sync::mpsc::Sender<(LogLevel, String, String)>>);
    impl LogListener for Collect {
        fn on_log(&self, level: LogLevel, module: String, message: String) {
            let _ = self.0.lock().unwrap().send((level, module, message));
        }
    }

    /// 装上回调后，核心的日志带着级别、模块与文本到达平台；第三方库的日志不转发。
    #[test]
    fn log_listener_receives_core_logs() {
        let (tx, rx) = std::sync::mpsc::channel();
        set_log_listener(Arc::new(Collect(Mutex::new(tx))));
        tracing::warn!("探针日志 {} code={}", 42, "x");
        tracing::info!(target: "hyper::proto", "第三方库的日志");
        tracing::trace!("trace 级别不转发");
        tracing::error!(attempt = 3, "带字段的日志");
        let mut got = Vec::new();
        while let Ok(r) = rx.recv_timeout(Duration::from_millis(500)) {
            // 同一进程里其他测试的日志也会到达，只看本测试发出的
            if r.2.contains("探针日志") || r.2.contains("第三方库") || r.2.contains("trace 级别") || r.2.contains("带字段的日志") {
                got.push(r);
            }
        }
        assert_eq!(got.len(), 2, "{got:?}");
        assert_eq!((got[0].0, got[0].2.as_str()), (LogLevel::Warn, "探针日志 42 code=x"));
        assert!(got[0].1.starts_with("hushcore::logging"), "{}", got[0].1);
        assert_eq!((got[1].0, got[1].2.as_str()), (LogLevel::Error, "带字段的日志 attempt=3"));
    }
}
