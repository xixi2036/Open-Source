//! Olm（1:1）封装：密钥上传包、签名校验、会话建立与加解密。基于 vodozemac。

use serde::{Deserialize, Serialize};
use vodozemac::olm::{Account, OlmMessage, Session, SessionConfig};
use vodozemac::{Curve25519PublicKey, Ed25519PublicKey, Ed25519Signature};

use crate::error::{HushError, Result};
use crate::util::{b64d, b64e};

/// Olm 会话配置：统一使用 version_2（非截断 MAC）。
pub fn olm_config() -> SessionConfig {
    SessionConfig::version_2()
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct SignedKey {
    pub key_id: String,
    pub key: String,
    pub signature: String,
}

pub fn identity_key_b64(acc: &Account) -> String {
    b64e(acc.curve25519_key().as_bytes())
}

pub fn signing_key_b64(acc: &Account) -> String {
    b64e(acc.ed25519_key().as_bytes())
}

/// 对 `key` 字符串的 UTF-8 字节做 Ed25519 签名。
pub fn sign_key(acc: &Account, key_id: String, key_b64: String) -> SignedKey {
    let sig = acc.sign(key_b64.as_bytes());
    SignedKey { key_id, signature: b64e(sig.to_bytes()), key: key_b64 }
}

/// 生成 n 个新 OTK，返回全部未发布 OTK 的签名列表（上传成功后需 `mark_keys_as_published`）。
pub fn prepare_one_time_keys(acc: &mut Account, n: usize) -> Vec<SignedKey> {
    if n > 0 {
        acc.generate_one_time_keys(n);
    }
    let mut keys: Vec<SignedKey> = acc
        .one_time_keys()
        .into_iter()
        .map(|(id, k)| sign_key(acc, id.to_base64(), b64e(k.as_bytes())))
        .collect();
    keys.sort_by(|a, b| a.key_id.cmp(&b.key_id));
    keys
}

/// 确保存在未发布的 fallback key 并返回其签名。
pub fn prepare_fallback_key(acc: &mut Account) -> Option<SignedKey> {
    if acc.fallback_key().is_empty() {
        acc.generate_fallback_key();
    }
    acc.fallback_key()
        .into_iter()
        .next()
        .map(|(id, k)| sign_key(acc, id.to_base64(), b64e(k.as_bytes())))
}

pub fn parse_curve25519(b64: &str) -> Result<Curve25519PublicKey> {
    let bytes = b64d(b64)?;
    Curve25519PublicKey::from_slice(&bytes).map_err(|e| HushError::crypto(format!("Curve25519 公钥无效: {e}")))
}

pub fn parse_ed25519(b64: &str) -> Result<Ed25519PublicKey> {
    let bytes = b64d(b64)?;
    let arr: [u8; 32] = bytes
        .as_slice()
        .try_into()
        .map_err(|_| HushError::crypto("Ed25519 公钥长度错误"))?;
    Ed25519PublicKey::from_slice(&arr).map_err(|e| HushError::crypto(format!("Ed25519 公钥无效: {e}")))
}

/// 校验对端 OTK / fallback key 的签名。
pub fn verify_signed_key(signing_key_b64: &str, key_b64: &str, signature_b64: &str) -> Result<()> {
    let pk = parse_ed25519(signing_key_b64)?;
    let sig_bytes = b64d(signature_b64)?;
    let sig = Ed25519Signature::from_slice(&sig_bytes).map_err(|e| HushError::crypto(format!("签名格式错误: {e}")))?;
    pk.verify(key_b64.as_bytes(), &sig).map_err(|_| HushError::crypto("一次性密钥签名校验失败"))
}

pub fn create_outbound(acc: &Account, their_identity: Curve25519PublicKey, their_otk: Curve25519PublicKey) -> Result<Session> {
    acc.create_outbound_session(olm_config(), their_identity, their_otk)
        .map_err(|e| HushError::crypto(format!("建立 Olm 会话失败: {e}")))
}

/// 加密，返回 (消息类型 0/1, 密文字节)。
pub fn encrypt(session: &mut Session, plaintext: &[u8]) -> Result<(u8, Vec<u8>)> {
    let msg = session.encrypt(plaintext).map_err(|e| HushError::crypto(format!("Olm 加密失败: {e}")))?;
    let (t, bytes) = msg.to_parts();
    Ok((t as u8, bytes))
}

pub fn parse_message(msg_type: u8, bytes: &[u8]) -> Result<OlmMessage> {
    OlmMessage::from_parts(msg_type as usize, bytes).map_err(|e| HushError::crypto(format!("Olm 消息解析失败: {e}")))
}

/// 与某个对端的一个会话。
pub struct SessEntry {
    pub session: Session,
    pub last_used: i64,
}

#[derive(Debug)]
pub enum OlmDecryptError {
    /// PreKey 消息携带的身份公钥与已知身份不一致。
    IdentityMismatch { claimed: Curve25519PublicKey },
    Failed(String),
}

pub struct Decrypted {
    pub plaintext: Vec<u8>,
    /// 解密所用会话在 `sessions` 中的下标
    pub index: usize,
    /// 是否为本次新建的入站会话（账户 OTK 已被消耗，需要持久化账户）
    pub created: bool,
}

/// 解密来自某对端的 Olm 消息。
/// - PreKey：先找 session_keys 相同的已有会话；否则校验身份并 `create_inbound_session`。
/// - Normal：按最近使用顺序逐个尝试已有会话（失败不会改变会话状态）。
pub fn decrypt(
    acc: &mut Account,
    sessions: &mut Vec<SessEntry>,
    msg: &OlmMessage,
    expected_identity: Option<Curve25519PublicKey>,
    now: i64,
) -> std::result::Result<Decrypted, OlmDecryptError> {
    match msg {
        OlmMessage::PreKey(pk) => {
            if let Some(i) = sessions.iter().position(|s| s.session.session_keys() == pk.session_keys()) {
                return match sessions[i].session.decrypt(msg) {
                    Ok(p) => {
                        sessions[i].last_used = now;
                        Ok(Decrypted { plaintext: p, index: i, created: false })
                    }
                    Err(e) => Err(OlmDecryptError::Failed(e.to_string())),
                };
            }
            if let Some(ik) = expected_identity {
                if ik != pk.identity_key() {
                    return Err(OlmDecryptError::IdentityMismatch { claimed: pk.identity_key() });
                }
            }
            let res = acc
                .create_inbound_session(olm_config(), pk.identity_key(), pk)
                .map_err(|e| OlmDecryptError::Failed(e.to_string()))?;
            sessions.push(SessEntry { session: res.session, last_used: now });
            Ok(Decrypted { plaintext: res.plaintext, index: sessions.len() - 1, created: true })
        }
        OlmMessage::Normal(_) => {
            let mut order: Vec<usize> = (0..sessions.len()).collect();
            order.sort_by_key(|&i| std::cmp::Reverse(sessions[i].last_used));
            let mut last_err = "没有可用的 Olm 会话".to_string();
            for i in order {
                match sessions[i].session.decrypt(msg) {
                    Ok(p) => {
                        sessions[i].last_used = now;
                        return Ok(Decrypted { plaintext: p, index: i, created: false });
                    }
                    Err(e) => last_err = e.to_string(),
                }
            }
            Err(OlmDecryptError::Failed(last_err))
        }
    }
}

/// 选择最近使用的会话用于加密。
pub fn best_session(sessions: &[SessEntry]) -> Option<usize> {
    (0..sessions.len()).max_by_key(|&i| sessions[i].last_used)
}

pub fn pickle_account(acc: &Account, key: &[u8; 32]) -> String {
    acc.pickle().encrypt(key)
}

pub fn unpickle_account(s: &str, key: &[u8; 32]) -> Result<Account> {
    let p = vodozemac::olm::AccountPickle::from_encrypted(s, key).map_err(|e| HushError::crypto(format!("账户 pickle 解密失败: {e}")))?;
    Ok(Account::from_pickle(p))
}

pub fn pickle_session(s: &Session, key: &[u8; 32]) -> String {
    s.pickle().encrypt(key)
}

pub fn unpickle_session(s: &str, key: &[u8; 32]) -> Result<Session> {
    let p = vodozemac::olm::SessionPickle::from_encrypted(s, key).map_err(|e| HushError::crypto(format!("会话 pickle 解密失败: {e}")))?;
    Ok(Session::from_pickle(p))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 模拟服务端 claim：取对端一个已签名 OTK 并校验。
    fn claim(bob: &mut Account) -> (String, String, SignedKey) {
        let keys = prepare_one_time_keys(bob, 5);
        bob.mark_keys_as_published();
        (identity_key_b64(bob), signing_key_b64(bob), keys[0].clone())
    }

    #[test]
    fn olm_prekey_then_normal_roundtrip() {
        let alice = Account::new();
        let mut bob = Account::new();
        let (bob_ik, bob_sk, otk) = claim(&mut bob);
        verify_signed_key(&bob_sk, &otk.key, &otk.signature).unwrap();

        let mut a_sess = create_outbound(&alice, parse_curve25519(&bob_ik).unwrap(), parse_curve25519(&otk.key).unwrap()).unwrap();
        let (t, ct) = encrypt(&mut a_sess, b"hello bob").unwrap();
        assert_eq!(t, 0, "首条消息应为 PreKey");

        let mut bob_sessions = Vec::new();
        let mut bob_acc = bob;
        let msg = parse_message(t, &ct).unwrap();
        let alice_ik = parse_curve25519(&identity_key_b64(&alice)).unwrap();
        let d = decrypt(&mut bob_acc, &mut bob_sessions, &msg, Some(alice_ik), 1).unwrap();
        assert!(d.created);
        assert_eq!(d.plaintext, b"hello bob");

        // 同一 PreKey 重放：命中已有会话，不再新建
        let (t2, ct2) = encrypt(&mut a_sess, b"second prekey").unwrap();
        assert_eq!(t2, 0);
        let d2 = decrypt(&mut bob_acc, &mut bob_sessions, &parse_message(t2, &ct2).unwrap(), Some(alice_ik), 2).unwrap();
        assert!(!d2.created);
        assert_eq!(bob_sessions.len(), 1);

        // Bob 回复 → Normal
        let bi = best_session(&bob_sessions).unwrap();
        let (t3, ct3) = encrypt(&mut bob_sessions[bi].session, b"hi alice").unwrap();
        assert_eq!(t3, 1);
        let mut alice_acc = alice;
        let mut alice_sessions = vec![SessEntry { session: a_sess, last_used: 1 }];
        let d3 = decrypt(&mut alice_acc, &mut alice_sessions, &parse_message(t3, &ct3).unwrap(), None, 3).unwrap();
        assert_eq!(d3.plaintext, b"hi alice");

        // Alice 收到回复后发 Normal
        let (t4, ct4) = encrypt(&mut alice_sessions[0].session, b"normal now").unwrap();
        assert_eq!(t4, 1);
        let d4 = decrypt(&mut bob_acc, &mut bob_sessions, &parse_message(t4, &ct4).unwrap(), None, 4).unwrap();
        assert_eq!(d4.plaintext, b"normal now");
    }

    #[test]
    fn bad_signature_rejected() {
        let mut bob = Account::new();
        let (_, _, otk) = claim(&mut bob);
        let other = Account::new();
        assert!(verify_signed_key(&signing_key_b64(&other), &otk.key, &otk.signature).is_err());
        assert!(verify_signed_key(&signing_key_b64(&bob), "dGFtcGVyZWQ=", &otk.signature).is_err());
    }

    #[test]
    fn identity_mismatch_detected() {
        let alice = Account::new();
        let mut bob = Account::new();
        let (bob_ik, _, otk) = claim(&mut bob);
        let mut s = create_outbound(&alice, parse_curve25519(&bob_ik).unwrap(), parse_curve25519(&otk.key).unwrap()).unwrap();
        let (t, ct) = encrypt(&mut s, b"x").unwrap();
        let someone = Account::new();
        let mut sessions = Vec::new();
        let r = decrypt(&mut bob, &mut sessions, &parse_message(t, &ct).unwrap(), Some(someone.curve25519_key()), 1);
        assert!(matches!(r, Err(OlmDecryptError::IdentityMismatch { .. })));
    }

    #[test]
    fn fallback_key_and_pickles() {
        let alice = Account::new();
        let mut bob = Account::new();
        let fb = prepare_fallback_key(&mut bob).unwrap();
        bob.mark_keys_as_published();
        verify_signed_key(&signing_key_b64(&bob), &fb.key, &fb.signature).unwrap();
        let mut s = create_outbound(&alice, bob.curve25519_key(), parse_curve25519(&fb.key).unwrap()).unwrap();
        let (t, ct) = encrypt(&mut s, b"via fallback").unwrap();

        // 账户经 pickle 往返后仍能解密
        let key = [3u8; 32];
        let mut bob2 = unpickle_account(&pickle_account(&bob, &key), &key).unwrap();
        let mut sessions = Vec::new();
        let d = decrypt(&mut bob2, &mut sessions, &parse_message(t, &ct).unwrap(), None, 1).unwrap();
        assert_eq!(d.plaintext, b"via fallback");
        let restored = unpickle_session(&pickle_session(&sessions[0].session, &key), &key).unwrap();
        assert_eq!(restored.session_id(), sessions[0].session.session_id());
        assert!(unpickle_account(&pickle_account(&bob, &key), &[4u8; 32]).is_err());
    }

    #[test]
    fn fallback_rotation_keeps_previous_key_usable() {
        let alice = Account::new();
        let mut bob = Account::new();
        let fb1 = prepare_fallback_key(&mut bob).unwrap();
        bob.mark_keys_as_published();
        // 在途会话：Alice 用旧 fallback 建立
        let mut s1 = create_outbound(&alice, bob.curve25519_key(), parse_curve25519(&fb1.key).unwrap()).unwrap();
        let (t1, c1) = encrypt(&mut s1, b"via old fallback").unwrap();
        let m1 = parse_message(t1, &c1).unwrap();
        match &m1 {
            OlmMessage::PreKey(pk) => assert_eq!(pk.one_time_key(), parse_curve25519(&fb1.key).unwrap(), "可识别用了 fallback"),
            _ => panic!("应为 PreKey"),
        }

        // Bob 轮换：已发布的 fallback 不在 fallback_key() 里，prepare 会生成新的
        let fb2 = prepare_fallback_key(&mut bob).unwrap();
        assert_ne!(fb1.key, fb2.key);
        bob.mark_keys_as_published();

        // 旧 fallback 的在途消息仍能解密；新 fallback 也可用
        let mut sessions = Vec::new();
        let d = decrypt(&mut bob, &mut sessions, &m1, None, 1).unwrap();
        assert_eq!(d.plaintext, b"via old fallback");
        let mut s2 = create_outbound(&alice, bob.curve25519_key(), parse_curve25519(&fb2.key).unwrap()).unwrap();
        let (t2, c2) = encrypt(&mut s2, b"via new fallback").unwrap();
        let d2 = decrypt(&mut bob, &mut sessions, &parse_message(t2, &c2).unwrap(), None, 2).unwrap();
        assert_eq!(d2.plaintext, b"via new fallback");
    }
}
