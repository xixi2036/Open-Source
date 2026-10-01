//! 聊天附件：发送（本地副本 → 分块流式加密到临时文件 → 流式上传 → 发送）与下载（流式落盘并校验 digest →
//! 流式解密到临时文件 → 改名）、本地缓存。内存占用与附件大小无关（格式 v2，见 `crypto::attachment`）。

use std::path::{Path, PathBuf};

use super::Inner;
use crate::content::{Content, ContentBody, WireAttachment};
use crate::crypto::attachment::{self, FORMAT_V1, FORMAT_V2};
use crate::error::{HushError, Result};
use crate::store::{MsgRow, StoredAttachment, StoredBody, StoredMessage};
use crate::types::*;
use crate::util::{b64d, b64e, safe_file_name};

/// 附件大小上限
const MAX_ATTACHMENT_BYTES: u64 = 100 * 1024 * 1024;
/// 旧格式 v1 附件（整文件读进内存解密）的大小上限（B2）。发送方可以自报 `format=1`，
/// 不设上限的话一条消息就能让接收方把 100MB 读进内存（移动端直接被系统杀掉）。
const MAX_V1_ATTACHMENT_BYTES: u64 = 25 * 1024 * 1024;
/// 下载时允许密文比按 `size` 算出的长度多出的余量（B2）
const DOWNLOAD_SLACK_BYTES: u64 = 64 * 1024;

/// 按消息里声明的明文大小与格式算出下载的密文最多多大（B2）。超过附件上限、或 v1 超过 25MB 时拒绝。
fn max_cipher_len(size: u64, format: u32) -> Result<u64> {
    if size > MAX_ATTACHMENT_BYTES {
        return Err(HushError::invalid("附件超过 100MB"));
    }
    let cipher = match format {
        FORMAT_V2 => attachment::v2_cipher_len(size),
        FORMAT_V1 if size > MAX_V1_ATTACHMENT_BYTES => return Err(HushError::invalid("旧格式附件超过 25MB，无法下载")),
        // v1：nonce(12) ‖ 密文 ‖ tag(16)
        FORMAT_V1 => size + 28,
        other => return Err(HushError::invalid(format!("不支持的附件格式 {other}"))),
    };
    Ok(cipher + DOWNLOAD_SLACK_BYTES)
}

/// 附件收发过程中的临时文件前缀（位于 `files/`，进程被杀时遗留的由定期清理删除）。
pub(crate) const TEMP_PREFIXES: [&str; 3] = [".enc-", ".dl-", ".dec-"];

/// 删除临时文件的守卫（成功改名后文件已不在原处，删除会无害地失败）。
struct TempFile(PathBuf);

impl Drop for TempFile {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}

/// 在阻塞线程池上执行文件 IO + 加解密。
async fn blocking<T: Send + 'static>(f: impl FnOnce() -> Result<T> + Send + 'static) -> Result<T> {
    tokio::task::spawn_blocking(f).await.map_err(|e| HushError::internal(format!("任务异常: {e}")))?
}

/// 把明文文件流式加密（v2）到 `dst`。
async fn encrypt_file(src: PathBuf, dst: PathBuf) -> Result<attachment::StreamEncrypted> {
    blocking(move || {
        let r = std::io::BufReader::new(std::fs::File::open(&src)?);
        let w = std::io::BufWriter::new(std::fs::File::create(&dst)?);
        attachment::encrypt_stream(r, w)
    })
    .await
}

/// 把已校验 digest 的密文文件按 `format` 解密到 `dst`。
async fn decrypt_file(format: u32, key: Vec<u8>, digest: Vec<u8>, src: PathBuf, dst: PathBuf) -> Result<()> {
    blocking(move || match format {
        FORMAT_V2 => {
            let r = std::io::BufReader::new(std::fs::File::open(&src)?);
            let f = std::fs::File::create(&dst)?;
            let mut w = std::io::BufWriter::new(f);
            attachment::decrypt_stream(&key, r, &mut w)?;
            let f = w.into_inner().map_err(|e| HushError::storage(e.error()))?;
            f.sync_all()?;
            Ok(())
        }
        // 旧格式是整文件一次 GCM，只能整块解密（旧附件本来就在内存里处理，大小有限）
        FORMAT_V1 => {
            let plain = attachment::decrypt_owned(&key, std::fs::read(&src)?, &digest)?;
            std::fs::write(&dst, plain)?;
            Ok(())
        }
        other => Err(HushError::invalid(format!("不支持的附件格式 {other}"))),
    })
    .await
}

