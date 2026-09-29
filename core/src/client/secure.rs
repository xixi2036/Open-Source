//! 加密通道：Olm 发送与接收、Megolm 群发送与 room_key 分发、解密失败处理（session_reset）。
//! 密钥上传 / 补充见 `keys.rs`，对端身份与安全码见 `identity.rs`。

use std::collections::HashSet;
use std::sync::Arc;

use serde_json::json;
use vodozemac::olm::Account;

use super::{direct_conv, group_conv, Inner};
use crate::api::{self, ClaimResp, Envelope, GroupSendResp, OlmSendResp};
use crate::content::{Content, ContentBody};
use crate::crypto::{megolm, olm};
use crate::error::{HushError, Result};
use crate::store::{IdentityRec, InboxPlain, OutboundRec, PendingRec};
use crate::util::{b64d, b64e, now_ms};

const MEGOLM_MAX_MESSAGES: u32 = 100;
const MEGOLM_MAX_AGE_MS: i64 = 7 * 24 * 3600 * 1000;

/// 出站 Megolm 会话是否需要轮换：已发满 100 条、超过 7 天，或者**已分发名单里有人不在当前成员表中**。
/// 最后一条保证即使「删除出站会话」与并发发送交错、旧会话被写回，下一次发送也一定会轮换，
/// 被移出的成员拿不到之后的消息。
pub(crate) fn outbound_needs_rotation(out: &OutboundRec, recipients: &HashSet<String>, now: i64) -> bool {
    out.msg_count >= MEGOLM_MAX_MESSAGES
        || now - out.created_at >= MEGOLM_MAX_AGE_MS
        || out.shared_with.iter().any(|u| !recipients.contains(u))
}

/// 预先 claim 的结果（按用户）。
pub(crate) type Claims = std::collections::HashMap<String, Result<ClaimResp>>;

/// 解密后的明文及其上下文。
pub struct Incoming {
    pub from: String,
    pub content: Content,
    pub server_ts: i64,
    /// 经 Megolm 收到时为群 id
    pub via_group: Option<String>,
    /// 经 Olm 收到时所用会话 id
    pub olm_session_id: Option<String>,
    /// 来自积压（连接建立后 `queue_empty` 之前）或 HTTP 补拉：`MessageAdded.backlog`
    pub backlog: bool,
}

pub enum EnvelopeOutcome {
    Decrypted(Box<Incoming>),
    /// 无法解密（已记录占位消息）
    Undecryptable,
    /// 群消息缺密钥，已挂起
    Pending,
    /// 丢弃（拉黑等）
    Dropped,
}

impl Inner {
    // ================= Olm 发送 =================

    /// claim 对端的一次性密钥（网络请求，不持锁）并校验签名。
    async fn claim_key(&self, uid: &str) -> Result<ClaimResp> {
        let c: ClaimResp = self.api()?.post_empty(&format!("/v1/keys/claim/{}", api::enc(uid))).await?;
        olm::verify_signed_key(&c.signing_key, &c.one_time_key.key, &c.one_time_key.signature)?;
        Ok(c)
    }

