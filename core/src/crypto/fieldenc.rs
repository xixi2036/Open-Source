//! 本地库字段级加密：AES-256-GCM，格式 `nonce(12) ‖ ciphertext‖tag`，AAD 为字段标签（防止跨字段挪用）。

use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use rand::RngCore;

use crate::error::{HushError, Result};

#[derive(Clone)]
pub struct DbCipher {
    cipher: Aes256Gcm,
    key: [u8; 32],
}

impl DbCipher {
    pub fn new(key: &[u8]) -> Result<Self> {
        if key.len() != 32 {
            return Err(HushError::invalid("db_key 必须为 32 字节"));
        }
        let mut k = [0u8; 32];
        k.copy_from_slice(key);
        Ok(DbCipher { cipher: Aes256Gcm::new_from_slice(&k).expect("32 字节密钥"), key: k })
    }

    /// vodozemac pickle 加密所用密钥（即库密钥本身）。
    pub fn pickle_key(&self) -> &[u8; 32] {
        &self.key
    }

    pub fn seal(&self, label: &str, plaintext: &[u8]) -> Vec<u8> {
        let mut nonce = [0u8; 12];
        rand::rngs::OsRng.fill_bytes(&mut nonce);
        let ct = self
            .cipher
            .encrypt(Nonce::from_slice(&nonce), Payload { msg: plaintext, aad: label.as_bytes() })
            .expect("AES-GCM 加密不会失败");
        let mut out = Vec::with_capacity(12 + ct.len());
        out.extend_from_slice(&nonce);
        out.extend_from_slice(&ct);
        out
    }

    pub fn open(&self, label: &str, data: &[u8]) -> Result<Vec<u8>> {
        if data.len() < 12 + 16 {
            return Err(HushError::storage("加密字段长度不足"));
        }
        self.cipher
            .decrypt(Nonce::from_slice(&data[..12]), Payload { msg: &data[12..], aad: label.as_bytes() })
            .map_err(|_| HushError::storage(format!("字段解密失败（{label}）：库密钥不匹配或数据损坏")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip_and_label_binding() {
        let c = DbCipher::new(&[7u8; 32]).unwrap();
        let blob = c.seal("msg.body", b"secret");
        assert_ne!(&blob[12..], b"secret");
        assert_eq!(c.open("msg.body", &blob).unwrap(), b"secret");
        assert!(c.open("other", &blob).is_err());
        let c2 = DbCipher::new(&[8u8; 32]).unwrap();
        assert!(c2.open("msg.body", &blob).is_err());
        assert!(DbCipher::new(&[1u8; 16]).is_err());
    }

    #[test]
    fn nonce_is_random() {
        let c = DbCipher::new(&[7u8; 32]).unwrap();
        assert_ne!(c.seal("l", b"x"), c.seal("l", b"x"));
    }
}