impl Inner {
    pub(crate) async fn send_attachment(
        &self,
        conv_id: &str,
        file_path: &str,
        mime: String,
        kind: AttachmentKind,
        caption: Option<String>,
        meta: AttachmentMeta,
    ) -> Result<MessageItem> {
        let conv = self.ensure_conv(conv_id)?;
        // 只取大小并复制文件，不把整个文件读进内存（加密上传时再读一次，且就地加密）
        let size = tokio::fs::metadata(file_path).await.map_err(|e| HushError::invalid(format!("读取文件失败: {e}")))?.len();
        // 服务端按密文大小限制 100MB（v2 每 64KiB 多 16 字节 tag）
        if attachment::v2_cipher_len(size) > MAX_ATTACHMENT_BYTES {
            return Err(HushError::invalid("附件超过 100MB"));
        }
        let file_name = meta
            .file_name
            .clone()
            .filter(|n| !n.is_empty())
            .or_else(|| Path::new(file_path).file_name().map(|n| n.to_string_lossy().to_string()))
            .unwrap_or_else(|| "file".into());
        let mut content =
            Content::new(ContentBody::Text { text: String::new(), quote: None, mentions: vec![] }, Self::group_of(conv_id), conv.expire_secs);
        content.expire_set_at = self.timer_set_at(conv_id);
        // 发送方本地保留一份明文副本
        let local = self.attachment_path(&content.id, 0, &file_name);
        tokio::fs::copy(file_path, &local).await.map_err(|e| HushError::invalid(format!("读取文件失败: {e}")))?;
        let wire = WireAttachment {
            id: String::new(),
            key: String::new(),
            digest: String::new(),
            size,
            mime,
            file_name,
            kind,
            width: meta.width,
            height: meta.height,
            duration_ms: meta.duration_ms,
            thumbnail: meta.thumbnail.as_ref().filter(|t| !t.is_empty()).map(b64e),
            format: FORMAT_V2,
        };
        let caption = caption.filter(|c| !c.is_empty());
        let body = StoredBody::Attachments {
            items: vec![StoredAttachment { wire, local_path: Some(local.to_string_lossy().to_string()) }],
            caption,
        };
        self.clear_draft_on_send(conv_id);
        let row = self.insert_outgoing(conv_id, &content, StoredMessage::plain(body, None))?;
        self.upload_and_send(row).await
    }

    /// 加密上传尚未上传的附件，然后发送。
    ///
    /// 上传结果写回时基于库里最新的行（A3）。上传途中消息被本地删除或撤回（B4）就此停止，**不再发送**
    /// （`delete_local` 因此可以当作「取消发送」用；已上传的密文留在服务端，没有人拿得到密钥，到期回收）。
    pub(super) async fn upload_and_send(&self, row: MsgRow) -> Result<MessageItem> {
        let api = self.api()?;
        let id = row.id.clone();
        // (附件下标, 服务端 id, key, digest, 实际加密的字节数)
        let mut uploaded: Vec<(usize, String, String, String, u64)> = Vec::new();
        let upload: Result<()> = async {
            if let StoredBody::Attachments { items, .. } = &row.msg.body {
                for (i, it) in items.iter().enumerate().filter(|(_, a)| a.wire.id.is_empty()) {
                    let path = it.local_path.clone().ok_or_else(|| HushError::invalid("缺少本地文件"))?;
                    // 分块流式加密到临时文件，再以流的形式上传：内存占用与附件大小无关
                    let tmp = TempFile(self.temp_path(".enc-"));
                    let enc = encrypt_file(PathBuf::from(path), tmp.0.clone()).await?;
                    let (att_id, _) = api.upload_file(&tmp.0, enc.cipher_len).await?;
                    // 以实际加密的字节数为准（本地副本就是被加密的文件）
                    uploaded.push((i, att_id, b64e(enc.key), b64e(enc.digest), enc.plain_len));
                }
            }
            Ok(())
        }
        .await;
        let failed = upload.is_err();
        let cur = self.store.modify_message(&id, |r| {
            if r.deleted {
                return false;
            }
            if let StoredBody::Attachments { items, .. } = &mut r.msg.body {
                for (i, att_id, key, digest, plain_len) in &uploaded {
                    if let Some(it) = items.get_mut(*i) {
                        it.wire.id = att_id.clone();
                        it.wire.key = key.clone();
                        it.wire.digest = digest.clone();
                        it.wire.format = FORMAT_V2;
                        it.wire.size = *plain_len;
                    }
                }
            }
            r.status = if failed { MessageStatus::Failed } else { MessageStatus::Sending };
            true
        })?;
        let Some(row) = cur else {
            tracing::info!("消息 {id} 在附件上传途中已被删除或撤回，不再发送");
            return Err(upload.err().unwrap_or_else(|| HushError::invalid("消息已删除，发送已取消")));
        };
        if let Err(e) = upload {
            self.emit(CoreEvent::MessageUpdated { conv_id: row.conv_id.clone(), message: self.msg_item(&row) });
            return Err(e);
        }
        let content = self.content_for(&row)?;
        self.finish_send(row, content).await
    }

