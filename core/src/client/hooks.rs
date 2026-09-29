//! 测试注入与调试状态。只在单元测试或启用 `test-hooks` 特性（tests/e2e.rs 通过自引用 dev-dependency 打开）时编译；
//! 发布构建里这些钩子都是空操作，生产路径上不再有故障注入判断，也不再为每个信封复制一份密文。

use super::Inner;
use crate::api::Envelope;

#[cfg(any(test, feature = "test-hooks"))]
pub struct TestHooks {
    /// 最近处理的信封（验证重投幂等）
    pub last_envelope: std::sync::Mutex<Option<Envelope>>,
    /// 接下来这么多次获取对端身份直接返回网络错误
    pub fail_identity_fetch: std::sync::atomic::AtomicU32,
    /// 接下来这么多次 room_key 发送直接视为失败
    pub fail_room_key_posts: std::sync::atomic::AtomicU32,
    /// WS 读帧上限（调小以模拟服务端推来超限帧）
    pub ws_max_frame: std::sync::atomic::AtomicUsize,
    /// 接下来这么多个信封在解密后、处理前模拟进程被杀（C-H 故障注入）
    pub crash_after_decrypt: std::sync::atomic::AtomicU32,
}

#[cfg(any(test, feature = "test-hooks"))]
impl Default for TestHooks {
    fn default() -> Self {
        TestHooks {
            last_envelope: Default::default(),
            fail_identity_fetch: Default::default(),
            fail_room_key_posts: Default::default(),
            ws_max_frame: std::sync::atomic::AtomicUsize::new(crate::ws::MAX_FRAME_BYTES),
            crash_after_decrypt: Default::default(),
        }
    }
}

#[cfg(any(test, feature = "test-hooks"))]
fn take_one(c: &std::sync::atomic::AtomicU32) -> bool {
    use std::sync::atomic::Ordering;
    c.fetch_update(Ordering::SeqCst, Ordering::SeqCst, |n| n.checked_sub(1)).is_ok()
}

impl Inner {
    /// 测试注入：本次获取对端身份是否应模拟网络失败。
    pub(crate) fn hook_fail_identity_fetch(&self) -> bool {
        #[cfg(any(test, feature = "test-hooks"))]
        {
            take_one(&self.hooks.fail_identity_fetch)
        }
        #[cfg(not(any(test, feature = "test-hooks")))]
        {
            false
        }
    }

    /// 测试注入：本次 room_key 发送是否应模拟失败。
    pub(crate) fn hook_fail_room_key_post(&self) -> bool {
        #[cfg(any(test, feature = "test-hooks"))]
        {
            take_one(&self.hooks.fail_room_key_posts)
        }
        #[cfg(not(any(test, feature = "test-hooks")))]
        {
            false
        }
    }

    /// 测试注入：本信封是否在解密之后、处理之前模拟进程被杀。
    pub(crate) fn hook_crash_after_decrypt(&self) -> bool {
        #[cfg(any(test, feature = "test-hooks"))]
        {
            take_one(&self.hooks.crash_after_decrypt)
        }
        #[cfg(not(any(test, feature = "test-hooks")))]
        {
            false
        }
    }

    /// 调试：记录最近处理的信封。
    pub(crate) fn hook_record_envelope(&self, _env: &Envelope) {
        #[cfg(any(test, feature = "test-hooks"))]
        {
            *self.hooks.last_envelope.lock().unwrap_or_else(|e| e.into_inner()) = Some(_env.clone());
        }
    }

    /// WS 读帧上限。
    pub(crate) fn ws_frame_limit(&self) -> usize {
        #[cfg(any(test, feature = "test-hooks"))]
        {
            self.hooks.ws_max_frame.load(std::sync::atomic::Ordering::Relaxed)
        }
        #[cfg(not(any(test, feature = "test-hooks")))]
        {
            crate::ws::MAX_FRAME_BYTES
        }
    }
}
