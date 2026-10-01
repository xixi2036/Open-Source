//! 小工具：时间、base64、UUID、图片尺寸解析。

use base64::engine::general_purpose::{GeneralPurpose, GeneralPurposeConfig};
use base64::engine::DecodePaddingMode;
use base64::{alphabet, Engine};

use crate::error::{HushError, Result};

/// 编码：标准字母表、带填充（契约 3 通用约定）。
const B64_ENC: GeneralPurpose = base64::engine::general_purpose::STANDARD;
/// 解码：填充可有可无（兼容 vodozemac 的无填充输出）。
const B64_DEC: GeneralPurpose = GeneralPurpose::new(
    &alphabet::STANDARD,
    GeneralPurposeConfig::new().with_decode_padding_mode(DecodePaddingMode::Indifferent),
);

pub fn b64e(data: impl AsRef<[u8]>) -> String {
    B64_ENC.encode(data)
}

pub fn b64d(s: &str) -> Result<Vec<u8>> {
    B64_DEC
        .decode(s.trim())
        .map_err(|e| HushError::invalid(format!("base64: {e}")))
}

pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis() as i64)
        .unwrap_or(0)
}

pub fn new_id() -> String {
    uuid::Uuid::new_v4().to_string()
}

/// 从 PNG / JPEG / GIF 文件头解析宽高（用于朋友圈图片元数据），无法识别返回 (0,0)。
pub fn image_dimensions(data: &[u8]) -> (u32, u32) {
    // PNG: 8 字节签名 + IHDR
    if data.len() >= 24 && &data[0..8] == b"\x89PNG\r\n\x1a\n" {
        let w = u32::from_be_bytes([data[16], data[17], data[18], data[19]]);
        let h = u32::from_be_bytes([data[20], data[21], data[22], data[23]]);
        return (w, h);
    }
    // GIF
    if data.len() >= 10 && (&data[0..6] == b"GIF87a" || &data[0..6] == b"GIF89a") {
        let w = u16::from_le_bytes([data[6], data[7]]) as u32;
        let h = u16::from_le_bytes([data[8], data[9]]) as u32;
        return (w, h);
    }
    // JPEG: 遍历段直到 SOFn
    if data.len() >= 4 && data[0] == 0xFF && data[1] == 0xD8 {
        let mut i = 2usize;
        while i + 9 < data.len() {
            if data[i] != 0xFF {
                i += 1;
                continue;
            }
            let marker = data[i + 1];
            if marker == 0xD8 || marker == 0x01 || (0xD0..=0xD7).contains(&marker) || marker == 0xFF {
                i += if marker == 0xFF { 1 } else { 2 };
                continue;
            }
            let len = u16::from_be_bytes([data[i + 2], data[i + 3]]) as usize;
            let is_sof = matches!(marker, 0xC0..=0xCF) && !matches!(marker, 0xC4 | 0xC8 | 0xCC);
            if is_sof {
                let h = u16::from_be_bytes([data[i + 5], data[i + 6]]) as u32;
                let w = u16::from_be_bytes([data[i + 7], data[i + 8]]) as u32;
                return (w, h);
            }
            i += 2 + len;
        }
    }
    (0, 0)
}

/// 文件名片段的字节上限（B3）。ext4 / f2fs 的文件名上限是 255 **字节**；附件缓存文件名还要加上
/// 约 35 字节的前缀（`<32 位十六进制>_<序号>_`），留足余量。
const SAFE_NAME_MAX_BYTES: usize = 150;

/// 生成可安全用作文件名的片段：替换非法字符，按 **UTF-8 字节** 截到 150 字节以内并保留扩展名。
/// （以前按字符截 80 个：80 个汉字是 240 字节，加上前缀就超过 255 字节，Android 上建文件直接失败。）
pub fn safe_file_name(name: &str) -> String {
    let cleaned: String = name
        .chars()
        .map(|c| if c.is_alphanumeric() || c == '.' || c == '-' || c == '_' { c } else { '_' })
        .collect();
    let trimmed = cleaned.trim_matches('.');
    if trimmed.is_empty() {
        return "file".to_string();
    }
    if trimmed.len() <= SAFE_NAME_MAX_BYTES {
        return trimmed.to_string();
    }
    // 保留扩展名（最多 16 字节，超过就不当作扩展名）
    let (stem, ext) = match trimmed.rfind('.') {
        Some(i) if trimmed.len() - i <= 16 && i > 0 => (&trimmed[..i], &trimmed[i..]),
        _ => (trimmed, ""),
    };
    let mut end = (SAFE_NAME_MAX_BYTES - ext.len()).min(stem.len());
    while !stem.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}{ext}", &stem[..end])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn b64_padding_indifferent() {
        let d = b"\x01\x02\x03\x04";
        let padded = b64e(d);
        assert!(padded.ends_with("=="));
        assert_eq!(b64d(&padded).unwrap(), d);
        assert_eq!(b64d(padded.trim_end_matches('=')).unwrap(), d);
    }

    #[test]
    fn png_dims() {
        let mut png = b"\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR".to_vec();
        png.extend_from_slice(&640u32.to_be_bytes());
        png.extend_from_slice(&480u32.to_be_bytes());
        assert_eq!(image_dimensions(&png), (640, 480));
        assert_eq!(image_dimensions(b"nope"), (0, 0));
    }

    #[test]
    fn jpeg_dims() {
        // SOI, APP0(len 4), SOF0
        let mut j = vec![0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x00, 0x00];
        j.extend_from_slice(&[0xFF, 0xC0, 0x00, 0x11, 0x08, 0x01, 0xE0, 0x02, 0x80, 0x03]);
        j.extend_from_slice(&[0u8; 16]);
        assert_eq!(image_dimensions(&j), (640, 480));
    }

    #[test]
    fn safe_names() {
        assert_eq!(safe_file_name("../../etc/passwd"), "_.._etc_passwd");
        assert_eq!(safe_file_name("照片 1.jpg"), "照片_1.jpg");
        assert_eq!(safe_file_name(""), "file");
    }

    /// B3：长中文文件名按字节截断（整字符边界）并保留扩展名，加上缓存前缀后不超过 255 字节。
    #[test]
    fn long_cjk_names_fit_in_255_bytes() {
        let name = format!("{}.pdf", "报告".repeat(60)); // 120 个汉字 = 360 字节
        let safe = safe_file_name(&name);
        assert!(safe.len() <= 150, "{}", safe.len());
        assert!(safe.ends_with(".pdf"), "保留扩展名: {safe}");
        assert!(safe.starts_with("报告报告"));
        assert!(35 + safe.len() <= 255);
        // 73 个汉字：以前正好越界
        let s73 = safe_file_name(&"汉".repeat(73));
        assert!(s73.len() <= 150 && s73.chars().all(|c| c == '汉'));
        // emoji（4 字节）与没有扩展名、超长「扩展名」的情况
        assert!(safe_file_name(&"😀".repeat(100)).len() <= 150);
        assert!(safe_file_name(&format!("a.{}", "x".repeat(300))).len() <= 150);
        // 短文件名不变
        assert_eq!(safe_file_name("短名字.jpg"), "短名字.jpg");
        // 真的能建出文件
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join(format!("{}_0_{}", "a".repeat(32), safe));
        std::fs::write(&p, b"x").unwrap();
    }
}