    /// 附件明文缓存路径：`files/<hash(消息id)>_<序号>_<文件名>`。用消息 id 的哈希而不是替换非法字符后的 id，
    /// 否则派生 id `bob:X` 与对方构造的内容 id `bob_X` 会映射到同一个文件，互相覆盖缓存。
    pub(crate) fn attachment_path(&self, message_id: &str, index: u32, file_name: &str) -> PathBuf {
        use sha2::{Digest, Sha256};
        let h = Sha256::digest(message_id.as_bytes());
        let tag: String = h[..16].iter().map(|b| format!("{b:02x}")).collect();
        self.files_dir.join(format!("{tag}_{index}_{}", safe_file_name(file_name)))
    }

    pub(crate) async fn download_attachment(&self, message_id: &str, index: u32) -> Result<String> {
        let row = self.store.get_message(message_id)?.ok_or_else(|| HushError::not_found("消息"))?;
        let item = match &row.msg.body {
            StoredBody::Attachments { items, .. } => {
                items.get(index as usize).cloned().ok_or_else(|| HushError::invalid("附件下标越界"))?
            }
            _ => return Err(HushError::invalid("该消息没有附件")),
        };
        if let Some(p) = &item.local_path {
            if Path::new(p).exists() {
                return Ok(p.clone());
            }
        }
        let key = b64d(&item.wire.key)?;
        let digest = b64d(&item.wire.digest)?;
        // 密文流式落盘并同时计算 SHA-256，先校验 digest，再流式解密到临时文件，成功后改名
        // B2：下载量不超过消息里声明的大小对应的密文长度（服务端 / 发送方不能让我们无限写盘），
        // 旧格式 v1 要整文件进内存解密，另有 25MB 上限
        let max = max_cipher_len(item.wire.size, item.wire.format)?;
        let enc = TempFile(self.temp_path(".dl-"));
        let (got, _) = self.api()?.download_to(&item.wire.id, &enc.0, max).await?;
        if got.as_slice() != digest.as_slice() {
            return Err(HushError::crypto("附件 digest 校验失败"));
        }
        let dec = TempFile(self.temp_path(".dec-"));
        decrypt_file(item.wire.format, key, digest, enc.0.clone(), dec.0.clone()).await?;
        let path = self.attachment_path(message_id, index, &item.wire.file_name);
        tokio::fs::rename(&dec.0, &path).await?;
        let p = path.to_string_lossy().to_string();
        // 只改这一个附件的本地路径，基于库里最新的行（A3）。下载期间消息被撤回 / 删除：不留下明文文件
        let updated = self.store.modify_message(message_id, |r| {
            if r.deleted {
                return false;
            }
            match &mut r.msg.body {
                StoredBody::Attachments { items, .. } => match items.get_mut(index as usize) {
                    Some(it) => {
                        it.local_path = Some(p.clone());
                        true
                    }
                    None => false,
                },
                _ => false,
            }
        })?;
        match updated {
            Some(row) => {
                self.emit(CoreEvent::MessageUpdated { conv_id: row.conv_id.clone(), message: self.msg_item(&row) });
                Ok(p)
            }
            None => {
                let _ = tokio::fs::remove_file(&path).await;
                Err(HushError::not_found("消息已删除"))
            }
        }
    }

