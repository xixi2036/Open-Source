//! 对端身份：首次信任（TOFU）、身份变更处理、安全码。

use super::{direct_conv, Inner};
use crate::api::{self, ClaimResp, IdentityResp};
use crate::crypto::safety;
use crate::error::{HushError, Result};
use crate::store::IdentityRec;
use crate::types::*;
use crate::util::b64d;

impl From<IdentityResp> for IdentityRec {
    fn from(r: IdentityResp) -> Self {
        IdentityRec {
            user_id: r.user_id,
            identity_key: r.identity_key,
            signing_key: r.signing_key,
            device_generation: r.device_generation,
            verified: false,
        }
    }
}

impl From<&ClaimResp> for IdentityRec {
    fn from(c: &ClaimResp) -> Self {
        IdentityRec {
            user_id: c.user_id.clone(),
            identity_key: c.identity_key.clone(),
            signing_key: c.signing_key.clone(),
            device_generation: c.device_generation,
            verified: false,
        }
    }
}

impl Inner {
    pub(super) async fn fetch_identity(&self, uid: &str) -> Result<IdentityResp> {
        if self.hook_fail_identity_fetch() {
            return Err(HushError::network("获取身份失败（测试注入）"));
        }
        self.api()?.get(&format!("/v1/keys/identity/{}", api::enc(uid))).await
    }

    /// 获取对端身份记录；本地没有则从服务端拉取并首次信任（TOFU）。
    pub(crate) async fn known_identity(&self, uid: &str) -> Result<IdentityRec> {
        if let Some(r) = self.store.get_identity(uid)? {
            return Ok(r);
        }
        // 以请求的 uid 为准（不信任响应里的 user_id）
        let rec = IdentityRec { user_id: uid.to_string(), ..IdentityRec::from(self.fetch_identity(uid).await?) };
        self.store.put_identity(&rec)?;
        Ok(rec)
    }

    /// 对端身份变更：丢弃 Olm 会话、清除核验标记、群密钥重新分发、插入系统提示、发事件。
    pub(crate) fn on_identity_changed(&self, uid: &str, new_rec: Option<IdentityRec>) -> Result<()> {
        let had_old = self.store.get_identity(uid)?.is_some();
        self.forget_claim_refusal(Some(uid));
        self.store.delete_olm_sessions(uid)?;
        self.store.unshare_user(uid)?;
        match new_rec {
            Some(r) => self.store.put_identity(&IdentityRec { verified: false, ..r })?,
            None => self.store.delete_identity(uid)?,
        }
        let conv = direct_conv(uid);
        if had_old && self.store.get_conv(&conv)?.is_some() {
            let name = self.display_name(uid);
            self.insert_system(&conv, &format!("与 {name} 的安全码已变更"))?;
        }
        self.emit(CoreEvent::KeyChanged { user_id: uid.to_string() });
        Ok(())
    }

    async fn my_identity_key(&self) -> Result<[u8; 32]> {
        let _g = self.crypto_lock.lock().await;
        let acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
        Ok(acc.curve25519_key().to_bytes())
    }

    fn compute_safety(&self, my_ik: &[u8; 32], rec: &IdentityRec) -> Result<String> {
        let me = self.my_id()?;
        let their: [u8; 32] = b64d(&rec.identity_key)?
            .try_into()
            .map_err(|_| HushError::crypto("对端身份公钥长度错误"))?;
        Ok(safety::safety_number(&me, my_ik, &rec.user_id, &their))
    }

    pub(crate) async fn safety_number(&self, uid: &str) -> Result<SafetyNumber> {
        let rec = self.known_identity(uid).await?;
        let my = self.my_identity_key().await?;
        let digits = self.compute_safety(&my, &rec)?;
        Ok(SafetyNumber { qr_payload: safety::qr_payload(&digits), digits, verified: rec.verified })
    }

    /// 扫码核验：一致则标记已核验。仅使用本地已知身份（需先调用过 safety_number 或通信过）。
    pub(crate) fn verify_safety_qr(&self, uid: &str, payload: &str) -> Result<bool> {
        let Some(scanned) = safety::parse_qr(payload) else {
            return Ok(false);
        };
        let rec = self.store.get_identity(uid)?.ok_or_else(|| HushError::not_found("尚未获取对方身份密钥"))?;
        let acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
        let digits = self.compute_safety(&acc.curve25519_key().to_bytes(), &rec)?;
        let ok = digits == scanned;
        if ok {
            self.store.set_verified(uid, true)?;
        }
        Ok(ok)
    }
}
