//! 聊天附件加密（AES-256-GCM，RustCrypto `aes-gcm`）。每个附件随机生成 32 字节密钥；`digest` = SHA-256(整个密文文件)。
//!
//! - **格式 v1**（旧版本发出，只解密）：`nonce(12) ‖ ciphertext ‖ tag(16)`，整个文件一次 GCM。
//! - **格式 v2**（C-I，当前发送格式）：分块 GCM，文件→文件流式加解密，内存占用与附件大小无关。
//!   ```text
//!   prefix(8 字节随机) ‖ chunk_0 ‖ chunk_1 ‖ … ‖ chunk_last
//!   chunk_i = ct_i ‖ tag_i(16)，ct_i 对应 64 KiB 明文（最后一块 0..=64 KiB；空文件也有一个空的最后一块）
//!   nonce_i = prefix ‖ u32_be(i)
//!   AAD     = "hush-att-v2" ‖ 0x00（中间块） / 0x01（最后一块）
//!   ```
//!   最后一块在 AAD 中标记 final：截断到块边界、删掉最后一块、在末尾追加数据都会让某一块的 AAD 不符而解密失败；
//!   块序号进入 nonce，调换或复制块同样失败。

use std::io::{Read, Write};

use aes_gcm::aead::{AeadInPlace, KeyInit};
use aes_gcm::{Aes256Gcm, Nonce, Tag};
use rand::RngCore;
use sha2::{Digest, Sha256};

use crate::error::{HushError, Result};

pub const NONCE_LEN: usize = 12;
pub const TAG_LEN: usize = 16;

/// 附件元数据 `format` 的取值。
pub const FORMAT_V1: u32 = 1;
pub const FORMAT_V2: u32 = 2;
/// v2 每块明文长度
pub const CHUNK_LEN: usize = 64 * 1024;
const PREFIX_LEN: usize = 8;
const AAD_LABEL: &[u8] = b"hush-att-v2";

// ================= v1（旧格式） =================

#[cfg(test)]
pub struct EncryptedAttachment {
    pub key: [u8; 32],
    pub blob: Vec<u8>,
    pub digest: [u8; 32],
}

/// v1 加密（只用于测试旧格式兼容）。
#[cfg(test)]
pub fn encrypt_v1(plaintext: &[u8]) -> EncryptedAttachment {
    let mut key = [0u8; 32];
    rand::rngs::OsRng.fill_bytes(&mut key);
    let mut buf = vec![0u8; NONCE_LEN];
    rand::rngs::OsRng.fill_bytes(&mut buf);
    buf.extend_from_slice(plaintext);
    let cipher = Aes256Gcm::new_from_slice(&key).expect("32 字节密钥");
    let (nonce, body) = buf.split_at_mut(NONCE_LEN);
    let tag = cipher.encrypt_in_place_detached(Nonce::from_slice(nonce), b"", body).expect("AES-GCM 加密不会失败");
    buf.extend_from_slice(&tag);
    let digest: [u8; 32] = Sha256::digest(&buf).into();
    EncryptedAttachment { key, blob: buf, digest }
}

/// v1：校验 digest 后就地解密，返回明文（复用 blob 的内存）。
pub fn decrypt_owned(key: &[u8], mut blob: Vec<u8>, expected_digest: &[u8]) -> Result<Vec<u8>> {
    if key.len() != 32 {
        return Err(HushError::crypto("附件密钥长度错误"));
    }
    let digest = Sha256::digest(&blob);
    if digest.as_slice() != expected_digest {
        return Err(HushError::crypto("附件 digest 校验失败"));
    }
    if blob.len() < NONCE_LEN + TAG_LEN {
        return Err(HushError::crypto("附件密文过短"));
    }
    let cipher = Aes256Gcm::new_from_slice(key).expect("32 字节密钥");
    let tag_at = blob.len() - TAG_LEN;
    let (head, tag) = blob.split_at_mut(tag_at);
    let (nonce, body) = head.split_at_mut(NONCE_LEN);
    cipher
        .decrypt_in_place_detached(Nonce::from_slice(nonce), b"", body, Tag::from_slice(tag))
        .map_err(|_| HushError::crypto("附件解密失败"))?;
    blob.truncate(tag_at);
    blob.drain(..NONCE_LEN);
    Ok(blob)
}

