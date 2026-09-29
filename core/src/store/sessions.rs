//! 加密状态：账户 pickle、对端身份（TOFU）、Olm 会话、Megolm 出站 / 入站会话与挂起的群消息。

use super::*;

impl Store {
    // ---------- account pickle ----------
    pub fn load_account(&self) -> Result<Option<vodozemac::olm::Account>> {
        match self.kv_get("account")? {
            Some(v) => {
                let s = String::from_utf8(v).map_err(HushError::storage)?;
                Ok(Some(olm::unpickle_account(&s, self.cipher.pickle_key())?))
            }
            None => Ok(None),
        }
    }
    pub fn save_account(&self, acc: &vodozemac::olm::Account) -> Result<()> {
        let p = olm::pickle_account(acc, self.cipher.pickle_key());
        self.kv_set("account", p.as_bytes())
    }

    // ---------- identities ----------
    pub fn get_identity(&self, uid: &str) -> Result<Option<IdentityRec>> {
        Ok(self
            .conn()
            .query_row(
                "SELECT user_id,identity_key,signing_key,device_generation,verified FROM identities WHERE user_id=?1",
                [uid],
                |r| {
                    Ok(IdentityRec {
                        user_id: r.get(0)?,
                        identity_key: r.get(1)?,
                        signing_key: r.get(2)?,
                        device_generation: r.get(3)?,
                        verified: b(r.get(4)?),
                    })
                },
            )
            .optional()?)
    }
    pub fn put_identity(&self, rec: &IdentityRec) -> Result<()> {
        self.conn().execute(
            "INSERT OR REPLACE INTO identities(user_id,identity_key,signing_key,device_generation,verified) VALUES(?1,?2,?3,?4,?5)",
            params![rec.user_id, rec.identity_key, rec.signing_key, rec.device_generation, rec.verified as i64],
        )?;
        Ok(())
    }
    pub fn delete_identity(&self, uid: &str) -> Result<()> {
        self.conn().execute("DELETE FROM identities WHERE user_id=?1", [uid])?;
        Ok(())
    }
    pub fn set_verified(&self, uid: &str, v: bool) -> Result<bool> {
        let n = self.conn().execute("UPDATE identities SET verified=?2 WHERE user_id=?1", params![uid, v as i64])?;
        Ok(n > 0)
    }

