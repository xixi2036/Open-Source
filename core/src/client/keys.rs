//! 密钥管理：新登录上传账户密钥、一次性密钥（OTK）补充、fallback key 轮换。
//!
//! 流程：持 crypto_lock 生成密钥并**先落盘**（仍为未发布）→ 锁外上传 → 持锁重新读取账户、标记已发布。

use std::sync::Arc;

use serde_json::json;
use vodozemac::olm::Account;

use super::Inner;
use crate::api::{self, KeyCount};
use crate::crypto::olm;
use crate::error::{HushError, Result};
use crate::util::now_ms;

/// fallback key 定期轮换的周期。
pub const FALLBACK_MAX_AGE_MS: i64 = 7 * 24 * 3600 * 1000;

/// 已发布的 fallback key 记录（加密存于 kv `fallback_meta`）。vodozemac 只能取到「未发布」的 fallback，
/// 所以自己记下公钥，用来判断对方的 PreKey 消息是否用了 fallback。
#[derive(Debug, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct FallbackMeta {
    pub public_key: String,
    pub created_at: i64,
    #[serde(default)]
    pub used: bool,
}

/// 是否该轮换 fallback key：没有记录（旧版本数据）、已被使用过，或已超过 7 天。
pub fn fallback_rotation_due(meta: Option<&FallbackMeta>, now: i64) -> bool {
    match meta {
        None => true,
        Some(m) => m.used || now - m.created_at >= FALLBACK_MAX_AGE_MS,
    }
}

/// `PUT /v1/keys` 请求体。
fn key_upload_body(acc: &Account, otks: Vec<olm::SignedKey>, fb: Option<&olm::SignedKey>) -> serde_json::Value {
    let mut body = json!({
        "identity_key": olm::identity_key_b64(acc),
        "signing_key": olm::signing_key_b64(acc),
        "one_time_keys": otks,
    });
    if let Some(f) = fb {
        body["fallback_key"] = json!(f);
    }
    body
}

pub const OTK_LOW_WATERMARK: i64 = 20;

pub const OTK_TARGET: i64 = 100;

impl Inner {
    /// 新登录：生成全新 Account，上传身份密钥 + 100 个 OTK + fallback key。
    pub(crate) async fn create_and_upload_account(&self, api: &api::Api) -> Result<()> {
        let _upload = self.key_upload.lock().await;
        let (body, fb) = {
            let _g = self.crypto_lock.lock().await;
            let mut acc = Account::new();
            let otks = olm::prepare_one_time_keys(&mut acc, OTK_TARGET as usize);
            let fb = olm::prepare_fallback_key(&mut acc);
            // 先落盘再上传：上传超时（服务端其实已收到）或进程被杀时，私钥不会丢
            self.store.save_account(&acc)?;
            (key_upload_body(&acc, otks, fb.as_ref()), fb)
        };
        self.upload_prepared_keys(api, &body, fb).await
    }

