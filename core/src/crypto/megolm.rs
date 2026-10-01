//! Megolm（群聊）封装。

use vodozemac::megolm::{DecryptionError, GroupSession, InboundGroupSession, MegolmMessage, SessionConfig, SessionKey};

use crate::error::{HushError, Result};

pub fn megolm_config() -> SessionConfig {
    SessionConfig::version_2()
}

pub fn new_outbound() -> GroupSession {
    GroupSession::new(megolm_config())
}

/// 当前棘轮位置导出的会话密钥（base64，经 Olm 以 room_key 分发）。
pub fn export_key(s: &GroupSession) -> String {
    s.session_key().to_base64()
}

pub fn inbound_from_key(key_b64: &str) -> Result<InboundGroupSession> {
    let key = SessionKey::from_base64(key_b64).map_err(|e| HushError::crypto(format!("room_key 无效: {e}")))?;
    Ok(InboundGroupSession::new(&key, megolm_config()))
}

pub fn encrypt(s: &mut GroupSession, plaintext: &[u8]) -> Vec<u8> {
    s.encrypt(plaintext).to_bytes()
}

#[cfg(test)]
pub fn decrypt(s: &mut InboundGroupSession, bytes: &[u8]) -> Result<Vec<u8>> {
    decrypt_detailed(s, bytes).map(|(p, _)| p).map_err(|e| match e {
        MegolmDecryptError::UnknownIndex { first_known, index } => {
            HushError::crypto(format!("Megolm 解密失败: 消息序号 {index} 早于已知的 {first_known}"))
        }
        MegolmDecryptError::Other(m) => HushError::crypto(m),
    })
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MegolmDecryptError {
    /// 消息序号早于本地入站会话的首个已知序号：之后可能收到更早位置导出的 room_key，应挂起而不是判为无法解密
    UnknownIndex { first_known: u32, index: u32 },
    Other(String),
}

/// 解密，返回 (明文, 消息序号)。序号用来识别被重放的密文（A8）。
pub fn decrypt_detailed(s: &mut InboundGroupSession, bytes: &[u8]) -> std::result::Result<(Vec<u8>, u32), MegolmDecryptError> {
    let msg = MegolmMessage::from_bytes(bytes).map_err(|e| MegolmDecryptError::Other(format!("Megolm 消息解析失败: {e}")))?;
    s.decrypt(&msg).map(|d| (d.plaintext, d.message_index)).map_err(|e| match e {
        DecryptionError::UnknownMessageIndex(first_known, index) => MegolmDecryptError::UnknownIndex { first_known, index },
        other => MegolmDecryptError::Other(format!("Megolm 解密失败: {other}")),
    })
}

/// 同一会话收到新的 room_key 时取「首个已知序号更小」的一方（两者必须是同一棘轮，否则返回 None 表示忽略新密钥）。
/// 返回 Some(合并后的会话) 表示应替换本地存储。
pub fn better_inbound(existing: &mut InboundGroupSession, new: &mut InboundGroupSession) -> Option<InboundGroupSession> {
    if new.first_known_index() >= existing.first_known_index() {
        return None;
    }
    existing.merge(new).filter(|m| m.first_known_index() < existing.first_known_index())
}

pub fn pickle_outbound(s: &GroupSession, key: &[u8; 32]) -> String {
    s.pickle().encrypt(key)
}

pub fn unpickle_outbound(p: &str, key: &[u8; 32]) -> Result<GroupSession> {
    let pk = vodozemac::megolm::GroupSessionPickle::from_encrypted(p, key).map_err(HushError::crypto)?;
    Ok(GroupSession::from_pickle(pk))
}

pub fn pickle_inbound(s: &InboundGroupSession, key: &[u8; 32]) -> String {
    s.pickle().encrypt(key)
}

pub fn unpickle_inbound(p: &str, key: &[u8; 32]) -> Result<InboundGroupSession> {
    let pk = vodozemac::megolm::InboundGroupSessionPickle::from_encrypted(p, key).map_err(HushError::crypto)?;
    Ok(InboundGroupSession::from_pickle(pk))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn earlier_room_key_replaces_later_one() {
        let mut out = new_outbound();
        let k0 = export_key(&out);
        let m0 = encrypt(&mut out, b"m0");
        let m1 = encrypt(&mut out, b"m1");
        let k2 = export_key(&out);
        let m2 = encrypt(&mut out, b"m2");

        // 先到的是较晚位置导出的密钥：m0/m1 报 UnknownIndex（可挂起），而不是一般解密失败
        let mut late = inbound_from_key(&k2).unwrap();
        assert_eq!(late.first_known_index(), 2);
        assert_eq!(decrypt_detailed(&mut late, &m0), Err(MegolmDecryptError::UnknownIndex { first_known: 2, index: 0 }));
        assert_eq!(decrypt_detailed(&mut late, &m2).unwrap(), (b"m2".to_vec(), 2));

        // 更早的密钥后到：应替换
        let mut early = inbound_from_key(&k0).unwrap();
        let mut merged = better_inbound(&mut late, &mut early).expect("更早的密钥应胜出");
        assert_eq!(merged.first_known_index(), 0);
        assert_eq!(decrypt_detailed(&mut merged, &m0).unwrap().0, b"m0");
        assert_eq!(decrypt_detailed(&mut merged, &m1).unwrap().0, b"m1");

        // 反过来：更晚的密钥不替换更早的
        let mut early2 = inbound_from_key(&k0).unwrap();
        let mut late2 = inbound_from_key(&k2).unwrap();
        assert!(better_inbound(&mut early2, &mut late2).is_none());

        // 不相连的会话（不同棘轮）即使序号更小也不合并
        let mut other = new_outbound();
        encrypt(&mut other, b"x");
        let mut unrelated_late = inbound_from_key(&export_key(&other)).unwrap();
        assert!(better_inbound(&mut unrelated_late, &mut inbound_from_key(&k0).unwrap()).is_none());
    }

    #[test]
    fn megolm_roundtrip_and_rotation() {
        let mut out = new_outbound();
        let first = encrypt(&mut out, b"before share");
        let key = export_key(&out);
        let mut inbound = inbound_from_key(&key).unwrap();
        assert_eq!(inbound.session_id(), out.session_id());
        // 导出点之前的消息无法解密（新成员看不到历史）
        assert!(decrypt(&mut inbound, &first).is_err());
        let c1 = encrypt(&mut out, b"hello group");
        let c2 = encrypt(&mut out, b"second");
        assert_eq!(decrypt(&mut inbound, &c2).unwrap(), b"second");
        assert_eq!(decrypt(&mut inbound, &c1).unwrap(), b"hello group");

        // 轮换：新会话的消息旧入站会话无法解密
        let mut rotated = new_outbound();
        assert_ne!(rotated.session_id(), out.session_id());
        let c3 = encrypt(&mut rotated, b"after kick");
        assert!(decrypt(&mut inbound, &c3).is_err());

        // pickle 往返
        let k = [5u8; 32];
        let mut out2 = unpickle_outbound(&pickle_outbound(&out, &k), &k).unwrap();
        let mut in2 = unpickle_inbound(&pickle_inbound(&inbound, &k), &k).unwrap();
        let c4 = encrypt(&mut out2, b"after pickle");
        assert_eq!(decrypt(&mut in2, &c4).unwrap(), b"after pickle");
    }
}