    /// 为其中还没有 Olm 会话的对端并发预先 claim（在 crypto_lock 之外），结果交给随后持锁的 `olm_encrypt_locked`。
    /// 期间若对端的会话已由别的任务建立，预取的 OTK 会被浪费一个，对端下次补充即可。
    pub(crate) async fn prefetch_claims<'a>(&self, uids: impl IntoIterator<Item = &'a str>) -> Claims {
        use futures_util::StreamExt;
        let mut todo: Vec<String> = Vec::new();
        for u in uids {
            if !todo.iter().any(|t| t == u) && !self.store.has_olm_session(u).unwrap_or(true) {
                todo.push(u.to_string());
            }
        }
        futures_util::stream::iter(todo)
            .map(|u| async move {
                let r = self.claim_key(&u).await;
                (u, r)
            })
            .buffer_unordered(8)
            .collect()
            .await
    }

    /// 用 claim 结果建立出站会话，并按需记录 / 更新对端身份（调用方持有 crypto_lock）。
    fn create_outbound_session(&self, acc: &Account, uid: &str, c: ClaimResp) -> Result<olm::SessEntry> {
        let new_rec = IdentityRec { user_id: uid.to_string(), ..IdentityRec::from(&c) };
        match self.store.get_identity(uid)? {
            None => self.store.put_identity(&new_rec)?,
            Some(old) if old.identity_key != c.identity_key || old.signing_key != c.signing_key => {
                self.on_identity_changed(uid, Some(new_rec))?;
            }
            Some(old) if old.device_generation != c.device_generation => {
                self.store.put_identity(&IdentityRec { device_generation: c.device_generation, ..old })?;
            }
            _ => {}
        }
        let ik = olm::parse_curve25519(&c.identity_key)?;
        let otk = olm::parse_curve25519(&c.one_time_key.key)?;
        let session = olm::create_outbound(acc, ik, otk)?;
        Ok(olm::SessEntry { session, last_used: now_ms() })
    }

    /// 为某用户加密（调用方持有 crypto_lock）。返回 (type, body_b64)。
    /// 没有会话时使用 `claims` 里预取的密钥；预取里没有（例如预取之后会话被删除）才在锁内补一次 claim。
    async fn olm_encrypt_locked(&self, acc: &Account, uid: &str, plaintext: &[u8], claims: &mut Claims) -> Result<(u8, String)> {
        let mut sessions = self.store.load_olm_sessions(uid)?;
        let idx = match olm::best_session(&sessions) {
            Some(i) => i,
            None => {
                let c = match claims.remove(uid) {
                    Some(r) => r?,
                    None => self.claim_key(uid).await?,
                };
                sessions.push(self.create_outbound_session(acc, uid, c)?);
                sessions.len() - 1
            }
        };
        let entry = &mut sessions[idx];
        let (t, bytes) = olm::encrypt(&mut entry.session, plaintext)?;
        entry.last_used = now_ms();
        self.store.save_olm_session(uid, entry)?;
        if t == 0 {
            Self::stat_inc(&self.stats.olm_prekey_tx);
        } else {
            Self::stat_inc(&self.stats.olm_normal_tx);
        }
        Ok((t, b64e(bytes)))
    }

    /// 批量经 Olm 发送内容。返回与输入一一对应的结果（Ok(server_ts) 或错误）。
    pub(crate) async fn send_olm_batch(&self, items: Vec<(String, Content, bool)>) -> Result<Vec<Result<i64>>> {
        let api = self.api()?;
        let mut claims = self.prefetch_claims(items.iter().map(|i| i.0.as_str())).await;
        let mut results: Vec<Result<i64>> = Vec::with_capacity(items.len());
        let mut wire = Vec::new();
        let mut index = Vec::new();
        {
            let _g = self.crypto_lock.lock().await;
            let acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
            for (i, (uid, content, urgent)) in items.iter().enumerate() {
                match self.olm_encrypt_locked(&acc, uid, &content.to_bytes(), &mut claims).await {
                    Ok((t, body)) => {
                        wire.push(json!({"to": uid, "type": t, "body": body, "client_id": content.id, "urgent": urgent}));
                        index.push(i);
                        results.push(Err(HushError::internal("未发送")));
                    }
                    Err(e) => results.push(Err(e)),
                }
            }
        }
        if wire.is_empty() {
            return Ok(results);
        }
        let resp: OlmSendResp = api.post("/v1/messages", &json!({ "messages": wire })).await?;
        for (k, r) in resp.results.iter().enumerate() {
            // 服务端结果与请求同序；同时用 (to, client_id) 兜底匹配
            let i = if k < index.len() && items[index[k]].0 == r.to && items[index[k]].1.id == r.client_id {
                index[k]
            } else if let Some(&i) = index.iter().find(|&&i| items[i].0 == r.to && items[i].1.id == r.client_id) {
                i
            } else {
                continue;
            };
            results[i] = match r.error_code() {
                None => Ok(r.server_ts),
                Some(code) => Err(HushError::Server { code: code.clone(), message: format!("发送给 {} 失败", r.to) }),
            };
        }
        Ok(results)
    }

    pub(crate) async fn send_olm(&self, uid: &str, content: Content, urgent: bool) -> Result<i64> {
        self.send_olm_batch(vec![(uid.to_string(), content, urgent)])
            .await?
            .pop()
            .unwrap_or_else(|| Err(HushError::internal("无结果")))
    }

    // ================= Megolm 发送 =================

    pub(crate) async fn send_group(&self, gid: &str, content: Content, urgent: bool) -> Result<i64> {
        let api = self.api()?;
        let me = self.my_id()?;
        // 同一群的发送串行化：room_key 分发与群消息按顺序进行，避免并发发送给同一成员分发不同位置的密钥
        let _send_guard = self.group_send.lock(gid).await;

        // 0) 锁外：为可能需要 room_key、且还没有 Olm 会话的成员预先 claim（首次向大群发送时不再在锁内逐个串行请求）
        let mut claims = match self.store.get_group(gid)? {
            Some(g) => {
                let shared = self.store.get_outbound(gid)?.map(|o| o.shared_with).unwrap_or_default();
                let cands: Vec<String> = g.member_ids().into_iter().filter(|u| *u != me && !shared.contains(u)).collect();
                self.prefetch_claims(cands.iter().map(|s| s.as_str())).await
            }
            None => Claims::new(),
        };

        // 1) 加锁：取/轮换出站会话，为未持有者准备 room_key，加密群消息
        let (session_id, body, key_msgs, room_key, mut failed) = {
            let _g = self.crypto_lock.lock().await;
            // 成员表在锁内读取，保证与轮换判断一致
            let group = self.store.get_group(gid)?.ok_or_else(|| HushError::not_found(format!("群 {gid}")))?;
            if !group.active || !group.is_member(&me) {
                return Err(HushError::invalid("你已不在该群"));
            }
            let recipients: HashSet<String> = group.member_ids().into_iter().filter(|u| *u != me).collect();
            let now = now_ms();
            let mut out = match self.store.get_outbound(gid)? {
                Some(o) if !outbound_needs_rotation(&o, &recipients, now) => o,
                other => {
                    if other.is_some() {
                        tracing::info!("群 {gid} 出站会话需要轮换");
                    }
                    OutboundRec { session: megolm::new_outbound(), created_at: now, msg_count: 0, shared_with: HashSet::new() }
                }
            };
            let session_id = out.session.session_id();
            let mut need: Vec<String> = recipients.iter().filter(|u| !out.shared_with.contains(*u)).cloned().collect();
            need.sort();
            // 在加密本条消息之前导出：接收方从这个位置开始能解密（重试时复用同一个 key）
            let key = megolm::export_key(&out.session);
            let (key_msgs, failed) = self.encrypt_room_keys_locked(gid, &session_id, &key, need, &mut claims).await?;
            let body = b64e(megolm::encrypt(&mut out.session, &content.to_bytes()));
            out.msg_count += 1;
            self.store.save_outbound(gid, &out)?;
            (session_id, body, key_msgs, key, failed)
        };

        // 2) 先发 room_key，成功者记为已分发；失败的成员（加密/claim 失败或发送失败）重试一次
        failed.extend(self.post_room_keys(&api, gid, &session_id, key_msgs).await?);
        if !failed.is_empty() {
            tracing::info!("群 {gid} 的 room_key 对 {} 人发送失败，重试一次", failed.len());
            let retry = std::mem::take(&mut failed);
            let mut claims = self.prefetch_claims(retry.iter().map(|s| s.as_str())).await;
            let (retry_msgs, mut still) = {
                let _g = self.crypto_lock.lock().await;
                self.encrypt_room_keys_locked(gid, &session_id, &room_key, retry, &mut claims).await?
            };
            still.extend(self.post_room_keys(&api, gid, &session_id, retry_msgs).await?);
            if !still.is_empty() {
                // 这些成员暂时解不开本条消息（会挂起）；他们仍不在已分发名单里，下次发送会再发 room_key
                tracing::warn!("群 {gid} 消息 {} 的 room_key 重试后仍未送达 {:?}，这些成员暂时无法解密该消息", content.id, still);
            }
        }

        // 3) 群消息
        let r: GroupSendResp = api
            .post(
                &format!("/v1/groups/{}/messages", api::enc(gid)),
                &json!({"session_id": session_id, "body": body, "client_id": content.id, "urgent": urgent}),
            )
            .await?;
        Ok(r.server_ts)
    }

    /// 为一组成员经 Olm 加密 room_key（调用方持有 crypto_lock）。返回 (待发送, 加密失败的成员)。
    async fn encrypt_room_keys_locked(
        &self,
        gid: &str,
        session_id: &str,
        key: &str,
        uids: Vec<String>,
        claims: &mut Claims,
    ) -> Result<(Vec<(String, String, u8, String)>, Vec<String>)> {
        let mut msgs = Vec::new();
        let mut failed = Vec::new();
        if uids.is_empty() {
            return Ok((msgs, failed));
        }
        let acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
        for uid in uids {
            let rk = Content::new(
                ContentBody::RoomKey { group_id: gid.to_string(), session_id: session_id.to_string(), session_key: key.to_string() },
                Some(gid.to_string()),
                0,
            );
            match self.olm_encrypt_locked(&acc, &uid, &rk.to_bytes(), claims).await {
                Ok((t, b)) => msgs.push((uid, rk.id.clone(), t, b)),
                Err(e) => {
                    tracing::warn!("无法向 {uid} 分发群密钥: {e}");
                    failed.push(uid);
                }
            }
        }
        Ok((msgs, failed))
    }

    /// 发送已加密的 room_key，成功者记入出站会话的已分发名单。返回发送失败的成员。
    async fn post_room_keys(&self, api: &api::Api, gid: &str, session_id: &str, msgs: Vec<(String, String, u8, String)>) -> Result<Vec<String>> {
        if msgs.is_empty() {
            return Ok(vec![]);
        }
        let all: Vec<String> = msgs.iter().map(|m| m.0.clone()).collect();
        if self.hook_fail_room_key_post() {
            tracing::warn!("room_key 发送失败（测试注入）");
            return Ok(all);
        }
        let wire: Vec<_> = msgs
            .iter()
            .map(|(uid, cid, t, b)| json!({"to": uid, "type": t, "body": b, "client_id": cid, "urgent": false}))
            .collect();
        let resp = match api.post::<OlmSendResp, _>("/v1/messages", &json!({ "messages": wire })).await {
            Ok(r) => r,
            Err(e) => {
                tracing::warn!("room_key 发送失败: {e}");
                return Ok(all);
            }
        };
        let ok: HashSet<String> = resp.results.iter().filter(|r| r.error_code().is_none()).map(|r| r.to.clone()).collect();
        {
            let _g = self.crypto_lock.lock().await;
            if let Some(mut o) = self.store.get_outbound(gid)? {
                if o.session.session_id() == session_id {
                    o.shared_with.extend(ok.iter().cloned());
                    self.store.save_outbound(gid, &o)?;
                }
            }
        }
        Ok(all.into_iter().filter(|u| !ok.contains(u)).collect())
    }

    // ================= 接收 =================

    /// 解密信封。解密成功时推进后的棘轮状态与明文在同一事务保存（明文写入该信封的收件箱行，见 `store::inbox`）。
    pub(crate) async fn decrypt_envelope(self: &Arc<Self>, env: &Envelope, backlog: bool) -> Result<EnvelopeOutcome> {
        if self.store.is_blocked(&env.from)? {
            return Ok(EnvelopeOutcome::Dropped);
        }
        match env.kind.as_str() {
            "olm" => self.decrypt_olm(env, backlog).await,
            "megolm" => self.decrypt_megolm(env, backlog).await,
            other => {
                tracing::warn!("未知信封类型 {other}");
                Ok(EnvelopeOutcome::Dropped)
            }
        }
    }

    /// 收件箱里已保存的明文（此前解密过、进程在落成消息前退出）：不再解密，直接还原为 `Incoming`。
    pub(crate) fn outcome_from_inbox(self: &Arc<Self>, env: &Envelope, p: InboxPlain, backlog: bool) -> Result<EnvelopeOutcome> {
        match b64d(&p.plaintext) {
            Ok(plain) => self.incoming_from_plain(env, &plain, p.olm_session_id, backlog),
            Err(e) => self.undecryptable(env, &format!("收件箱明文损坏: {e}"), backlog),
        }
    }

    /// 明文 → `Incoming`。经 Megolm 收到的要求 `Content.group_id` 与信封一致。
    fn incoming_from_plain(self: &Arc<Self>, env: &Envelope, plain: &[u8], olm_session_id: Option<String>, backlog: bool) -> Result<EnvelopeOutcome> {
        let content = match Content::from_bytes(plain) {
            Ok(c) => c,
            Err(e) => return self.undecryptable(env, &format!("明文格式错误: {e}"), backlog),
        };
        let via_group = if env.kind == "megolm" {
            let gid = env.group_id.clone().unwrap_or_default();
            if content.group_id.as_deref() != Some(gid.as_str()) {
                return self.undecryptable(env, "Content.group_id 与信封不符", backlog);
            }
            Some(gid)
        } else {
            None
        };
        Ok(EnvelopeOutcome::Decrypted(Box::new(Incoming {
            from: env.from.clone(),
            content,
            server_ts: env.server_ts,
            via_group,
            olm_session_id,
            backlog,
        })))
    }

    async fn decrypt_olm(self: &Arc<Self>, env: &Envelope, backlog: bool) -> Result<EnvelopeOutcome> {
        let bytes = match b64d(&env.body) {
            Ok(b) => b,
            Err(_) => return self.undecryptable(env, "body 非 base64", backlog),
        };
        let msg = match olm::parse_message(env.msg_type, &bytes) {
            Ok(m) => m,
            Err(e) => return self.undecryptable(env, &e.to_string(), backlog),
        };
        let is_prekey = env.msg_type == 0;
        let mut retried = false;
        loop {
            // PreKey 需要校验对端身份（网络请求在加锁前完成）
            let expected = if is_prekey {
                // 临时错误（网络、5xx、429）向上返回：信封不 ack，稍后重投；永久错误（如用户不存在）记为占位
                let rec = match self.known_identity(&env.from).await {
                    Ok(r) => r,
                    Err(e) if e.is_transient() => return Err(e),
                    Err(e) => return self.undecryptable(env, &format!("无法获取对方身份: {e}"), backlog),
                };
                Some(olm::parse_curve25519(&rec.identity_key)?)
            } else {
                None
            };
            let res = {
                let _g = self.crypto_lock.lock().await;
                let mut acc = self.store.load_account()?.ok_or(HushError::NotLoggedIn)?;
                let mut sessions = self.store.load_olm_sessions(&env.from)?;
                match olm::decrypt(&mut acc, &mut sessions, &msg, expected, now_ms()) {
                    Ok(d) => {
                        let entry = &sessions[d.index];
                        // 棘轮状态（会话 / 账户）与明文在同一事务落盘（C-H）
                        let saved = InboxPlain { plaintext: b64e(&d.plaintext), olm_session_id: Some(entry.session.session_id()) };
                        self.store.save_olm_decrypt(&env.from, entry, d.created.then_some(&acc), Some((&env.id, &saved)))?;
                        if d.created {
                            if let vodozemac::olm::OlmMessage::PreKey(pk) = &msg {
                                self.note_fallback_used(&pk.one_time_key())?;
                            }
                        }
                        Ok((d.plaintext, entry.session.session_id(), d.created))
                    }
                    Err(e) => Err(e),
                }
            };
            match res {
                Ok((plain, sid, created)) => {
                    if is_prekey {
                        Self::stat_inc(&self.stats.olm_prekey_rx);
                    } else {
                        Self::stat_inc(&self.stats.olm_normal_rx);
                    }
                    if created {
                        self.spawn_replenish();
                    }
                    return self.incoming_from_plain(env, &plain, Some(sid), backlog);
                }
                Err(olm::OlmDecryptError::IdentityMismatch { claimed }) if !retried => {
                    // 可能对方重新登录而我们漏收 key_changed：向服务端确认
                    retried = true;
                    let r = match self.fetch_identity(&env.from).await {
                        Ok(r) => r,
                        Err(e) if e.is_transient() => return Err(e),
                        Err(e) => return self.undecryptable(env, &format!("无法确认对方身份: {e}"), backlog),
                    };
                    if olm::parse_curve25519(&r.identity_key)? == claimed {
                        // unshare_user 会改写出站会话的 shared_with，与 send_group 一样须持 crypto_lock
                        let _g = self.crypto_lock.lock().await;
                        self.on_identity_changed(&env.from, Some(IdentityRec { user_id: env.from.clone(), ..IdentityRec::from(r) }))?;
                        continue;
                    }
                    return self.undecryptable(env, "PreKey 身份与服务端记录不符", backlog);
                }
                Err(olm::OlmDecryptError::Failed(why)) => return self.undecryptable(env, &why, backlog),
                Err(e) => return self.undecryptable(env, &format!("{e:?}"), backlog),
            }
        }
    }

    pub(crate) fn undecryptable(self: &Arc<Self>, env: &Envelope, why: &str, backlog: bool) -> Result<EnvelopeOutcome> {
        tracing::warn!("来自 {} 的消息无法解密: {why}", env.from);
        Self::stat_inc(&self.stats.undecryptable);
        let conv = match (&env.kind[..], &env.group_id) {
            ("megolm", Some(g)) => group_conv(g),
            _ => direct_conv(&env.from),
        };
        self.store_incoming_placeholder(&conv, env, backlog)?;
        if env.kind == "olm" {
            self.maybe_send_session_reset(&env.from);
        }
        Ok(EnvelopeOutcome::Undecryptable)
    }

    /// 解密失败后请求对方重建会话（同一用户 60 秒内最多一次）。
    fn maybe_send_session_reset(self: &Arc<Self>, uid: &str) {
        let now = now_ms();
        {
            let mut m = self.reset_sent.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(t) = m.get(uid) {
                if now - *t < 60_000 {
                    return;
                }
            }
            m.insert(uid.to_string(), now);
        }
        let me = self.clone();
        let uid = uid.to_string();
        let generation = self.session_gen();
        tokio::spawn(async move {
            // 期间登出或换了账号：不能以新账号的身份给旧账号的联系人发 reset
            if !me.same_session(generation) {
                return;
            }
            {
                let _g = me.crypto_lock.lock().await;
                let _ = me.store.delete_olm_sessions(&uid);
            }
            let c = Content::new(ContentBody::SessionReset {}, None, 0);
            if let Err(e) = me.send_olm(&uid, c, false).await {
                tracing::warn!("发送 session_reset 失败: {e}");
            }
        });
    }

    async fn decrypt_megolm(self: &Arc<Self>, env: &Envelope, backlog: bool) -> Result<EnvelopeOutcome> {
        let (Some(gid), Some(sid)) = (env.group_id.clone(), env.session_id.clone()) else {
            return Ok(EnvelopeOutcome::Dropped);
        };
        if self.store.get_group(&gid)?.is_none() {
            let _ = self.refresh_group(&gid).await;
        }
        let res = {
            let _g = self.crypto_lock.lock().await;
            match self.store.get_inbound(&env.from, &sid)? {
                None => None,
                Some((stored_gid, mut s)) => {
                    if stored_gid != gid {
                        Some(Err(HushError::crypto("会话所属群不符")))
                    } else {
                        let bytes = b64d(&env.body)?;
                        match megolm::decrypt_detailed(&mut s, &bytes) {
                            Ok(p) => {
                                let saved = InboxPlain { plaintext: b64e(&p), olm_session_id: None };
                                self.store.save_inbound_decrypt(&env.from, &gid, &s, &env.id, &saved)?;
                                Some(Ok(p))
                            }
                            // 消息早于本地密钥的起点：挂起，等待更早位置导出的 room_key
                            Err(megolm::MegolmDecryptError::UnknownIndex { first_known, index }) => {
                                tracing::info!("群消息序号 {index} 早于已知 {first_known}，挂起等待更早的 room_key");
                                None
                            }
                            Err(megolm::MegolmDecryptError::Other(m)) => Some(Err(HushError::crypto(m))),
                        }
                    }
                }
            }
        };
        match res {
            None => {
                self.store.add_pending(&PendingRec {
                    envelope_id: env.id.clone(),
                    group_id: gid,
                    sender: env.from.clone(),
                    session_id: sid,
                    body: env.body.clone(),
                    server_ts: env.server_ts,
                })?;
                Ok(EnvelopeOutcome::Pending)
            }
            Some(Ok(plain)) => self.incoming_from_plain(env, &plain, None, backlog),
            Some(Err(e)) => self.undecryptable(env, &e.to_string(), backlog),
        }
    }

    /// 收到 room_key：保存入站会话。返回因此可以处理的挂起信封。
    ///
    /// 发送者按本地群信息不是成员时（本地可能过时：room_key 先于 group_updated 到达，或积压与刷新并发），
    /// 先向服务端刷新一次再判断。刷新失败不向上返回错误：此时 Olm 棘轮已推进，信封若不 ack，重投时无法再解密。
    pub(crate) async fn accept_room_key(&self, from: &str, gid: &str, sid: &str, key: &str) -> Result<Vec<Envelope>> {
        let mut group = self.store.get_group(gid)?;
        if !group.as_ref().is_some_and(|g| g.is_member(from)) {
            match self.refresh_group(gid).await {
                Ok(_) => {}
                Err(e) if e.is_server_code("not_found") || e.is_server_code("forbidden") => {
                    tracing::warn!("忽略 {from} 的 room_key：我们不在群 {gid} 中");
                    return Ok(vec![]);
                }
                Err(e) => tracing::warn!("刷新群 {gid} 失败，按本地信息判断 room_key: {e}"),
            }
            group = self.store.get_group(gid)?;
        }
        match &group {
            Some(g) if !g.is_member(from) => {
                tracing::warn!("忽略非群成员 {from} 的 room_key");
                return Ok(vec![]);
            }
            Some(_) => {}
            // 本地没有该群且刷新遇到临时错误：先接受。服务端只转发群成员发出的群消息，
            // 非成员注入的入站会话不会有对应的群消息可解；丢弃则可能永久丢失真实成员的密钥
            None => tracing::info!("群 {gid} 信息暂不可得，先接受 {from} 的 room_key"),
        }
        {
            let _g = self.crypto_lock.lock().await;
            let mut s = megolm::inbound_from_key(key)?;
            if s.session_id() != sid {
                return Err(HushError::crypto("room_key 会话 id 不符"));
            }
            match self.store.get_inbound(from, sid)? {
                None => self.store.save_inbound(from, gid, &s)?,
                // 同一会话再次收到 room_key（重试 / 并发分发）：首个已知序号更小者胜出，
                // 否则先到的较晚位置密钥会让新成员永远解不开更早的消息
                Some((stored_gid, mut existing)) if stored_gid == gid => {
                    if let Some(better) = megolm::better_inbound(&mut existing, &mut s) {
                        tracing::info!("room_key 更早（{} < {}），替换入站会话", better.first_known_index(), existing.first_known_index());
                        self.store.save_inbound(from, gid, &better)?;
                    }
                }
                Some(_) => tracing::warn!("忽略 {from} 的 room_key：会话所属群不符"),
            }
        }
        let pending = self.store.pending_for(from, sid)?;
        Ok(pending
            .into_iter()
            .map(|p| Envelope {
                id: p.envelope_id,
                kind: "megolm".into(),
                from: p.sender,
                msg_type: 0,
                group_id: Some(p.group_id),
                session_id: Some(p.session_id),
                body: p.body,
                server_ts: p.server_ts,
            })
            .collect())
    }

    /// session_reset：保留承载该消息的会话，丢弃其余会话。
    pub(crate) async fn on_session_reset(&self, from: &str, keep: Option<&str>) -> Result<()> {
        let _g = self.crypto_lock.lock().await;
        match keep {
            Some(sid) => self.store.delete_olm_sessions_except(from, sid),
            None => self.store.delete_olm_sessions(from),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rec(shared: &[&str], count: u32, created_at: i64) -> OutboundRec {
        OutboundRec {
            session: megolm::new_outbound(),
            created_at,
            msg_count: count,
            shared_with: shared.iter().map(|s| s.to_string()).collect(),
        }
    }

    fn megolm_env(id: &str, from: &str, gid: &str, sid: &str, bytes: Vec<u8>) -> Envelope {
        Envelope {
            id: id.into(),
            kind: "megolm".into(),
            from: from.into(),
            msg_type: 0,
            group_id: Some(gid.into()),
            session_id: Some(sid.into()),
            body: b64e(bytes),
            server_ts: 1,
        }
    }

    /// 较晚位置的 room_key 先到：更早的消息挂起（而不是判为无法解密）；更早的 room_key 后到时替换并解开挂起消息。
    #[tokio::test]
    async fn later_room_key_first_then_earlier_one() {
        let (inner, _dir) = super::super::test_inner();
        inner.store.put_group(&super::super::test_group("g1", &["me", "alice"]), true).unwrap();
        let mut out = megolm::new_outbound();
        let sid = out.session_id();
        let k0 = megolm::export_key(&out);
        let text = |t: &str| Content::new(ContentBody::Text { text: t.into(), quote: None, mentions: vec![] }, Some("g1".into()), 0).to_bytes();
        let m0 = megolm::encrypt(&mut out, &text("m0"));
        let k1 = megolm::export_key(&out);
        let m1 = megolm::encrypt(&mut out, &text("m1"));

        assert!(inner.accept_room_key("alice", "g1", &sid, &k1).await.unwrap().is_empty());
        let e0 = megolm_env("e0", "alice", "g1", &sid, m0);
        assert!(matches!(inner.decrypt_envelope(&e0, false).await.unwrap(), EnvelopeOutcome::Pending), "早于密钥起点应挂起");
        assert!(matches!(inner.decrypt_envelope(&megolm_env("e1", "alice", "g1", &sid, m1), false).await.unwrap(), EnvelopeOutcome::Decrypted(_)));
        assert_eq!(inner.store.pending_count().unwrap(), 1);
        assert_eq!(inner.stats.undecryptable.load(std::sync::atomic::Ordering::Relaxed), 0);

        let pending = inner.accept_room_key("alice", "g1", &sid, &k0).await.unwrap();
        assert_eq!(pending.len(), 1);
        assert_eq!(inner.store.get_inbound("alice", &sid).unwrap().unwrap().1.first_known_index(), 0, "更早的密钥应替换");
        match inner.decrypt_envelope(&pending[0], false).await.unwrap() {
            EnvelopeOutcome::Decrypted(i) => assert!(matches!(i.content.body, ContentBody::Text { ref text, .. } if text == "m0")),
            _ => panic!("替换后应能解密挂起消息"),
        }
        // 再收到较晚的密钥不会回退
        inner.accept_room_key("alice", "g1", &sid, &k1).await.unwrap();
        assert_eq!(inner.store.get_inbound("alice", &sid).unwrap().unwrap().1.first_known_index(), 0);
    }

    /// room_key 到达后逐条处理挂起消息：排在前面的坏消息不影响后面的，坏消息记为占位并清掉挂起记录。
    #[tokio::test]
    async fn pending_messages_processed_individually() {
        let (inner, _dir) = super::super::test_inner();
        inner.store.put_group(&super::super::test_group("g1", &["me", "alice"]), true).unwrap();
        let mut out = megolm::new_outbound();
        let sid = out.session_id();
        let key = megolm::export_key(&out);
        let good = Content::new(ContentBody::Text { text: "挂起后解开".into(), quote: None, mentions: vec![] }, Some("g1".into()), 0);
        let body = b64e(megolm::encrypt(&mut out, &good.to_bytes()));
        let pend = |id: &str, body: String, ts: i64| PendingRec {
            envelope_id: id.into(),
            group_id: "g1".into(),
            sender: "alice".into(),
            session_id: sid.clone(),
            body,
            server_ts: ts,
        };
        inner.store.add_pending(&pend("bad", "!!!不是 base64!!!".into(), 1)).unwrap();
        inner.store.add_pending(&pend("good", body, 2)).unwrap();

        let rk = Content::new(
            ContentBody::RoomKey { group_id: "g1".into(), session_id: sid.clone(), session_key: key },
            Some("g1".into()),
            0,
        );
        inner
            .handle_incoming(Incoming { from: "alice".into(), content: rk, server_ts: 3, via_group: None, olm_session_id: Some("s".into()), backlog: false })
            .await
            .unwrap();
        let m = inner.store.get_message(&good.id).unwrap().expect("好的挂起消息应被处理");
        assert!(matches!(m.msg.body, crate::store::StoredBody::Text { ref text } if text == "挂起后解开"));
        assert_eq!(inner.store.pending_count().unwrap(), 0, "两条挂起记录都应清掉");
        let bad = inner.store.get_message("bad").unwrap().expect("坏消息应记为占位");
        assert!(matches!(bad.msg.body, crate::store::StoredBody::Undecryptable));
    }

    /// 密钥上传与 claim 这类 HTTP 请求进行期间，crypto_lock 必须是空闲的（收发消息的加解密不被阻塞）。
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn crypto_lock_not_held_during_http() {
        use std::time::Duration;
        let srv = crate::client::stub::start(Arc::new(|m, path, _b| {
            std::thread::sleep(Duration::from_millis(600));
            match (m, path) {
                ("GET", "/v1/keys/count") => (200, r#"{"one_time_key_count":0}"#.into()),
                ("PUT", "/v1/keys") => (200, r#"{"one_time_key_count":100}"#.into()),
                _ => (503, "busy".into()),
            }
        }))
        .await;
        let (inner, _dir) = crate::client::test_inner_at(&srv.url);
        crate::client::published_account(&inner);

        let i2 = inner.clone();
        let t = tokio::spawn(async move { i2.replenish_keys().await });
        for _ in 0..8 {
            tokio::time::sleep(Duration::from_millis(100)).await;
            let g = tokio::time::timeout(Duration::from_millis(50), inner.crypto_lock.lock()).await;
            assert!(g.is_ok(), "补充密钥的 HTTP 请求期间不应持有 crypto_lock");
        }
        t.await.unwrap().unwrap();

        let i2 = inner.clone();
        let t = tokio::spawn(async move {
            let c = Content::new(ContentBody::Text { text: "x".into(), quote: None, mentions: vec![] }, None, 0);
            i2.send_olm_batch(vec![("bob".into(), c.clone(), false), ("carol".into(), c, false)]).await
        });
        tokio::time::sleep(Duration::from_millis(200)).await;
        let g = tokio::time::timeout(Duration::from_millis(50), inner.crypto_lock.lock()).await;
        assert!(g.is_ok(), "claim 请求期间不应持有 crypto_lock");
        drop(g);
        let started = std::time::Instant::now();
        let r = t.await.unwrap().unwrap();
        assert!(r.iter().all(|x| matches!(x, Err(e) if e.is_server_code("http_503"))), "claim 失败应逐条返回");
        assert!(started.elapsed() < Duration::from_millis(1000), "两个对端的 claim 应并发进行");
    }

    /// 解密失败后安排的 session_reset 还没执行就换了账号：不能再以新账号的身份去 claim / 发送。
    #[tokio::test]
    async fn background_reset_dropped_after_account_switch() {
        let srv = crate::client::stub::start(Arc::new(|_m, _p, _b| (503, "x".into()))).await;
        let (inner, _dir) = crate::client::test_inner_at(&srv.url);
        let generation = inner.session_gen();
        inner.maybe_send_session_reset("alice");
        // 当前线程运行时：任务要等本测试让出后才会执行
        let mut other = inner.auth().unwrap();
        other.user_id = "someone-else".into();
        other.token = "t2".into();
        inner.set_auth(Some(other)).unwrap();
        assert!(!inner.same_session(generation));
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        assert_eq!(srv.count("POST", "/v1/keys/claim/"), 0, "旧账号的后台任务不应再发请求");

        // 同一登录态下照常执行
        inner.reset_sent.lock().unwrap().clear();
        inner.maybe_send_session_reset("bob");
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        assert_eq!(srv.count("POST", "/v1/keys/claim/bob"), 1);
    }

    #[test]
    fn rotation_when_shared_with_contains_non_member() {
        let members: HashSet<String> = ["bob", "carol"].iter().map(|s| s.to_string()).collect();
        let now = 1_000_000;
        assert!(!outbound_needs_rotation(&rec(&["bob"], 3, now), &members, now), "子集：不需要轮换");
        assert!(!outbound_needs_rotation(&rec(&["bob", "carol"], 3, now), &members, now));
        assert!(outbound_needs_rotation(&rec(&["bob", "mallory"], 3, now), &members, now), "含已移出成员必须轮换");
        assert!(outbound_needs_rotation(&rec(&[], MEGOLM_MAX_MESSAGES, now), &members, now), "发满 100 条");
        assert!(outbound_needs_rotation(&rec(&[], 0, now - MEGOLM_MAX_AGE_MS), &members, now), "超过 7 天");
    }
}
