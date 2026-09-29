//! 安全码（设计文档 4.4）。
//!
//! 每方指纹：初始值 `0x00 0x00 ‖ identity_key(32B) ‖ user_id(UTF-8)`，
//! 迭代 5200 次 `h = SHA-512(h ‖ identity_key)`；取前 30 字节，每 5 字节按大端整数 mod 100000
//! 转为 5 位十进制，共 30 位。两方按 user_id 字典序拼接为 60 位。

use sha2::{Digest, Sha512};

pub const ITERATIONS: usize = 5200;
pub const QR_PREFIX: &str = "hush-sn:v1:";

pub fn fingerprint(user_id: &str, identity_key: &[u8; 32]) -> String {
    let mut h: Vec<u8> = Vec::with_capacity(2 + 32 + user_id.len());
    h.extend_from_slice(&[0u8, 0u8]);
    h.extend_from_slice(identity_key);
    h.extend_from_slice(user_id.as_bytes());
    for _ in 0..ITERATIONS {
        let mut hasher = Sha512::new();
        hasher.update(&h);
        hasher.update(identity_key);
        h = hasher.finalize().to_vec();
    }
    let mut out = String::with_capacity(30);
    for chunk in h[..30].chunks(5) {
        let mut v: u64 = 0;
        for b in chunk {
            v = (v << 8) | *b as u64;
        }
        out.push_str(&format!("{:05}", v % 100_000));
    }
    out
}

/// 60 位安全码：两方指纹按 user_id 字典序拼接。
pub fn safety_number(a_id: &str, a_key: &[u8; 32], b_id: &str, b_key: &[u8; 32]) -> String {
    let fa = fingerprint(a_id, a_key);
    let fb = fingerprint(b_id, b_key);
    if a_id <= b_id {
        format!("{fa}{fb}")
    } else {
        format!("{fb}{fa}")
    }
}

pub fn qr_payload(digits: &str) -> String {
    format!("{QR_PREFIX}{digits}")
}

/// 解析二维码内容，返回 60 位数字。
pub fn parse_qr(payload: &str) -> Option<String> {
    let d = payload.trim().strip_prefix(QR_PREFIX)?;
    if d.len() == 60 && d.bytes().all(|c| c.is_ascii_digit()) {
        Some(d.to_string())
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn both_sides_agree() {
        let ka = [1u8; 32];
        let kb = [2u8; 32];
        let a = safety_number("alice-id", &ka, "bob-id", &kb);
        let b = safety_number("bob-id", &kb, "alice-id", &ka);
        assert_eq!(a, b);
        assert_eq!(a.len(), 60);
        assert!(a.bytes().all(|c| c.is_ascii_digit()));
        // 字典序：alice-id 在前
        assert_eq!(&a[..30], fingerprint("alice-id", &ka));
    }

    #[test]
    fn changes_with_key() {
        let a = safety_number("a", &[1u8; 32], "b", &[2u8; 32]);
        let b = safety_number("a", &[1u8; 32], "b", &[3u8; 32]);
        assert_ne!(a, b);
        assert_eq!(&a[..30], &b[..30]);
    }

    #[test]
    fn deterministic_fingerprint() {
        let f1 = fingerprint("user", &[9u8; 32]);
        let f2 = fingerprint("user", &[9u8; 32]);
        assert_eq!(f1, f2);
        assert_ne!(f1, fingerprint("user2", &[9u8; 32]));
    }

    #[test]
    fn qr_roundtrip() {
        let d = safety_number("a", &[1u8; 32], "b", &[2u8; 32]);
        assert_eq!(parse_qr(&qr_payload(&d)).unwrap(), d);
        assert!(parse_qr("hush-sn:v1:123").is_none());
        assert!(parse_qr("other").is_none());
    }
}
