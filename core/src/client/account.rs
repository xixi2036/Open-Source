//! 账号、资料、好友与拉黑。

use std::path::Path;
use std::sync::Arc;

use serde_json::json;

use super::{AuthState, Inner};
use crate::api::{self, Api, Empty, VerifyResp};
use crate::error::{HushError, Result};
use crate::types::*;
use crate::util::safe_file_name;

/// 明文媒体（头像、朋友圈图片）下载进内存的大小上限（B2）
const MAX_MEDIA_BYTES: u64 = 25 * 1024 * 1024;

impl Inner {
    pub(crate) async fn request_code(&self, server_url: &str, email: &str) -> Result<()> {
        let base = api::normalize_base(server_url, self.allow_insecure_http())?;
        let email = email.trim();
        if !email.contains('@') {
            return Err(HushError::invalid("邮箱格式不正确"));
        }
        let a = Api::new(self.http.clone(), base, None);
        let _: Empty = a.post("/v1/auth/code", &json!({ "email": email })).await?;
        Ok(())
    }

    pub(crate) async fn verify_code(self: &Arc<Self>, server_url: &str, email: &str, code: &str) -> Result<MyAccount> {
        let base = api::normalize_base(server_url, self.allow_insecure_http())?;
        let email = email.trim().to_string();
        let a = Api::new(self.http.clone(), base.clone(), None);
        let platform = if self.platform.is_empty() { "android".to_string() } else { self.platform.clone() };
        let r: VerifyResp = a
            .post(
                "/v1/auth/verify",
                &json!({"email": email, "code": code.trim(), "device_name": self.device_name, "platform": platform}),
            )
            .await?;

        // 停掉旧连接；新设备代：旧的加密会话全部作废
        self.stop();
        let prev = self.auth.read().unwrap_or_else(|e| e.into_inner()).clone();
        let prev_uid = match prev {
            Some(p) => Some(p.user_id),
            None => self.store.kv_get_json::<String>("last_user_id")?,
        };
        if prev_uid.as_deref() != Some(r.user_id.as_str()) {
            self.store.wipe_all()?;
        } else {
            self.store.wipe_crypto()?;
        }
        self.set_auth(Some(AuthState { server_url: base.clone(), token: r.token.clone(), user_id: r.user_id.clone(), email: email.clone() }))?;
        self.store.kv_set_json("last_user_id", &r.user_id)?;

        let api = self.api()?;
        if let Err(e) = self.create_and_upload_account(&api).await {
            self.set_auth(None)?;
            return Err(e);
        }
        // B5：到这里登录已经完成（token 已保存、密钥已上传）。取资料失败不能再返回 Err——那样调用方以为没登录上，
        // 而本地已是登录态。用缓存里的资料（同一用户重新登录）或只有 id 的空资料顶上，之后照常刷新。
        let me: Profile = match api.get::<Profile>("/v1/me").await {
            Ok(me) => {
                self.store.put_profile_authoritative(&me)?;
                me
            }
            Err(e) => {
                tracing::warn!("登录后获取个人资料失败，先用本地缓存: {e}");
                let cached = self.store.get_profile(&r.user_id).ok().flatten();
                cached.unwrap_or(Profile { id: r.user_id.clone(), email: Some(email.clone()), ..Default::default() })
            }
        };
        let email = me.email.clone().filter(|e| !e.is_empty()).unwrap_or(email);
        let current = self.auth.read().unwrap_or_else(|e| e.into_inner()).clone();
        if let Some(mut a) = current {
            a.email = email.clone();
            self.set_auth(Some(a))?;
        }
        Ok(MyAccount { user_id: r.user_id, email, server_url: base, profile: me, is_new: r.is_new })
    }

    pub(crate) fn my_account(&self) -> Option<MyAccount> {
        let a = self.auth.read().unwrap_or_else(|e| e.into_inner()).clone()?;
        let profile = self
            .store
            .get_profile(&a.user_id)
            .ok()
            .flatten()
            .unwrap_or(Profile { id: a.user_id.clone(), ..Default::default() });
        Some(MyAccount { user_id: a.user_id, email: a.email, server_url: a.server_url, profile, is_new: false })
    }