    // ---------- olm sessions ----------
    pub fn load_olm_sessions(&self, uid: &str) -> Result<Vec<olm::SessEntry>> {
        let rows: Vec<(String, i64)> = {
            let c = self.conn();
            let mut st = c.prepare("SELECT pickle,last_used FROM olm_sessions WHERE user_id=?1")?;
            let v = st.query_map([uid], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        let mut out = Vec::new();
        for (p, lu) in rows {
            match olm::unpickle_session(&p, self.cipher.pickle_key()) {
                Ok(s) => out.push(olm::SessEntry { session: s, last_used: lu }),
                Err(e) => tracing::warn!("丢弃无法解析的 Olm 会话: {e}"),
            }
        }
        Ok(out)
    }
    fn write_olm_session(&self, c: &Connection, uid: &str, e: &olm::SessEntry) -> Result<()> {
        let p = olm::pickle_session(&e.session, self.cipher.pickle_key());
        c.execute(
            "INSERT OR REPLACE INTO olm_sessions(user_id,session_id,pickle,last_used) VALUES(?1,?2,?3,?4)",
            params![uid, e.session.session_id(), p, e.last_used],
        )?;
        // 每个对端最多保留 MAX_OLM_SESSIONS 个会话（按最近使用）
        c.execute(
            "DELETE FROM olm_sessions WHERE user_id=?1 AND session_id NOT IN
               (SELECT session_id FROM olm_sessions WHERE user_id=?1 ORDER BY last_used DESC LIMIT ?2)",
            params![uid, MAX_OLM_SESSIONS],
        )?;
        Ok(())
    }
    pub fn save_olm_session(&self, uid: &str, e: &olm::SessEntry) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        self.write_olm_session(&tx, uid, e)?;
        tx.commit()?;
        Ok(())
    }
    /// 解密入站 Olm 消息后保存会话；新建了入站会话（消耗了 OTK）时连同账户在同一事务内写入，
    /// 避免崩溃时只落盘其一（会话已推进而账户仍含已用 OTK，或反之）。
    /// `inbox` 为 (信封 id, 明文)：明文与推进后的棘轮状态在同一事务写入收件箱（C-H）。
    pub fn save_olm_decrypt(
        &self,
        uid: &str,
        e: &olm::SessEntry,
        acc: Option<&vodozemac::olm::Account>,
        inbox: Option<(&str, &InboxPlain)>,
    ) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        self.write_olm_session(&tx, uid, e)?;
        if let Some(acc) = acc {
            let p = olm::pickle_account(acc, self.cipher.pickle_key());
            tx.execute("INSERT OR REPLACE INTO kv(k,v) VALUES('account',?1)", params![p.as_bytes()])?;
        }
        if let Some((env_id, plain)) = inbox {
            self.write_inbox_plain(&tx, env_id, plain)?;
        }
        tx.commit()?;
        Ok(())
    }
    pub fn delete_olm_sessions(&self, uid: &str) -> Result<()> {
        self.conn().execute("DELETE FROM olm_sessions WHERE user_id=?1", [uid])?;
        Ok(())
    }
    pub fn delete_olm_sessions_except(&self, uid: &str, keep_session_id: &str) -> Result<()> {
        self.conn()
            .execute("DELETE FROM olm_sessions WHERE user_id=?1 AND session_id<>?2", params![uid, keep_session_id])?;
        Ok(())
    }
    pub fn has_olm_session(&self, uid: &str) -> Result<bool> {
        Ok(self.conn().query_row("SELECT 1 FROM olm_sessions WHERE user_id=?1 LIMIT 1", [uid], |_| Ok(())).optional()?.is_some())
    }