// ================= v2（分块流式） =================

/// v2 加密结果。
#[derive(Debug, Clone)]
pub struct StreamEncrypted {
    pub key: [u8; 32],
    /// SHA-256(整个密文)
    pub digest: [u8; 32],
    /// 密文总长度（上传时的 Content-Length）
    pub cipher_len: u64,
    pub plain_len: u64,
}

fn chunk_nonce(prefix: &[u8; PREFIX_LEN], index: u32) -> [u8; NONCE_LEN] {
    let mut n = [0u8; NONCE_LEN];
    n[..PREFIX_LEN].copy_from_slice(prefix);
    n[PREFIX_LEN..].copy_from_slice(&index.to_be_bytes());
    n
}

fn chunk_aad(last: bool) -> [u8; 12] {
    let mut a = [0u8; 12];
    a[..AAD_LABEL.len()].copy_from_slice(AAD_LABEL);
    a[AAD_LABEL.len()] = last as u8;
    a
}

/// 读满 `buf`，或读到 EOF 为止，返回读到的字节数。
fn read_full<R: Read>(r: &mut R, buf: &mut [u8]) -> std::io::Result<usize> {
    let mut n = 0;
    while n < buf.len() {
        match r.read(&mut buf[n..]) {
            Ok(0) => break,
            Ok(k) => n += k,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(e) => return Err(e),
        }
    }
    Ok(n)
}

/// 流式加密：从 `r` 读明文，向 `w` 写 v2 密文。内存中只有两块 64 KiB 缓冲。
pub fn encrypt_stream<R: Read, W: Write>(mut r: R, mut w: W) -> Result<StreamEncrypted> {
    let mut key = [0u8; 32];
    rand::rngs::OsRng.fill_bytes(&mut key);
    let mut prefix = [0u8; PREFIX_LEN];
    rand::rngs::OsRng.fill_bytes(&mut prefix);
    let cipher = Aes256Gcm::new_from_slice(&key).expect("32 字节密钥");
    let mut hasher = Sha256::new();
    let mut out = |data: &[u8], hasher: &mut Sha256| -> Result<()> {
        hasher.update(data);
        w.write_all(data)?;
        Ok(())
    };
    out(&prefix, &mut hasher)?;
    let mut cipher_len = PREFIX_LEN as u64;
    let mut plain_len = 0u64;

    let mut cur = vec![0u8; CHUNK_LEN];
    let mut next = vec![0u8; CHUNK_LEN];
    let mut n = read_full(&mut r, &mut cur)?;
    let mut index: u32 = 0;
    loop {
        // 本块读满时预读下一块，才能知道本块是否是最后一块
        let (m, last) = if n == CHUNK_LEN {
            let m = read_full(&mut r, &mut next)?;
            (m, m == 0)
        } else {
            (0, true)
        };
        let body = &mut cur[..n];
        let tag = cipher
            .encrypt_in_place_detached(Nonce::from_slice(&chunk_nonce(&prefix, index)), &chunk_aad(last), body)
            .map_err(|_| HushError::crypto("附件加密失败"))?;
        out(body, &mut hasher)?;
        out(&tag, &mut hasher)?;
        plain_len += n as u64;
        cipher_len += (n + TAG_LEN) as u64;
        if last {
            break;
        }
        std::mem::swap(&mut cur, &mut next);
        n = m;
        index = index.checked_add(1).ok_or_else(|| HushError::invalid("附件过大"))?;
    }
    w.flush()?;
    Ok(StreamEncrypted { key, digest: hasher.finalize().into(), cipher_len, plain_len })
}