    /// 退出登录：作废 token，停止连接，清空本地数据。
    pub(crate) async fn logout(&self) -> Result<()> {
        // 不经 `api()`：token 若已过期，这里的 401 不应再触发一次 LoggedOut(session_expired)
        if let Ok(a) = self.auth() {
            // B1：公网明文地址（旧版本保存的登录态）不再带着 token 发请求，直接清本地
            if api::ensure_secure(&a.server_url, self.allow_insecure_http()).is_ok() {
                let api = Api::new(self.http.clone(), a.server_url, Some(a.token));
                if let Err(e) = api.post_empty::<Empty>("/v1/auth/logout").await {
                    tracing::warn!("服务端登出失败（忽略）: {e}");
                }
            }
        }
        self.stop();
        self.set_auth(None)?;
        self.store.wipe_all()?;
        // B8：WAL 截断并 VACUUM，已删除的数据不留在库文件里（失败只记日志，不影响登出）
        if let Err(e) = self.store.compact_after_wipe() {
            tracing::warn!("登出后压缩本地库失败: {e}");
        }
        let _ = std::fs::remove_dir_all(&self.files_dir);
        std::fs::create_dir_all(self.files_dir.join("media"))?;
        Ok(())
    }

    /// 服务端通知被挤下线 / token 失效。
    pub(crate) fn on_logged_out(&self, reason: &str) {
        let _ = self.set_auth(None);
        self.finish_logged_out(reason, true);
    }

    /// 带 `token` 的请求收到 401（或 WS 握手 401）：token 已失效（例如 90 天未使用）。
    /// 原因为 `session_expired`；token 已不是当前登录态时忽略（旧请求晚到）。
    ///
    /// B1：**只清 token，不清加密状态**。401 只是一个没有完整性保护之外任何凭据的 HTTP 状态码——
    /// 明文链路上的中间人、出了问题的网关都能造出来；以前一个 401 就把账户密钥与全部会话销毁了。
    /// 加密状态留到下次 `verify_code`：同一用户重新登录时按原有逻辑重建（服务端此时会换新的设备代）。
    pub(crate) fn on_session_expired(&self, token: &str) {
        if self.clear_auth_if(token) {
            tracing::warn!("登录已过期（401），清除本地登录态（加密状态保留到重新登录）");
            self.finish_logged_out("session_expired", false);
        }
    }

    /// 登录态已清除之后：停止后台任务、发出 LoggedOut。`wipe_crypto`：同时清加密状态（保留消息）——
    /// 只用于服务端经已认证的连接明确通知的登出（4001 在别处登录、`logged_out` 帧）。
    fn finish_logged_out(&self, reason: &str, wipe_crypto: bool) {
        if wipe_crypto {
            let _ = self.store.wipe_crypto();
        }
        self.signal_stop();
        self.set_connection_state(ConnectionState::Disconnected);
        self.emit(CoreEvent::LoggedOut { reason: reason.to_string() });
    }

    // ================= 资料 =================

    pub(crate) async fn update_profile(&self, name: Option<String>, about: Option<String>, avatar_path: Option<String>) -> Result<Profile> {
        let api = self.api()?;
        let mut body = serde_json::Map::new();
        if let Some(n) = name {
            body.insert("name".into(), json!(n));
        }
        if let Some(a) = about {
            body.insert("about".into(), json!(a));
        }
        if let Some(p) = avatar_path.filter(|p| !p.is_empty()) {
            let data = tokio::fs::read(&p).await.map_err(|e| HushError::invalid(format!("读取头像失败: {e}")))?;
            body.insert("avatar_id".into(), json!(self.upload_media(&api, data).await?));
        }
        let p: Profile = api.put("/v1/me/profile", &body).await?;
        self.store.put_profile(&p)?;
        Ok(p)
    }

    pub(crate) async fn get_user(&self, uid: &str) -> Result<Profile> {
        let p: Profile = self.api()?.get(&format!("/v1/users/{}", api::enc(uid))).await?;
        // 服务端按查看者判断是否返回邮箱，这里的结果是权威的：没有邮箱时清掉缓存里的旧邮箱
        self.store.put_profile_authoritative(&p)?;
        Ok(p)
    }

    pub(crate) async fn search_user_by_email(&self, email: &str) -> Result<Option<Profile>> {
        let q: String = url::form_urlencoded::byte_serialize(email.trim().as_bytes()).collect();
        match self.api()?.get::<Profile>(&format!("/v1/users/search?email={q}")).await {
            // 搜索结果不带邮箱：缓存沿用已有邮箱（K2），返回合并后的资料，与 cached_profile 一致
            Ok(p) => Ok(Some(self.store.put_profile(&p)?)),
            Err(e) if e.is_server_code("not_found") => Ok(None),
            Err(e) => Err(e),
        }
    }

    pub(crate) fn media_path(&self, id: &str) -> std::path::PathBuf {
        self.files_dir.join("media").join(safe_file_name(id))
    }