    // ---------- megolm ----------
    pub fn get_outbound(&self, gid: &str) -> Result<Option<OutboundRec>> {
        let row: Option<(String, i64, i64, String)> = self
            .conn()
            .query_row(
                "SELECT pickle,created_at,msg_count,shared_with FROM megolm_out WHERE group_id=?1",
                [gid],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
            )
            .optional()?;
        match row {
            Some((p, created_at, count, shared)) => Ok(Some(OutboundRec {
                session: megolm::unpickle_outbound(&p, self.cipher.pickle_key())?,
                created_at,
                msg_count: count as u32,
                shared_with: serde_json::from_str(&shared)?,
            })),
            None => Ok(None),
        }
    }
    pub fn save_outbound(&self, gid: &str, rec: &OutboundRec) -> Result<()> {
        let p = megolm::pickle_outbound(&rec.session, self.cipher.pickle_key());
        self.conn().execute(
            "INSERT OR REPLACE INTO megolm_out(group_id,session_id,pickle,created_at,msg_count,shared_with) VALUES(?1,?2,?3,?4,?5,?6)",
            params![gid, rec.session.session_id(), p, rec.created_at, rec.msg_count as i64, serde_json::to_string(&rec.shared_with)?],
        )?;
        Ok(())
    }
    pub fn delete_outbound(&self, gid: &str) -> Result<()> {
        self.conn().execute("DELETE FROM megolm_out WHERE group_id=?1", [gid])?;
        Ok(())
    }
    #[cfg(any(test, feature = "test-hooks"))]
    pub fn outbound_session_id(&self, gid: &str) -> Result<Option<String>> {
        Ok(self.conn().query_row("SELECT session_id FROM megolm_out WHERE group_id=?1", [gid], |r| r.get(0)).optional()?)
    }
    /// 某用户身份变更后：从所有出站会话的已分发名单中移除，下次发送时重新分发 room_key。
    pub fn unshare_user(&self, uid: &str) -> Result<()> {
        let rows: Vec<(String, String)> = {
            let c = self.conn();
            let mut st = c.prepare("SELECT group_id,shared_with FROM megolm_out")?;
            let v = st.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        for (gid, shared) in rows {
            let mut set: HashSet<String> = serde_json::from_str(&shared)?;
            if set.remove(uid) {
                self.conn().execute(
                    "UPDATE megolm_out SET shared_with=?2 WHERE group_id=?1",
                    params![gid, serde_json::to_string(&set)?],
                )?;
            }
        }
        Ok(())
    }
    pub fn get_inbound(&self, sender: &str, sid: &str) -> Result<Option<(String, InboundGroupSession)>> {
        let row: Option<(String, String)> = self
            .conn()
            .query_row(
                "SELECT group_id,pickle FROM megolm_in WHERE sender=?1 AND session_id=?2",
                params![sender, sid],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .optional()?;
        match row {
            Some((gid, p)) => Ok(Some((gid, megolm::unpickle_inbound(&p, self.cipher.pickle_key())?))),
            None => Ok(None),
        }
    }
    pub fn save_inbound(&self, sender: &str, gid: &str, s: &InboundGroupSession) -> Result<()> {
        let c = self.conn();
        Self::write_inbound(&c, &self.cipher, sender, gid, s)
    }
    fn write_inbound(c: &Connection, cipher: &DbCipher, sender: &str, gid: &str, s: &InboundGroupSession) -> Result<()> {
        let p = megolm::pickle_inbound(s, cipher.pickle_key());
        c.execute(
            "INSERT OR REPLACE INTO megolm_in(sender,session_id,group_id,pickle) VALUES(?1,?2,?3,?4)",
            params![sender, s.session_id(), gid, p],
        )?;
        Ok(())
    }
    /// 解密群消息后保存入站会话，明文在同一事务写入收件箱（C-H）。
    pub fn save_inbound_decrypt(&self, sender: &str, gid: &str, s: &InboundGroupSession, env_id: &str, plain: &InboxPlain) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        Self::write_inbound(&tx, &self.cipher, sender, gid, s)?;
        self.write_inbox_plain(&tx, env_id, plain)?;
        tx.commit()?;
        Ok(())
    }
    #[cfg(any(test, feature = "test-hooks"))]
    pub fn has_inbound(&self, sender: &str, sid: &str) -> Result<bool> {
        Ok(self
            .conn()
            .query_row("SELECT 1 FROM megolm_in WHERE sender=?1 AND session_id=?2", params![sender, sid], |_| Ok(()))
            .optional()?
            .is_some())
    }
    pub fn add_pending(&self, p: &PendingRec) -> Result<()> {
        self.conn().execute(
            "INSERT OR IGNORE INTO megolm_pending(envelope_id,group_id,sender,session_id,body,server_ts,received_at) VALUES(?1,?2,?3,?4,?5,?6,?7)",
            params![p.envelope_id, p.group_id, p.sender, p.session_id, p.body, p.server_ts, crate::util::now_ms()],
        )?;
        Ok(())
    }
    pub fn pending_for(&self, sender: &str, sid: &str) -> Result<Vec<PendingRec>> {
        let c = self.conn();
        let mut st = c.prepare(
            "SELECT envelope_id,group_id,sender,session_id,body,server_ts FROM megolm_pending WHERE sender=?1 AND session_id=?2 ORDER BY server_ts",
        )?;
        let v = st
            .query_map(params![sender, sid], |r| {
                Ok(PendingRec {
                    envelope_id: r.get(0)?,
                    group_id: r.get(1)?,
                    sender: r.get(2)?,
                    session_id: r.get(3)?,
                    body: r.get(4)?,
                    server_ts: r.get(5)?,
                })
            })?
            .collect::<std::result::Result<Vec<_>, _>>()?;
        Ok(v)
    }
    pub fn delete_pending(&self, envelope_id: &str) -> Result<()> {
        self.conn().execute("DELETE FROM megolm_pending WHERE envelope_id=?1", [envelope_id])?;
        Ok(())
    }
    #[cfg(any(test, feature = "test-hooks"))]
    pub fn pending_count(&self) -> Result<i64> {
        Ok(self.conn().query_row("SELECT COUNT(*) FROM megolm_pending", [], |r| r.get(0))?)
    }
}
