//! 资料、好友、拉黑、群组与会话列表。

use super::*;

impl Store {
    // ---------- profiles ----------
    /// 缓存**非权威来源**的资料（K2）：邮箱搜索、群成员、朋友圈、好友申请、`profile_updated` 事件、修改资料的返回。
    /// 这些来源按契约不带邮箱（或不可信），所以**邮箱一律沿用缓存**——既不清掉，也不改写；
    /// 只有 `put_profile_authoritative`（`/v1/users/{id}`、`/v1/me`）、`set_friends`（`/v1/friends`）与删好友能改邮箱。
    /// 返回实际缓存的资料（含沿用的邮箱）。
    pub fn put_profile(&self, p: &Profile) -> Result<Profile> {
        let mut p = p.clone();
        if p.id.is_empty() {
            return Ok(p);
        }
        p.email = self.get_profile(&p.id)?.and_then(|old| old.email);
        self.write_profile(&p)?;
        Ok(p)
    }
    /// 缓存资料，邮箱以本次为准（`/v1/users/{id}`、`/v1/me`、好友列表：服务端按可见性决定是否返回邮箱）。
    pub fn put_profile_authoritative(&self, p: &Profile) -> Result<()> {
        if p.id.is_empty() {
            return Ok(());
        }
        self.write_profile(p)
    }
    fn write_profile(&self, p: &Profile) -> Result<()> {
        self.conn()
            .execute("INSERT OR REPLACE INTO profiles(user_id,json) VALUES(?1,?2)", params![p.id, serde_json::to_string(p)?])?;
        Ok(())
    }
    /// 不再是好友：清掉缓存的邮箱（邮箱只对好友可见）。
    fn forget_email(&self, uid: &str) -> Result<()> {
        if let Some(mut p) = self.get_profile(uid)? {
            if p.email.take().is_some() {
                self.write_profile(&p)?;
            }
        }
        Ok(())
    }
    pub fn get_profile(&self, uid: &str) -> Result<Option<Profile>> {
        let s: Option<String> =
            self.conn().query_row("SELECT json FROM profiles WHERE user_id=?1", [uid], |r| r.get(0)).optional()?;
        Ok(match s {
            Some(s) => Some(serde_json::from_str(&s)?),
            None => None,
        })
    }