    /// 上传明文媒体（头像、群头像、朋友圈图片）并放入本地缓存，返回附件 id。
    /// 先写临时文件再上传（数据移交给请求，不复制一份），成功后改名为缓存文件；缓存失败不影响上传结果。
    pub(crate) async fn upload_media(&self, api: &Api, data: Vec<u8>) -> Result<String> {
        let tmp = self.files_dir.join("media").join(format!(".upload-{}", crate::util::new_id()));
        let cached = tokio::fs::write(&tmp, &data).await.is_ok();
        let r = api.upload(data).await;
        match &r {
            Ok((id, _)) if cached => {
                if tokio::fs::rename(&tmp, self.media_path(id)).await.is_err() {
                    let _ = tokio::fs::remove_file(&tmp).await;
                }
            }
            _ => {
                let _ = tokio::fs::remove_file(&tmp).await;
            }
        }
        Ok(r?.0)
    }

    /// 下载明文附件（头像 / 朋友圈图片）并缓存。
    pub(crate) async fn fetch_media(&self, id: &str) -> Result<String> {
        if id.is_empty() {
            return Err(HushError::invalid("附件 id 为空"));
        }
        let path = self.media_path(id);
        if Path::new(&path).exists() {
            return Ok(path.to_string_lossy().to_string());
        }
        let data = self.api()?.download(id, MAX_MEDIA_BYTES).await?;
        let tmp = path.with_extension("part");
        tokio::fs::write(&tmp, &data).await?;
        tokio::fs::rename(&tmp, &path).await?;
        Ok(path.to_string_lossy().to_string())
    }

    // ================= 好友 =================

    pub(crate) async fn send_friend_request(&self, uid: &str, message: &str) -> Result<()> {
        let r: FriendRequest = self.api()?.post("/v1/friends/requests", &json!({"to_user_id": uid, "message": message})).await?;
        self.store.put_profile(&r.to)?;
        if r.status == "accepted" {
            self.refresh_friends().await?;
        }
        Ok(())
    }

    pub(crate) async fn list_friend_requests(&self) -> Result<FriendRequests> {
        let r: FriendRequests = self.api()?.get("/v1/friends/requests").await?;
        for fr in r.incoming.iter().chain(r.outgoing.iter()) {
            self.store.put_profile(&fr.from)?;
            self.store.put_profile(&fr.to)?;
        }
        Ok(r)
    }

    pub(crate) async fn accept_friend_request(&self, id: &str) -> Result<()> {
        let _: Empty = self.api()?.post_empty(&format!("/v1/friends/requests/{}/accept", api::enc(id))).await?;
        self.refresh_friends().await?;
        Ok(())
    }

    pub(crate) async fn reject_friend_request(&self, id: &str) -> Result<()> {
        let _: Empty = self.api()?.post_empty(&format!("/v1/friends/requests/{}/reject", api::enc(id))).await?;
        Ok(())
    }

    pub(crate) async fn refresh_friends(&self) -> Result<Vec<Profile>> {
        let list: Vec<Profile> = self.api()?.get("/v1/friends").await?;
        self.store.set_friends(&list)?;
        self.emit(CoreEvent::FriendsChanged);
        Ok(list)
    }

    pub(crate) fn friends(&self) -> Result<Vec<Profile>> {
        let mut out = Vec::new();
        for id in self.store.friend_ids()? {
            out.push(self.store.get_profile(&id)?.unwrap_or(Profile { id, ..Default::default() }));
        }
        out.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(out)
    }

    pub(crate) async fn remove_friend(&self, uid: &str) -> Result<()> {
        let _: Empty = self.api()?.delete(&format!("/v1/friends/{}", api::enc(uid))).await?;
        self.store.remove_friend(uid)?;
        self.emit(CoreEvent::FriendsChanged);
        Ok(())
    }

    pub(crate) async fn block(&self, uid: &str) -> Result<()> {
        let _: Empty = self.api()?.post_empty(&format!("/v1/blocks/{}", api::enc(uid))).await?;
        self.store.add_block(uid)?;
        self.emit(CoreEvent::FriendsChanged);
        Ok(())
    }

    pub(crate) async fn unblock(&self, uid: &str) -> Result<()> {
        let _: Empty = self.api()?.delete(&format!("/v1/blocks/{}", api::enc(uid))).await?;
        self.store.remove_block(uid)?;
        self.emit(CoreEvent::FriendsChanged);
        Ok(())
    }