/// 流式解密 v2 密文（digest 由调用方在此之前校验）。任何一块认证失败、截断、缺最后一块都返回错误；
/// 出错时 `w` 里可能已写入前面若干块的明文，调用方应写到临时文件、成功后再改名。返回明文长度。
pub fn decrypt_stream<R: Read, W: Write>(key: &[u8], mut r: R, mut w: W) -> Result<u64> {
    if key.len() != 32 {
        return Err(HushError::crypto("附件密钥长度错误"));
    }
    let cipher = Aes256Gcm::new_from_slice(key).expect("32 字节密钥");
    let mut prefix = [0u8; PREFIX_LEN];
    if read_full(&mut r, &mut prefix)? != PREFIX_LEN {
        return Err(HushError::crypto("附件密文过短"));
    }
    const FULL: usize = CHUNK_LEN + TAG_LEN;
    let mut cur = vec![0u8; FULL];
    let mut next = vec![0u8; FULL];
    let mut n = read_full(&mut r, &mut cur)?;
    let mut index: u32 = 0;
    let mut plain_len = 0u64;
    loop {
        if n < TAG_LEN {
            return Err(HushError::crypto("附件密文被截断"));
        }
        let (m, last) = if n == FULL {
            let m = read_full(&mut r, &mut next)?;
            (m, m == 0)
        } else {
            (0, true)
        };
        let (body, tag) = cur[..n].split_at_mut(n - TAG_LEN);
        cipher
            .decrypt_in_place_detached(Nonce::from_slice(&chunk_nonce(&prefix, index)), &chunk_aad(last), body, Tag::from_slice(tag))
            .map_err(|_| HushError::crypto("附件解密失败（数据被篡改或不完整）"))?;
        w.write_all(body)?;
        plain_len += body.len() as u64;
        if last {
            break;
        }
        std::mem::swap(&mut cur, &mut next);
        n = m;
        index = index.checked_add(1).ok_or_else(|| HushError::crypto("附件块数过多"))?;
    }
    w.flush()?;
    Ok(plain_len)
}

/// v2 密文长度：前缀 + 明文 + 每块一个 tag（至少一块）。
pub fn v2_cipher_len(plain_len: u64) -> u64 {
    let chunks = plain_len.div_ceil(CHUNK_LEN as u64).max(1);
    PREFIX_LEN as u64 + plain_len + chunks * TAG_LEN as u64
}

#[cfg(test)]
mod tests {
    use super::*;

    fn data(n: usize) -> Vec<u8> {
        (0..n as u32).map(|i| (i.wrapping_mul(31) % 251) as u8).collect()
    }

    fn enc(plain: &[u8]) -> (StreamEncrypted, Vec<u8>) {
        let mut out = Vec::new();
        let r = encrypt_stream(plain, &mut out).unwrap();
        (r, out)
    }

    fn dec(key: &[u8], blob: &[u8]) -> Result<Vec<u8>> {
        let mut out = Vec::new();
        decrypt_stream(key, blob, &mut out)?;
        Ok(out)
    }

    #[test]
    fn v1_roundtrip_and_standard_gcm() {
        use aes_gcm::aead::Aead;
        let d = data(100_000);
        let e = encrypt_v1(&d);
        assert_eq!(e.blob.len(), 12 + d.len() + 16);
        let cipher = Aes256Gcm::new_from_slice(&e.key).unwrap();
        assert_eq!(cipher.decrypt(Nonce::from_slice(&e.blob[..12]), &e.blob[12..]).unwrap(), d);
        assert_eq!(decrypt_owned(&e.key, e.blob.clone(), &e.digest).unwrap(), d);
        let mut bad = e.blob.clone();
        bad[20] ^= 1;
        assert!(decrypt_owned(&e.key, bad.clone(), &e.digest).is_err());
        let d2: [u8; 32] = Sha256::digest(&bad).into();
        assert!(decrypt_owned(&e.key, bad, &d2).is_err(), "digest 也改掉：GCM tag 仍拒绝");
        assert!(decrypt_owned(&[0u8; 32], e.blob.clone(), &e.digest).is_err());
    }

    #[test]
    fn v2_roundtrip_boundaries() {
        for n in [0, 1, CHUNK_LEN - 1, CHUNK_LEN, CHUNK_LEN + 1, 3 * CHUNK_LEN, 3 * CHUNK_LEN + 7] {
            let d = data(n);
            let (r, blob) = enc(&d);
            assert_eq!(blob.len() as u64, r.cipher_len, "n={n}");
            assert_eq!(r.cipher_len, v2_cipher_len(n as u64), "n={n}");
            assert_eq!(r.plain_len, n as u64);
            assert_eq!(r.digest, <[u8; 32]>::from(Sha256::digest(&blob)), "digest 是整个密文的 SHA-256");
            assert_eq!(dec(&r.key, &blob).unwrap(), d, "n={n}");
        }
    }