    // ---------- friends / blocks ----------
    /// 以服务端好友列表为准（资料连同邮箱整条覆盖）；不再是好友的用户清掉缓存的邮箱。
    pub fn set_friends(&self, list: &[Profile]) -> Result<()> {
        let former: Vec<String> = self.friend_ids()?.into_iter().filter(|id| !list.iter().any(|p| &p.id == id)).collect();
        self.write_friends(list)?;
        for id in former {
            self.forget_email(&id)?;
        }
        Ok(())
    }
    fn write_friends(&self, list: &[Profile]) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        tx.execute("DELETE FROM friends", [])?;
        for p in list {
            tx.execute("INSERT OR IGNORE INTO friends(user_id) VALUES(?1)", [&p.id])?;
            tx.execute("INSERT OR REPLACE INTO profiles(user_id,json) VALUES(?1,?2)", params![p.id, serde_json::to_string(p)?])?;
        }
        tx.commit()?;
        Ok(())
    }
    pub fn add_friend(&self, uid: &str) -> Result<()> {
        self.conn().execute("INSERT OR IGNORE INTO friends(user_id) VALUES(?1)", [uid])?;
        Ok(())
    }
    pub fn remove_friend(&self, uid: &str) -> Result<()> {
        self.conn().execute("DELETE FROM friends WHERE user_id=?1", [uid])?;
        self.forget_email(uid)
    }
    pub fn friend_ids(&self) -> Result<Vec<String>> {
        let c = self.conn();
        let mut st = c.prepare("SELECT user_id FROM friends ORDER BY user_id")?;
        let v = st.query_map([], |r| r.get(0))?.collect::<std::result::Result<Vec<String>, _>>()?;
        Ok(v)
    }
    pub fn set_blocks(&self, ids: &[String]) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        tx.execute("DELETE FROM blocks", [])?;
        for id in ids {
            tx.execute("INSERT OR IGNORE INTO blocks(user_id) VALUES(?1)", [id])?;
        }
        tx.commit()?;
        Ok(())
    }
    pub fn add_block(&self, uid: &str) -> Result<()> {
        self.conn().execute("INSERT OR IGNORE INTO blocks(user_id) VALUES(?1)", [uid])?;
        Ok(())
    }
    pub fn remove_block(&self, uid: &str) -> Result<()> {
        self.conn().execute("DELETE FROM blocks WHERE user_id=?1", [uid])?;
        Ok(())
    }
    pub fn blocked(&self) -> Result<Vec<String>> {
        let c = self.conn();
        let mut st = c.prepare("SELECT user_id FROM blocks ORDER BY user_id")?;
        let v = st.query_map([], |r| r.get(0))?.collect::<std::result::Result<Vec<String>, _>>()?;
        Ok(v)
    }
    pub fn is_blocked(&self, uid: &str) -> Result<bool> {
        Ok(self.conn().query_row("SELECT 1 FROM blocks WHERE user_id=?1", [uid], |_| Ok(())).optional()?.is_some())
    }

    // ---------- groups ----------
    pub fn put_group(&self, g: &GroupInfo, active: bool) -> Result<()> {
        self.conn().execute(
            "INSERT OR REPLACE INTO groups(group_id,json,active) VALUES(?1,?2,?3)",
            params![g.id, serde_json::to_string(g)?, active as i64],
        )?;
        Ok(())
    }
    pub fn set_group_active(&self, gid: &str, active: bool) -> Result<()> {
        self.conn().execute("UPDATE groups SET active=?2 WHERE group_id=?1", params![gid, active as i64])?;
        Ok(())
    }
    pub fn get_group(&self, gid: &str) -> Result<Option<GroupInfo>> {
        let row: Option<(String, i64)> = self
            .conn()
            .query_row("SELECT json,active FROM groups WHERE group_id=?1", [gid], |r| Ok((r.get(0)?, r.get(1)?)))
            .optional()?;
        Ok(match row {
            Some((j, a)) => {
                let mut g: GroupInfo = serde_json::from_str(&j)?;
                g.active = b(a);
                Some(g)
            }
            None => None,
        })
    }
    pub fn list_groups(&self) -> Result<Vec<GroupInfo>> {
        let rows: Vec<(String, i64)> = {
            let c = self.conn();
            let mut st = c.prepare("SELECT json,active FROM groups")?;
            let v = st.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        let mut out = Vec::new();
        for (j, a) in rows {
            let mut g: GroupInfo = serde_json::from_str(&j)?;
            g.active = b(a);
            out.push(g);
        }
        out.sort_by(|a, b| a.name.cmp(&b.name));
        Ok(out)
    }

    // ---------- conversations ----------
    /// 不存在则创建，返回是否新建。
    /// 被删除过的会话（K1）重新出现时沿用删除前的免打扰设置。
    pub fn ensure_conv(&self, conv_id: &str, expire_secs: u32) -> Result<bool> {
        let c = self.conn();
        let hint = super::muted_hint_key(conv_id);
        let n = c.execute(
            "INSERT OR IGNORE INTO conversations(conv_id,last_ts,expire_secs,muted)
             VALUES(?1,?2,?3,EXISTS(SELECT 1 FROM kv WHERE k=?4))",
            params![conv_id, crate::util::now_ms(), expire_secs as i64, hint],
        )?;
        if n > 0 {
            c.execute("DELETE FROM kv WHERE k=?1", [hint])?;
        }
        Ok(n > 0)
    }
    fn conv_from_row(r: &rusqlite::Row) -> rusqlite::Result<ConvRow> {
        let draft_blob: Option<Vec<u8>> = r.get(8)?;
        let draft_ts: Option<i64> = r.get(9)?;
        Ok(ConvRow {
            conv_id: r.get(0)?,
            last_ts: r.get(1)?,
            unread: r.get::<_, i64>(2)? as u32,
            pinned: b(r.get(3)?),
            muted: b(r.get(4)?),
            archived: b(r.get(5)?),
            expire_secs: r.get::<_, i64>(6)? as u32,
            unread_mentions: r.get::<_, i64>(7)? as u32,
            draft: draft_blob.zip(draft_ts),
        })
    }
    /// 会话行 + 未读提及数（K6，部分索引 messages_mentions）+ 草稿（K5）。
    const CONV_SELECT: &'static str = "SELECT c.conv_id,c.last_ts,c.unread,c.pinned,c.muted,c.archived,c.expire_secs,
          (SELECT COUNT(*) FROM messages m WHERE m.conv_id=c.conv_id AND m.mentions_me=1 AND m.read=0 AND m.deleted=0),
          d.text, d.updated_at
        FROM conversations c LEFT JOIN drafts d ON d.conv_id=c.conv_id";
    pub fn get_conv(&self, conv_id: &str) -> Result<Option<ConvRow>> {
        Ok(self
            .conn()
            .query_row(&format!("{} WHERE c.conv_id=?1", Self::CONV_SELECT), [conv_id], Self::conv_from_row)
            .optional()?)
    }
    /// 置顶优先；其余按最近活动时间，有草稿的会话取「最后消息时间」与「草稿更新时间」中较晚者（K5）。
    pub fn list_convs(&self) -> Result<Vec<ConvRow>> {
        let c = self.conn();
        let mut st = c.prepare(&format!(
            "{} ORDER BY c.pinned DESC, MAX(c.last_ts, COALESCE(d.updated_at, 0)) DESC",
            Self::CONV_SELECT
        ))?;
        let v = st.query_map([], Self::conv_from_row)?.collect::<std::result::Result<Vec<_>, _>>()?;
        Ok(v)
    }
    pub fn set_conv_flag(&self, conv_id: &str, flag: &str, v: bool) -> Result<()> {
        let col = match flag {
            "pinned" | "muted" | "archived" => flag,
            _ => return Err(HushError::invalid("未知会话标志")),
        };
        let n = self
            .conn()
            .execute(&format!("UPDATE conversations SET {col}=?2 WHERE conv_id=?1"), params![conv_id, v as i64])?;
        if n == 0 {
            return Err(HushError::not_found(format!("会话 {conv_id}")));
        }
        Ok(())
    }
    pub fn set_conv_expire(&self, conv_id: &str, secs: u32) -> Result<()> {
        self.conn()
            .execute("UPDATE conversations SET expire_secs=?2 WHERE conv_id=?1", params![conv_id, secs as i64])?;
        Ok(())
    }
    /// 有新消息：更新时间，增加未读，取消归档（未免打扰时）。
    pub fn bump_conv(&self, conv_id: &str, ts: i64, unread_inc: u32) -> Result<()> {
        self.conn().execute(
            "UPDATE conversations SET last_ts=MAX(last_ts,?2), unread=unread+?3,
               archived=CASE WHEN ?3>0 AND muted=0 THEN 0 ELSE archived END WHERE conv_id=?1",
            params![conv_id, ts, unread_inc as i64],
        )?;
        Ok(())
    }
    pub fn clear_unread(&self, conv_id: &str) -> Result<()> {
        self.conn().execute("UPDATE conversations SET unread=0 WHERE conv_id=?1", [conv_id])?;
        Ok(())
    }
}