    pub(crate) async fn refresh_blocks(&self) -> Result<Vec<String>> {
        let ids: Vec<String> = self.api()?.get("/v1/blocks").await?;
        self.store.set_blocks(&ids)?;
        Ok(ids)
    }
}

#[cfg(test)]
mod tests {
    use std::sync::mpsc;
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    use super::*;
    use crate::client::{stub, test_inner_at};

    struct Events(Mutex<mpsc::Sender<CoreEvent>>);
    impl EventListener for Events {
        fn on_event(&self, e: CoreEvent) {
            let _ = self.0.lock().unwrap().send(e);
        }
    }

    fn logged_out_reasons(rx: &mpsc::Receiver<CoreEvent>) -> Vec<String> {
        let mut v = vec![];
        while let Ok(e) = rx.recv_timeout(Duration::from_millis(300)) {
            if let CoreEvent::LoggedOut { reason } = e {
                v.push(reason);
            }
        }
        v
    }

    /// C-G / B1：任一接口 401 → 清除本地登录态，发出 LoggedOut(session_expired)，只发一次；
    /// 加密状态（账户密钥、会话）与消息都保留，留到重新登录时处理。
    #[tokio::test]
    async fn rest_401_expires_session() {
        let srv = stub::start(Arc::new(|_m, _p, _b| (401, r#"{"error":"unauthorized","message":"登录已过期"}"#.into()))).await;
        let (inner, _dir) = test_inner_at(&srv.url);
        crate::client::published_account(&inner);
        inner.insert_system("u:bob", "保留的消息").unwrap();
        let (tx, rx) = mpsc::channel();
        inner.set_listener(Arc::new(Events(Mutex::new(tx))));
        let err = inner.get_user("bob").await.unwrap_err();
        assert!(err.is_server_code("unauthorized"), "{err:?}");
        assert!(!inner.is_logged_in());
        assert!(inner.store.kv_get("auth").unwrap().is_none(), "持久化的登录态也清除");
        assert!(inner.store.load_account().unwrap().is_some(), "B1：一个 401 不能销毁加密状态");
        assert_eq!(inner.store.messages("u:bob", None, 10).unwrap().len(), 1, "消息保留");
        // 并发 / 之后的请求不会重复登出
        assert!(inner.get_user("bob").await.is_err());
        assert_eq!(logged_out_reasons(&rx), vec!["session_expired".to_string()]);
    }

    /// 旧登录态发出的请求晚到的 401 不能登出新的登录；登录前的接口（没有 token）401 也不触发登出。
    #[tokio::test]
    async fn stale_token_401_ignored() {
        let srv = stub::start(Arc::new(|_m, _p, _b| (401, r#"{"error":"unauthorized","message":"x"}"#.into()))).await;
        let (inner, _dir) = test_inner_at(&srv.url);
        let (tx, rx) = mpsc::channel();
        inner.set_listener(Arc::new(Events(Mutex::new(tx))));
        let old_api = inner.api().unwrap();
        let mut a = inner.auth().unwrap();
        a.token = "t2".into();
        inner.set_auth(Some(a)).unwrap();
        assert!(old_api.get::<Profile>("/v1/me").await.is_err());
        assert!(inner.is_logged_in(), "旧 token 的 401 不影响新登录");
        assert!(inner.verify_code(&srv.url, "a@b.c", "000000").await.is_err());
        assert!(inner.is_logged_in(), "没有 token 的请求 401 不触发登出");
        assert!(logged_out_reasons(&rx).is_empty());
    }

    /// C-C：/v1/me 的 email 进入 Profile（旧实现里外层的 email 字段会把它抢走）。
    #[test]
    fn me_profile_email_parsed() {
        let p: Profile = serde_json::from_str(r#"{"id":"u","name":"n","about":"","avatar_id":"","updated_at":1,"email":"a@x"}"#).unwrap();
        assert_eq!(p.email.as_deref(), Some("a@x"));
        let p: Profile = serde_json::from_str(r#"{"id":"u","name":"n","email":""}"#).unwrap();
        assert_eq!(p.email, None, "空串视为没有");
        assert!(!serde_json::to_string(&p).unwrap().contains("email"), "没有邮箱时不序列化");
    }

    /// B1：公网地址必须 https。登录入口直接拒绝明文公网地址（不发请求）；旧版本保存的公网 http 登录态不再带 token 发请求；
    /// 本机与内网地址照常可用；调试开关可以放开。
    #[tokio::test]
    async fn public_http_rejected_unless_allowed() {
        let (inner, _dir) = crate::client::test_inner();
        for url in ["http://hush.example.com", "http://8.8.8.8:8080"] {
            assert!(matches!(inner.request_code(url, "a@b.c").await, Err(HushError::InvalidInput { .. })), "{url}");
            assert!(matches!(inner.verify_code(url, "a@b.c", "000000").await, Err(HushError::InvalidInput { .. })));
        }
        assert!(inner.is_logged_in(), "被拒绝的登录尝试不影响现有登录态");
        // 内网地址：照常去连（这里没有服务端，得到的是网络错误而不是参数错误）
        assert!(matches!(inner.request_code("http://127.0.0.1:1", "a@b.c").await, Err(HushError::Network { .. })));
        assert!(inner.api().is_ok());
        // 旧版本保存的公网 http 登录态
        let mut a = inner.auth().unwrap();
        a.server_url = "http://hush.example.com".into();
        inner.set_auth(Some(a)).unwrap();
        assert!(matches!(inner.api(), Err(HushError::InvalidInput { .. })));
        assert!(matches!(inner.get_user("bob").await, Err(HushError::InvalidInput { .. })));
        assert!(inner.is_logged_in(), "不因此登出");
        inner.set_allow_insecure_http(true);
        assert!(inner.api().is_ok());
    }

    /// B5：密钥上传成功后 `/v1/me` 失败，`verify_code` 仍返回成功（登录已完成），不再出现「返回 Err 但已是登录态」。
    #[tokio::test]
    async fn verify_code_succeeds_when_profile_fetch_fails() {
        let srv = stub::start(Arc::new(|m, p, _b| match (m, p) {
            ("POST", "/v1/auth/verify") => (200, r#"{"user_id":"u-1","token":"tok-1","is_new":true}"#.into()),
            ("PUT", "/v1/keys") => (200, r#"{"one_time_key_count":100}"#.into()),
            ("GET", "/v1/me") => (503, "Service Unavailable".into()),
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let dir = tempfile::tempdir().unwrap();
        let inner = Inner::new_arc(dir.path().to_string_lossy().to_string(), vec![7u8; 32], "test".into(), "t".into()).unwrap();
        let acct = inner.verify_code(&srv.url, "me@x.y", "123456").await.expect("登录已完成，应返回成功");
        assert_eq!((acct.user_id.as_str(), acct.email.as_str(), acct.is_new), ("u-1", "me@x.y", true));
        assert_eq!(acct.profile.id, "u-1");
        assert!(inner.is_logged_in());
        assert!(inner.store.load_account().unwrap().is_some());
        assert_eq!(inner.my_account().unwrap().user_id, "u-1");
        // 密钥上传失败仍然是登录失败，且不留登录态
        let srv2 = stub::start(Arc::new(|m, p, _b| match (m, p) {
            ("POST", "/v1/auth/verify") => (200, r#"{"user_id":"u-2","token":"tok-2"}"#.into()),
            _ => (503, "x".into()),
        }))
        .await;
        let dir2 = tempfile::tempdir().unwrap();
        let inner2 = Inner::new_arc(dir2.path().to_string_lossy().to_string(), vec![7u8; 32], "test".into(), "t".into()).unwrap();
        assert!(inner2.verify_code(&srv2.url, "me@x.y", "123456").await.is_err());
        assert!(!inner2.is_logged_in());
    }

    /// B8：`secure_delete` 打开；登出后 WAL 被截断、库文件收缩。
    #[tokio::test]
    async fn logout_compacts_database() {
        let (inner, dir) = crate::client::test_inner();
        let secure: i64 = inner.store.debug_query_i64("PRAGMA secure_delete");
        assert_eq!(secure, 1);
        for i in 0..400 {
            inner.insert_system("u:bob", &format!("占空间的消息 {i} {}", "x".repeat(2000))).unwrap();
        }
        let db = dir.path().join("hush.sqlite");
        let wal = dir.path().join("hush.sqlite-wal");
        inner.store.debug_exec("PRAGMA wal_checkpoint(TRUNCATE)");
        let before = std::fs::metadata(&db).unwrap().len();
        assert!(before > 800_000, "{before}");
        inner.logout().await.unwrap();
        let after = std::fs::metadata(&db).unwrap().len();
        assert!(after < before / 4, "登出后库文件应收缩: {before} -> {after}");
        assert_eq!(std::fs::metadata(&wal).map(|m| m.len()).unwrap_or(0), 0, "WAL 应被截断");
        assert!(inner.store.messages("u:bob", None, 10).unwrap().is_empty());
    }
}