    #[test]
    fn v2_nonce_and_key_are_fresh() {
        let (a, ba) = enc(b"same");
        let (b, bb) = enc(b"same");
        assert_ne!(a.key, b.key);
        assert_ne!(ba, bb);
    }

    /// 篡改任一块（含前缀、tag）、截断、删掉最后一块、追加、调换块、错误密钥都必须失败。
    #[test]
    fn v2_rejects_tampering_and_truncation() {
        let d = data(3 * CHUNK_LEN + 100);
        let (r, blob) = enc(&d);
        let full = CHUNK_LEN + TAG_LEN;
        let chunk_at = |i: usize| PREFIX_LEN + i * full;
        // 每一块中间、每一块的 tag、前缀各翻一位
        let mut positions = vec![0usize, PREFIX_LEN - 1];
        for i in 0..4 {
            positions.push(chunk_at(i) + 5);
            let end = if i == 3 { blob.len() } else { chunk_at(i + 1) };
            positions.push(end - 1);
        }
        for p in positions {
            let mut bad = blob.clone();
            bad[p] ^= 0x40;
            assert!(dec(&r.key, &bad).is_err(), "篡改位置 {p} 应失败");
        }
        // 截断：块中间、块边界（删掉最后一块）、只剩前缀、空
        for len in [blob.len() - 1, chunk_at(3), chunk_at(2), chunk_at(1) + 10, PREFIX_LEN + 3, PREFIX_LEN, 4, 0] {
            assert!(dec(&r.key, &blob[..len]).is_err(), "截断到 {len} 应失败");
        }
        // 末尾追加数据：原最后一块不再是最后一块
        let mut ext = blob.clone();
        ext.extend_from_slice(&[0u8; 40]);
        assert!(dec(&r.key, &ext).is_err());
        // 调换前两块
        let mut sw = blob.clone();
        sw[chunk_at(0)..chunk_at(1)].copy_from_slice(&blob[chunk_at(1)..chunk_at(2)]);
        sw[chunk_at(1)..chunk_at(2)].copy_from_slice(&blob[chunk_at(0)..chunk_at(1)]);
        assert!(dec(&r.key, &sw).is_err());
        // 删掉中间一块
        let mut gap = blob[..chunk_at(1)].to_vec();
        gap.extend_from_slice(&blob[chunk_at(2)..]);
        assert!(dec(&r.key, &gap).is_err());
        assert!(dec(&[1u8; 32], &blob).is_err());
        // 恰好一整块的附件去掉 tag 以外的部分也失败
        let (r1, one) = enc(&data(CHUNK_LEN));
        assert!(dec(&r1.key, &one[..one.len() - TAG_LEN]).is_err());
        assert_eq!(dec(&r1.key, &one).unwrap(), data(CHUNK_LEN));
    }

    /// 20MB 文件→文件往返：流式读写，结果一致。
    #[test]
    fn v2_large_file_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let (src, enc_p, out) = (dir.path().join("a"), dir.path().join("a.enc"), dir.path().join("a.out"));
        let d = data(20 * 1024 * 1024 + 123);
        std::fs::write(&src, &d).unwrap();
        let r = encrypt_stream(
            std::io::BufReader::new(std::fs::File::open(&src).unwrap()),
            std::io::BufWriter::new(std::fs::File::create(&enc_p).unwrap()),
        )
        .unwrap();
        assert_eq!(std::fs::metadata(&enc_p).unwrap().len(), r.cipher_len);
        let n = decrypt_stream(
            &r.key,
            std::io::BufReader::new(std::fs::File::open(&enc_p).unwrap()),
            std::io::BufWriter::new(std::fs::File::create(&out).unwrap()),
        )
        .unwrap();
        assert_eq!(n, d.len() as u64);
        assert!(std::fs::read(&out).unwrap() == d);
    }
}