    /// OTK 数量低于 20 时补充到 100；fallback key 被使用过或已满 7 天时一并轮换；
    /// 上次上传中断而遗留的未发布密钥（私钥已落盘）原样重传。
    ///
    /// 只有生成密钥与标记已发布这两步持有 crypto_lock，HTTP 请求都在锁外，不阻塞收发消息的加解密。
    pub(crate) async fn replenish_keys(&self) -> Result<()> {
        let _upload = self.key_upload.lock().await;
        let api = self.api()?;
        let count: KeyCount = api.get("/v1/keys/count").await?;
        let meta: Option<FallbackMeta> = self.store.kv_get_json("fallback_meta")?;
        let rotate_fb = fallback_rotation_due(meta.as_ref(), now_ms());
        let (body, fb, new_otks) = {
            let _g = self.crypto_lock.lock().await;
            let mut acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
            let leftover_otks = acc.one_time_keys().len() as i64;
            let leftover_fb = !acc.fallback_key().is_empty();
            let need_otks = count.one_time_key_count < OTK_LOW_WATERMARK;
            if !need_otks && !rotate_fb && leftover_otks == 0 && !leftover_fb {
                return Ok(());
            }
            let n = if need_otks { (OTK_TARGET - count.one_time_key_count - leftover_otks).max(0) as usize } else { 0 };
            // 返回全部未发布 OTK（本次新生成的 + 上次中断遗留的）
            let otks = olm::prepare_one_time_keys(&mut acc, n);
            // 已发布的 fallback 不会出现在 acc.fallback_key() 里，prepare_fallback_key 会生成新的；
            // 遗留的未发布 fallback 则原样返回。vodozemac 仍保留上一个 fallback 的私钥，用旧 fallback 建立中的会话照样能解密。
            let fb = if rotate_fb || leftover_fb { olm::prepare_fallback_key(&mut acc) } else { None };
            if leftover_otks > 0 || leftover_fb {
                tracing::info!("重传上次未确认上传的密钥：OTK {leftover_otks} 个，fallback {leftover_fb}");
            }
            // 先落盘（此时仍是未发布状态），再上传
            self.store.save_account(&acc)?;
            (key_upload_body(&acc, otks, fb.as_ref()), fb, n)
        };
        self.upload_prepared_keys(&api, &body, fb).await?;
        if new_otks > 0 {
            tracing::info!("补充 OTK {new_otks} 个");
        }
        Ok(())
    }

    /// 上传已落盘的密钥包（锁外），成功后重新读取账户并标记为已发布、记录 fallback。
    /// 必须重新读取：上传期间 decrypt_olm 可能消耗了 OTK 并保存了账户，写回旧对象会让用过的 OTK 复活。
    async fn upload_prepared_keys(&self, api: &api::Api, body: &serde_json::Value, fb: Option<olm::SignedKey>) -> Result<()> {
        let _: KeyCount = api.put("/v1/keys", body).await?;
        let _g = self.crypto_lock.lock().await;
        let mut acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
        acc.mark_keys_as_published();
        self.store.save_account(&acc)?;
        if let Some(f) = fb {
            self.store.kv_set_json("fallback_meta", &FallbackMeta { public_key: f.key, created_at: now_ms(), used: false })?;
            tracing::info!("已发布新的 fallback key");
        }
        Ok(())
    }

    /// 对方用我们的 fallback key 建立了会话：标记已使用，触发尽快轮换。
    pub(super) fn note_fallback_used(&self, one_time_key: &vodozemac::Curve25519PublicKey) -> Result<()> {
        let Some(mut meta) = self.store.kv_get_json::<FallbackMeta>("fallback_meta")? else { return Ok(()) };
        if !meta.used && olm::parse_curve25519(&meta.public_key).ok().as_ref() == Some(one_time_key) {
            meta.used = true;
            self.store.kv_set_json("fallback_meta", &meta)?;
            tracing::info!("fallback key 已被使用，将轮换");
        }
        Ok(())
    }