    /// 附件收发用的临时文件路径（`files/<前缀><uuid>`）。
    fn temp_path(&self, prefix: &str) -> PathBuf {
        self.files_dir.join(format!("{prefix}{}", crate::util::new_id()))
    }

    pub(super) fn remove_local_files(&self, row: &MsgRow) {
        if let StoredBody::Attachments { items, .. } = &row.msg.body {
            for it in items {
                if let Some(p) = &it.local_path {
                    if Path::new(p).starts_with(&self.files_dir) {
                        let _ = std::fs::remove_file(p);
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::client::test_inner;

    #[test]
    fn attachment_paths_do_not_collide() {
        let (inner, _dir) = test_inner();
        let a = inner.attachment_path("bob:X", 0, "a.jpg");
        let b = inner.attachment_path("bob_X", 0, "a.jpg");
        assert_ne!(a, b, "派生 id 与构造的内容 id 不能映射到同一文件");
        assert!(a.starts_with(&inner.files_dir));
        assert!(a.to_string_lossy().ends_with("_0_a.jpg"));
    }

    type Shared = Arc<std::sync::Mutex<Vec<u8>>>;

    /// 上传桩：记录收到的请求体；下载返回 `served` 中的附件密文。
    fn blob_stub(uploaded: Shared, served: Shared) -> crate::client::stub::Handler {
        Arc::new(move |m, path, body| match (m, path) {
            ("POST", "/v1/attachments") => {
                *uploaded.lock().unwrap() = body.to_vec();
                (200, format!(r#"{{"id":"att1","size":{}}}"#, body.len()))
            }
            ("GET", "/v1/attachments/att1") => (200, crate::client::stub::binary(&served.lock().unwrap())),
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        })
    }

    fn file_msg(inner: &Inner, id: &str, wire: WireAttachment) {
        let mut row = MsgRow {
            id: id.into(),
            conv_id: "u:bob".into(),
            sender_id: "bob".into(),
            outgoing: false,
            sent_at: 1,
            server_ts: 1,
            sort_ts: 1,
            status: MessageStatus::Delivered,
            msg: StoredMessage::plain(StoredBody::Attachments { items: vec![StoredAttachment { wire, local_path: None }], caption: None }, None),
            expire_secs: 0,
            expires_at: None,
            deleted: false,
            read: false,
            mentions_me: false,
        };
        inner.store.ensure_conv("u:bob", 0).unwrap();
        inner.store.insert_message(&mut row).unwrap();
    }

    fn wire(key: &[u8], digest: &[u8], size: u64, format: u32) -> WireAttachment {
        WireAttachment {
            id: "att1".into(),
            key: b64e(key),
            digest: b64e(digest),
            size,
            mime: "application/octet-stream".into(),
            file_name: "f.bin".into(),
            kind: AttachmentKind::File,
            width: 0,
            height: 0,
            duration_ms: 0,
            thumbnail: None,
            format,
        }
    }

    fn no_temp_files(inner: &Inner) -> bool {
        std::fs::read_dir(&inner.files_dir)
            .unwrap()
            .flatten()
            .all(|e| !TEMP_PREFIXES.iter().any(|p| e.file_name().to_string_lossy().starts_with(p)))
    }

    /// 附件（v2）：流式加密上传（带 Content-Length）；上传的密文能按消息里的密钥 / digest 流式解密；
    /// 再经下载接口取回、校验、解密得到原文件。不留临时文件。
    #[tokio::test]
    async fn attachment_v2_upload_download_roundtrip() {
        let (up, served): (Shared, Shared) = Default::default();
        let srv = crate::client::stub::start(blob_stub(up.clone(), served.clone())).await;
        let (inner, dir) = crate::client::test_inner_at(&srv.url);
        let src = dir.path().join("photo.bin");
        let data: Vec<u8> = (0..(3 * attachment::CHUNK_LEN as u32 + 999)).map(|i| (i % 253) as u8).collect();
        std::fs::write(&src, &data).unwrap();
        let meta = AttachmentMeta { file_name: None, width: 0, height: 0, duration_ms: 0, thumbnail: None };
        // 没有账户：消息发送失败，但附件已上传
        let r = inner
            .send_attachment("u:bob", &src.to_string_lossy(), "application/octet-stream".into(), AttachmentKind::File, None, meta)
            .await;
        assert!(r.is_err());
        let row = inner.store.messages("u:bob", None, 10).unwrap().pop().unwrap();
        assert_eq!(row.status, MessageStatus::Failed);
        let StoredBody::Attachments { items, .. } = &row.msg.body else { panic!("应为附件消息") };
        let it = &items[0];
        assert_eq!((it.wire.id.as_str(), it.wire.format, it.wire.size), ("att1", FORMAT_V2, data.len() as u64));
        assert_eq!(std::fs::read(it.local_path.as_ref().unwrap()).unwrap(), data, "本地明文副本");
        let blob = up.lock().unwrap().clone();
        assert_eq!(blob.len() as u64, attachment::v2_cipher_len(data.len() as u64), "按 Content-Length 完整上传");
        let key = b64d(&it.wire.key).unwrap();
        let mut plain = Vec::new();
        attachment::decrypt_stream(&key, &blob[..], &mut plain).unwrap();
        assert_eq!(plain, data);
        assert!(no_temp_files(&inner));

        // 接收方：下载、校验、解密
        *served.lock().unwrap() = blob;
        file_msg(&inner, "in1", wire(&key, &b64d(&it.wire.digest).unwrap(), data.len() as u64, FORMAT_V2));
        let p = inner.download_attachment("in1", 0).await.unwrap();
        assert_eq!(std::fs::read(&p).unwrap(), data);
        assert!(no_temp_files(&inner));
    }

    /// 旧格式 v1 附件（没有 format 字段的旧消息）仍能下载解密；digest 不符或密文被篡改时失败且不留文件。
    #[tokio::test]
    async fn attachment_v1_compat_and_tamper_rejected() {
        let (up, served): (Shared, Shared) = Default::default();
        let srv = crate::client::stub::start(blob_stub(up, served.clone())).await;
        let (inner, _dir) = crate::client::test_inner_at(&srv.url);
        let data = b"legacy attachment bytes".repeat(1000);
        let e = attachment::encrypt_v1(&data);
        *served.lock().unwrap() = e.blob.clone();
        // 旧消息正文里没有 format：反序列化为 1
        let mut w = serde_json::to_value(wire(&e.key, &e.digest, data.len() as u64, FORMAT_V1)).unwrap();
        w.as_object_mut().unwrap().remove("format");
        file_msg(&inner, "old", serde_json::from_value(w).unwrap());
        let p = inner.download_attachment("old", 0).await.unwrap();
        assert_eq!(std::fs::read(&p).unwrap(), data);

        // 篡改（digest 同步改掉，由 GCM 拒绝）
        let mut bad = e.blob.clone();
        bad[30] ^= 1;
        *served.lock().unwrap() = bad.clone();
        use sha2::Digest;
        let d2: [u8; 32] = sha2::Sha256::digest(&bad).into();
        file_msg(&inner, "bad", wire(&e.key, &d2, data.len() as u64, FORMAT_V1));
        assert!(inner.download_attachment("bad", 0).await.is_err());
        // digest 不符
        file_msg(&inner, "bad2", wire(&e.key, &[0u8; 32], data.len() as u64, FORMAT_V1));
        assert!(inner.download_attachment("bad2", 0).await.is_err());
        assert!(!inner.attachment_path("bad", 0, "f.bin").exists());
        assert!(no_temp_files(&inner));
    }

    /// B2：下载量受消息里声明的大小约束；旧格式 v1 超过 25MB 直接拒绝（不发请求、不进内存）。
    #[tokio::test]
    async fn download_bounded_by_declared_size() {
        let (up, served): (Shared, Shared) = Default::default();
        let srv = crate::client::stub::start(blob_stub(up, served.clone())).await;
        let (inner, _dir) = crate::client::test_inner_at(&srv.url);
        // 发送方自报 format=1、60MB：拒绝，且没有任何下载请求
        file_msg(&inner, "v1-big", wire(&[1u8; 32], &[0u8; 32], 60 * 1024 * 1024, FORMAT_V1));
        let e = inner.download_attachment("v1-big", 0).await.unwrap_err();
        assert!(matches!(e, HushError::InvalidInput { .. }), "{e:?}");
        assert_eq!(srv.count("GET", "/v1/attachments/"), 0);
        // 声明 100 字节，服务端却给 300KB：中止
        *served.lock().unwrap() = vec![9u8; 300 * 1024];
        use sha2::Digest;
        let d: [u8; 32] = sha2::Sha256::digest(vec![9u8; 300 * 1024]).into();
        for (id, format) in [("v2-lie", FORMAT_V2), ("v1-lie", FORMAT_V1)] {
            file_msg(&inner, id, wire(&[1u8; 32], &d, 100, format));
            let e = inner.download_attachment(id, 0).await.unwrap_err();
            assert!(matches!(&e, HushError::InvalidInput { message } if message.contains("声明")), "{e:?}");
        }
        // 声明超过 100MB / 未知格式
        file_msg(&inner, "huge", wire(&[1u8; 32], &d, u64::MAX, FORMAT_V2));
        assert!(inner.download_attachment("huge", 0).await.is_err());
        file_msg(&inner, "fmt9", wire(&[1u8; 32], &d, 100, 9));
        assert!(inner.download_attachment("fmt9", 0).await.is_err());
        assert!(no_temp_files(&inner));
        assert_eq!(max_cipher_len(0, FORMAT_V2).unwrap(), attachment::v2_cipher_len(0) + DOWNLOAD_SLACK_BYTES);
        assert!(max_cipher_len(MAX_V1_ATTACHMENT_BYTES, FORMAT_V1).is_ok());
    }

    /// B4：附件上传途中消息被本地删除——上传结束后不再发送（不 claim、不 POST 消息），也不为它发事件。
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn attachment_deleted_during_upload_is_not_sent() {
        // 上传请求卡在服务端，直到测试删掉消息后放行
        let (release, gate) = std::sync::mpsc::channel::<()>();
        let gate = std::sync::Mutex::new(gate);
        let srv = crate::client::stub::start(Arc::new(move |m, path, body| match (m, path) {
            ("POST", "/v1/attachments") => {
                let _ = gate.lock().unwrap().recv_timeout(std::time::Duration::from_secs(10));
                (200, format!(r#"{{"id":"att1","size":{}}}"#, body.len()))
            }
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let (inner, dir) = crate::client::test_inner_at(&srv.url);
        crate::client::published_account(&inner);
        let src = dir.path().join("a.bin");
        std::fs::write(&src, vec![3u8; 10_000]).unwrap();
        let i2 = inner.clone();
        let path = src.to_string_lossy().to_string();
        let send = tokio::spawn(async move {
            let meta = AttachmentMeta { file_name: None, width: 0, height: 0, duration_ms: 0, thumbnail: None };
            i2.send_attachment("u:bob", &path, "application/octet-stream".into(), AttachmentKind::File, None, meta).await
        });
        // 等消息入库、上传开始
        let id = loop {
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
            if let Some(m) = inner.store.messages("u:bob", None, 1).unwrap().pop() {
                if srv.count("POST", "/v1/attachments") == 1 {
                    break m.id;
                }
            }
        };
        inner.delete_local(&id).unwrap();
        release.send(()).unwrap();
        let r = send.await.unwrap();
        assert!(matches!(r, Err(HushError::InvalidInput { .. })), "已删除的消息应取消发送: {r:?}");
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        assert_eq!(srv.count("POST", "/v1/keys/claim/"), 0, "不应再去 claim 对方密钥");
        assert_eq!(srv.count("POST", "/v1/messages"), 0, "消息不应发出");
        assert!(inner.store.get_message(&id).unwrap().is_none());
        assert!(no_temp_files(&inner));
    }
}