    pub(crate) fn spawn_replenish(self: &Arc<Self>) {
        use std::sync::atomic::Ordering;
        if self.replenishing.swap(true, Ordering::SeqCst) {
            return;
        }
        let me = self.clone();
        let generation = self.session_gen();
        tokio::spawn(async move {
            if me.same_session(generation) {
                if let Err(e) = me.replenish_keys().await {
                    tracing::warn!("补充 OTK 失败: {e}");
                }
            }
            me.replenishing.store(false, Ordering::SeqCst);
        });
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashSet;

    use super::*;
    use crate::client::published_account;
    use crate::util::b64e;

    fn b64_set(v: &serde_json::Value) -> HashSet<String> {
        v["one_time_keys"].as_array().unwrap().iter().map(|k| k["key"].as_str().unwrap().to_string()).collect()
    }

    /// PUT /v1/keys 失败（例如服务端已落库但客户端超时）：新密钥的私钥已落盘；重试时原样重传同一批密钥，
    /// 成功后才标记已发布并记录 fallback。
    #[tokio::test]
    async fn interrupted_key_upload_keeps_private_keys() {
        use std::sync::atomic::{AtomicU16, Ordering};
        use std::sync::Mutex;
        let put_status = Arc::new(AtomicU16::new(503));
        let uploads: Arc<Mutex<Vec<serde_json::Value>>> = Arc::default();
        let (st, up) = (put_status.clone(), uploads.clone());
        let srv = crate::client::stub::start(Arc::new(move |m, path, body| match (m, path) {
            // 第二次（成功的）上传之后服务端才有足够的 OTK
            ("GET", "/v1/keys/count") => {
                let n = if up.lock().unwrap().len() >= 2 { 100 } else { 0 };
                (200, format!(r#"{{"one_time_key_count":{n}}}"#))
            }
            ("PUT", "/v1/keys") => {
                up.lock().unwrap().push(serde_json::from_slice(body).unwrap());
                match st.load(Ordering::SeqCst) {
                    200 => (200, r#"{"one_time_key_count":100}"#.into()),
                    s => (s, "timeout".into()),
                }
            }
            _ => (404, r#"{"error":"not_found"}"#.into()),
        }))
        .await;
        let (inner, _dir) = crate::client::test_inner_at(&srv.url);
        published_account(&inner);

        assert!(inner.replenish_keys().await.is_err());
        let first = uploads.lock().unwrap()[0].clone();
        let sent = b64_set(&first);
        assert_eq!(sent.len(), OTK_TARGET as usize);
        let acc = inner.store.load_account().unwrap().unwrap();
        let kept: HashSet<String> = acc.one_time_keys().values().map(|k| b64e(k.as_bytes())).collect();
        assert_eq!(kept, sent, "上传失败后，已发出的 OTK 私钥必须仍在本地账户里");
        let fb_sent = first["fallback_key"]["key"].as_str().unwrap().to_string();
        assert_eq!(acc.fallback_key().values().map(|k| b64e(k.as_bytes())).collect::<Vec<_>>(), vec![fb_sent.clone()]);
        assert!(inner.store.kv_get_json::<FallbackMeta>("fallback_meta").unwrap().is_none(), "未确认上传前不记录 fallback");

        put_status.store(200, Ordering::SeqCst);
        inner.replenish_keys().await.unwrap();
        let second = uploads.lock().unwrap()[1].clone();
        assert_eq!(b64_set(&second), sent, "重试应原样重传同一批 OTK，而不是另生成一批");
        assert_eq!(second["fallback_key"]["key"].as_str(), Some(fb_sent.as_str()));
        let acc = inner.store.load_account().unwrap().unwrap();
        assert!(acc.one_time_keys().is_empty() && acc.fallback_key().is_empty(), "成功后标记为已发布");
        assert_eq!(inner.store.kv_get_json::<FallbackMeta>("fallback_meta").unwrap().unwrap().public_key, fb_sent);

        // 没有遗留、数量充足、fallback 未到期：不再上传
        inner.replenish_keys().await.ok();
        assert_eq!(uploads.lock().unwrap().len(), 2);
    }

    #[test]
    fn fallback_rotation_schedule() {
        let now = 10 * FALLBACK_MAX_AGE_MS;
        let fresh = FallbackMeta { public_key: "k".into(), created_at: now - 1000, used: false };
        assert!(!fallback_rotation_due(Some(&fresh), now));
        assert!(fallback_rotation_due(None, now), "旧数据没有记录：轮换一次");
        assert!(fallback_rotation_due(Some(&FallbackMeta { used: true, ..fresh.clone() }), now), "被使用过");
        assert!(fallback_rotation_due(Some(&FallbackMeta { created_at: now - FALLBACK_MAX_AGE_MS, ..fresh }), now), "满 7 天");
    }
}
