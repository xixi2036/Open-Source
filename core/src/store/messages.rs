//! 消息、回应与去重记录。

use super::*;

impl Store {
    // ---------- messages ----------
    fn msg_from_row(&self, r: &rusqlite::Row) -> rusqlite::Result<(MsgRow, Vec<u8>)> {
        let body: Vec<u8> = r.get(8)?;
        Ok((
            MsgRow {
                id: r.get(0)?,
                conv_id: r.get(1)?,
                sender_id: r.get(2)?,
                outgoing: b(r.get(3)?),
                sent_at: r.get(4)?,
                server_ts: r.get(5)?,
                sort_ts: r.get(6)?,
                status: MessageStatus::parse(&r.get::<_, String>(7)?),
                msg: StoredMessage::plain(StoredBody::Undecryptable, None),
                expire_secs: r.get::<_, i64>(9)? as u32,
                expires_at: r.get(10)?,
                deleted: b(r.get(11)?),
                read: b(r.get(12)?),
                mentions_me: b(r.get(13)?),
            },
            body,
        ))
    }
    /// 解密一行的正文，返回是否成功。失败时保留 `msg_from_row` 预置的 Undecryptable。
    /// 日志只记错误类别：serde 的错误文本可能带出正文片段。
    fn decode_row(&self, m: &mut MsgRow, body: &[u8]) -> bool {
        match self.cipher.open(LABEL_BODY, body) {
            Ok(plain) => match serde_json::from_slice::<StoredMessage>(&plain) {
                Ok(msg) => {
                    m.msg = msg;
                    true
                }
                Err(e) => {
                    tracing::warn!("消息 {} 正文格式无法解析（{:?}），显示为占位", m.id, e.classify());
                    false
                }
            },
            Err(e) => {
                tracing::warn!("消息 {} 正文无法解密，显示为占位: {e}", m.id);
                false
            }
        }
    }
    /// 解密消息正文。单行损坏（坏扇区、写到一半）只把该行显示为「无法解密」，不让整页 / 会话列表 / 搜索失败。
    fn decode_rows(&self, rows: Vec<(MsgRow, Vec<u8>)>) -> Result<Vec<MsgRow>> {
        let mut out = Vec::with_capacity(rows.len());
        for (mut m, body) in rows {
            self.decode_row(&mut m, &body);
            out.push(m);
        }
        Ok(out)
    }
    const MSG_COLS: &'static str =
        "id,conv_id,sender_id,outgoing,sent_at,server_ts,sort_ts,status,body,expire_secs,expires_at,deleted,read,mentions_me";

    fn query_msgs(&self, sql_tail: &str, p: &[&dyn rusqlite::ToSql]) -> Result<Vec<MsgRow>> {
        let rows = {
            let c = self.conn();
            let mut st = c.prepare_cached(&format!("SELECT {} FROM messages {}", Self::MSG_COLS, sql_tail))?;
            let v = st.query_map(p, |r| self.msg_from_row(r))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        self.decode_rows(rows)
    }

    /// 同一会话内从 `ts` 起没有被其他消息占用的排序时间。`messages(before_ts)` 以 sort_ts 为游标，
    /// 同一会话内 sort_ts 唯一才能保证翻页不漏不重（同一毫秒的消息依次错开 1ms）。
    ///
    /// C1：最多向后探测 `SORT_PROBE_LIMIT` 个位置；还被占用（同一毫秒连续几十条，只有积压洪泛或恶意服务端才会出现）
    /// 就直接排到会话当前最后一条之后。每次插入的代价有上限，同一时间戳连续插入 N 条是线性的，
    /// 不再是逐个位置探测的 O(N²)（期间一直持有库锁）。
    fn free_sort_ts(c: &Connection, conv_id: &str, ts: i64, self_id: &str) -> Result<i64> {
        const SORT_PROBE_LIMIT: usize = 32;
        let mut st = c.prepare_cached("SELECT 1 FROM messages WHERE conv_id=?1 AND sort_ts=?2 AND id<>?3 LIMIT 1")?;
        let mut t = ts;
        for _ in 0..SORT_PROBE_LIMIT {
            if !st.exists(params![conv_id, t, self_id])? {
                return Ok(t);
            }
            // A6：不会溢出（入口已钳到当前时间附近），这里仍用饱和加法兜底
            t = t.saturating_add(1);
        }
        let last: Option<i64> = c
            .prepare_cached("SELECT sort_ts FROM messages WHERE conv_id=?1 ORDER BY sort_ts DESC LIMIT 1")?
            .query_row([conv_id], |r| r.get(0))
            .optional()?;
        Ok(last.map_or(t, |l| l.saturating_add(1).max(t)))
    }

    /// 插入消息（id 已存在时忽略，返回 false）。`m.sort_ts` 会被调整为会话内唯一的值（见 `free_sort_ts`），
    /// 并钳到当前时间附近（A6，见 `clamp_ts`）。
    pub fn insert_message(&self, m: &mut MsgRow) -> Result<bool> {
        let body = self.cipher.seal(LABEL_BODY, &serde_json::to_vec(&m.msg)?);
        let c = self.conn();
        m.sort_ts = Self::free_sort_ts(&c, &m.conv_id, clamp_ts(m.sort_ts), &m.id)?;
        let n = c
            .prepare_cached(&format!(
                "INSERT OR IGNORE INTO messages({},has_files) VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14,?15)",
                Self::MSG_COLS
            ))?
            .execute(params![
                m.id,
                m.conv_id,
                m.sender_id,
                m.outgoing as i64,
                m.sent_at,
                m.server_ts,
                m.sort_ts,
                m.status.as_str(),
                body,
                m.expire_secs as i64,
                m.expires_at,
                m.deleted as i64,
                m.read as i64,
                m.mentions_me as i64,
                m.msg.has_files() as i64
            ])?;
        Ok(n > 0)
    }
    /// 插入收到的消息。消息 id 由发送方决定，不同发送者可能（恶意或巧合）使用同一个 id：
    /// 主键冲突且已有行属于**别的发送者**时，改用派生 id `发送者:原id` 存储（写回 `m.id`），两条都保留；
    /// 同一发送者的同一 id 视为重复，返回 false。
    pub fn insert_incoming(&self, m: &mut MsgRow) -> Result<bool> {
        if self.insert_message(m)? {
            return Ok(true);
        }
        if self.message_sender(&m.id)?.as_deref() == Some(m.sender_id.as_str()) {
            return Ok(false);
        }
        let alt = derived_message_id(&m.sender_id, &m.id);
        let orig = std::mem::replace(&mut m.id, alt);
        let inserted = self.insert_message(m)?;
        if !inserted {
            m.id = orig;
        }
        Ok(inserted)
    }

    fn message_sender(&self, id: &str) -> Result<Option<String>> {
        Ok(self.conn().query_row("SELECT sender_id FROM messages WHERE id=?1", [id], |r| r.get(0)).optional()?)
    }

    /// 按协议里引用的内容 id 查找某发送者的消息（`sender_id` 对自己发出的消息就是自己的 user_id）。
    /// 先查原 id，再查冲突时使用的派生 id。`conv_id` 为 Some 时要求在该会话内。
    pub fn find_by_content(&self, conv_id: Option<&str>, content_id: &str, sender_id: &str) -> Result<Option<MsgRow>> {
        for id in [content_id.to_string(), derived_message_id(sender_id, content_id)] {
            if let Some(m) = self.get_message(&id)? {
                if m.sender_id == sender_id && conv_id.is_none_or(|c| c == m.conv_id) {
                    return Ok(Some(m));
                }
            }
        }
        Ok(None)
    }

    /// 在库锁内完成「读出 → 修改 → 写回」（A3）。`f` 返回 true 表示有改动，此时写回并返回修改后的行；
    /// 消息不存在或 `f` 返回 false 时返回 None。
    ///
    /// 以前的写法是调用方先读出整行、稍后整行写回：两条路径交错时后写的会覆盖先写的（`mark_read` 把并发到达的
    /// 撤回覆盖掉，已撤回的消息「复活」）。现在所有改动都基于锁内读到的最新行。只写会变化的列，
    /// 不写 server_ts / sort_ts（它们只由 `mark_sent` 修改）；正文没变时不重新加密，正文无法读取的行不会被占位覆盖。
    pub fn modify_message(&self, id: &str, f: impl FnOnce(&mut MsgRow) -> bool) -> Result<Option<MsgRow>> {
        let c = self.conn();
        let row = c
            .prepare_cached(&format!("SELECT {} FROM messages WHERE id=?1", Self::MSG_COLS))?
            .query_row([id], |r| self.msg_from_row(r))
            .optional()?;
        let Some((mut m, body)) = row else { return Ok(None) };
        let readable = self.decode_row(&mut m, &body);
        let before = m.msg.clone();
        if !f(&mut m) {
            return Ok(None);
        }
        if m.msg != before || !readable {
            if !readable && m.msg == before {
                // 正文读不出来、调用方也没有换正文：只更新其余列
                c.execute(
                    "UPDATE messages SET status=?2, expire_secs=?3, expires_at=?4, deleted=?5, read=?6 WHERE id=?1",
                    params![m.id, m.status.as_str(), m.expire_secs as i64, m.expires_at, m.deleted as i64, m.read as i64],
                )?;
                return Ok(Some(m));
            }
            let sealed = self.cipher.seal(LABEL_BODY, &serde_json::to_vec(&m.msg)?);
            c.execute(
                "UPDATE messages SET status=?2, body=?3, expire_secs=?4, expires_at=?5, deleted=?6, read=?7, has_files=?8 WHERE id=?1",
                params![m.id, m.status.as_str(), sealed, m.expire_secs as i64, m.expires_at, m.deleted as i64, m.read as i64, m.msg.has_files() as i64],
            )?;
        } else {
            c.execute(
                "UPDATE messages SET status=?2, expire_secs=?3, expires_at=?4, deleted=?5, read=?6 WHERE id=?1",
                params![m.id, m.status.as_str(), m.expire_secs as i64, m.expires_at, m.deleted as i64, m.read as i64],
            )?;
        }
        Ok(Some(m))
    }
    /// 发出的消息已被服务端接受（K3）：记下 server_ts，排序时间改为服务端时间（会话内唯一），
    /// 发送中 / 失败改为已发送（已送达、已读保持）。返回新的 sort_ts；消息不存在时返回 None。
    pub fn mark_sent(&self, id: &str, server_ts: i64) -> Result<Option<i64>> {
        let server_ts = clamp_ts(server_ts);
        let c = self.conn();
        let row: Option<(String, i64)> =
            c.query_row("SELECT conv_id, sort_ts FROM messages WHERE id=?1", [id], |r| Ok((r.get(0)?, r.get(1)?))).optional()?;
        let Some((conv_id, old_sort)) = row else { return Ok(None) };
        let sort_ts = if server_ts > 0 { Self::free_sort_ts(&c, &conv_id, server_ts, id)? } else { old_sort };
        c.execute(
            "UPDATE messages SET server_ts=?2, sort_ts=?3,
               status=CASE WHEN status IN ('sending','failed') THEN 'sent' ELSE status END WHERE id=?1",
            params![id, server_ts, sort_ts],
        )?;
        Ok(Some(sort_ts))
    }
    /// 会话已读（C4 / A3）：一个事务里取出未读的收到消息，再用**一条** UPDATE 标为已读并给阅后即焚消息开始计时
    /// （部分索引 `messages_unread`，没有未读时几乎不花时间）。只改 read / status / expires_at 三列，
    /// 不碰正文与 deleted，所以不会覆盖并发到达的撤回。返回这些消息（字段已是更新后的值）。
    pub fn mark_conv_read(&self, conv_id: &str, now: i64) -> Result<Vec<MsgRow>> {
        let rows = {
            let mut c = self.conn();
            let tx = c.transaction()?;
            let rows = {
                // 指定部分索引：不然规划器会为了别的条件去走 (conv_id, sort_ts)，把整个会话扫一遍
                let mut st = tx.prepare_cached(&format!(
                    "SELECT {} FROM messages INDEXED BY messages_unread WHERE conv_id=?1 AND outgoing=0 AND read=0",
                    Self::MSG_COLS
                ))?;
                let mut v = st.query_map([conv_id], |r| self.msg_from_row(r))?.collect::<std::result::Result<Vec<_>, _>>()?;
                v.sort_by_key(|(m, _)| m.sort_ts);
                v
            };
            if !rows.is_empty() {
                tx.execute(
                    "UPDATE messages INDEXED BY messages_unread SET read=1, status='read',
                       expires_at=CASE WHEN expire_secs>0 AND expires_at IS NULL THEN ?2 + expire_secs*1000 ELSE expires_at END
                     WHERE conv_id=?1 AND outgoing=0 AND read=0",
                    params![conv_id, now],
                )?;
            }
            tx.execute("UPDATE conversations SET unread=0 WHERE conv_id=?1 AND unread<>0", [conv_id])?;
            tx.commit()?;
            rows
        };
        let mut out = self.decode_rows(rows)?;
        for m in &mut out {
            m.read = true;
            m.status = MessageStatus::Read;
            if m.expire_secs > 0 && m.expires_at.is_none() {
                m.expires_at = Some(now + m.expire_secs as i64 * 1000);
            }
        }
        Ok(out)
    }
    /// 启动时调用：上次进程退出时仍处于「发送中」的消息不可能还在发送，改为「发送失败」，以便用户重发。
    /// 返回受影响的条数。
    pub fn fail_interrupted_sends(&self) -> Result<usize> {
        Ok(self.conn().execute(
            "UPDATE messages SET status=?1 WHERE outgoing=1 AND status=?2",
            params![MessageStatus::Failed.as_str(), MessageStatus::Sending.as_str()],
        )?)
    }
    pub fn get_message(&self, id: &str) -> Result<Option<MsgRow>> {
        Ok(self.query_msgs("WHERE id=?1", &[&id])?.into_iter().next())
    }
    pub fn messages(&self, conv_id: &str, before: Option<i64>, limit: u32) -> Result<Vec<MsgRow>> {
        let before = before.unwrap_or(i64::MAX);
        let mut v = self.query_msgs(
            "WHERE conv_id=?1 AND sort_ts<?2 ORDER BY sort_ts DESC, rowid DESC LIMIT ?3",
            &[&conv_id, &before, &(limit as i64)],
        )?;
        v.reverse();
        Ok(v)
    }
    pub fn last_message(&self, conv_id: &str) -> Result<Option<MsgRow>> {
        Ok(self
            .query_msgs("WHERE conv_id=?1 ORDER BY sort_ts DESC, rowid DESC LIMIT 1", &[&conv_id])?
            .into_iter()
            .next())
    }
    /// 该会话里有本地附件文件的消息（C5：删除 / 清空会话时只需要它们，不必解密全部消息）。
    pub fn messages_with_files(&self, conv_id: &str) -> Result<Vec<MsgRow>> {
        self.query_msgs("INDEXED BY messages_files WHERE conv_id=?1 AND has_files=1", &[&conv_id])
    }
    /// 本地搜索（C2）：从最近写入的消息往前分批扫描（按 rowid，不需要排序），逐条解密后只解析搜索需要的字段
    /// （不为缩略图等分配内存），命中的才完整解码；够 `limit` 条即停，结果按排序时间从新到旧。
    /// 每批之间释放库锁，不会长时间挡住收发消息。`q` 须已转小写。
    pub fn search_messages(&self, q: &str, limit: usize) -> Result<Vec<MsgRow>> {
        const BATCH: i64 = 500;
        let mut out = Vec::new();
        let mut cursor = i64::MAX;
        'scan: loop {
            let rows: Vec<(i64, MsgRow, Vec<u8>)> = {
                let c = self.conn();
                let mut st = c.prepare_cached(&format!(
                    "SELECT {},rowid FROM messages WHERE rowid<?1 AND deleted=0 ORDER BY rowid DESC LIMIT ?2",
                    Self::MSG_COLS
                ))?;
                let v = st
                    .query_map(params![cursor, BATCH], |r| {
                        let (m, body) = self.msg_from_row(r)?;
                        Ok((r.get::<_, i64>(14)?, m, body))
                    })?
                    .collect::<std::result::Result<Vec<_>, _>>()?;
                v
            };
            let Some(last) = rows.last() else { break };
            cursor = last.0;
            let full = rows.len() as i64 == BATCH;
            for (_, mut m, body) in rows {
                let Ok(plain) = self.cipher.open(LABEL_BODY, &body) else { continue };
                if !serde_json::from_slice::<SlimMessage>(&plain).is_ok_and(|s| s.matches(q)) {
                    continue;
                }
                if let Ok(msg) = serde_json::from_slice::<StoredMessage>(&plain) {
                    m.msg = msg;
                    out.push(m);
                    if out.len() >= limit {
                        break 'scan;
                    }
                }
            }
            if !full {
                break;
            }
        }
        out.sort_by_key(|m| std::cmp::Reverse(m.sort_ts));
        Ok(out)
    }
    pub fn delete_message(&self, id: &str) -> Result<()> {
        let c = self.conn();
        c.execute("DELETE FROM messages WHERE id=?1", [id])?;
        c.execute("DELETE FROM reactions WHERE message_id=?1", [id])?;
        Ok(())
    }
    pub fn clear_conversation(&self, conv_id: &str) -> Result<Vec<String>> {
        let c = self.conn();
        let ids: Vec<String> = {
            let mut st = c.prepare("SELECT id FROM messages WHERE conv_id=?1")?;
            let v = st.query_map([conv_id], |r| r.get(0))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        c.execute("DELETE FROM reactions WHERE message_id IN (SELECT id FROM messages WHERE conv_id=?1)", [conv_id])?;
        c.execute("DELETE FROM messages WHERE conv_id=?1", [conv_id])?;
        c.execute("UPDATE conversations SET unread=0 WHERE conv_id=?1", [conv_id])?;
        Ok(ids)
    }
    /// 删除会话（K1）：全部消息、回应、草稿、会话行，以及群会话里仍在等 room_key 的挂起消息（群信息、群密钥不动）。免打扰设置记在 kv 里，
    /// 会话因新消息重新出现时沿用（见 `ensure_conv`）。附件缓存文件由调用方先删除。
    pub fn delete_conversation(&self, conv_id: &str) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        let muted: Option<i64> = tx.query_row("SELECT muted FROM conversations WHERE conv_id=?1", [conv_id], |r| r.get(0)).optional()?;
        tx.execute("DELETE FROM reactions WHERE message_id IN (SELECT id FROM messages WHERE conv_id=?1)", [conv_id])?;
        tx.execute("DELETE FROM messages WHERE conv_id=?1", [conv_id])?;
        tx.execute("DELETE FROM drafts WHERE conv_id=?1", [conv_id])?;
        tx.execute("DELETE FROM conversations WHERE conv_id=?1", [conv_id])?;
        // 群会话：丢弃删除前还在等 room_key 的旧群消息，否则密钥到达后旧消息会让会话带着旧内容重新出现
        if let Some(gid) = conv_id.strip_prefix("g:") {
            tx.execute("DELETE FROM megolm_pending WHERE group_id=?1", [gid])?;
        }
        tx.execute("DELETE FROM kv WHERE k=?1", [super::muted_hint_key(conv_id)])?;
        if muted.unwrap_or(0) != 0 {
            tx.execute("INSERT OR REPLACE INTO kv(k,v) VALUES(?1, x'01')", [super::muted_hint_key(conv_id)])?;
        }
        tx.commit()?;
        Ok(())
    }

    // ---------- drafts ----------
    /// 保存 / 清除草稿（K5，字段加密）。`text` 为 None 或空白时清除。内容没变时不改更新时间。返回是否有变化。
    pub fn set_draft(&self, conv_id: &str, text: Option<&str>, now: i64) -> Result<bool> {
        let text = text.filter(|t| !t.trim().is_empty());
        let cur = self.get_draft(conv_id)?;
        if cur.as_deref() == text {
            return Ok(false);
        }
        let c = self.conn();
        match text {
            Some(t) => {
                c.execute(
                    "INSERT OR REPLACE INTO drafts(conv_id,text,updated_at) VALUES(?1,?2,?3)",
                    params![conv_id, self.cipher.seal(LABEL_DRAFT, t.as_bytes()), now],
                )?;
            }
            None => {
                c.execute("DELETE FROM drafts WHERE conv_id=?1", [conv_id])?;
            }
        }
        Ok(true)
    }
    pub fn get_draft(&self, conv_id: &str) -> Result<Option<String>> {
        let blob: Option<Vec<u8>> =
            self.conn().query_row("SELECT text FROM drafts WHERE conv_id=?1", [conv_id], |r| r.get(0)).optional()?;
        Ok(blob.and_then(|b| self.open_draft(&b)))
    }
    /// 解密草稿；损坏时视为没有草稿。
    pub fn open_draft(&self, blob: &[u8]) -> Option<String> {
        match self.cipher.open(LABEL_DRAFT, blob).map(String::from_utf8) {
            Ok(Ok(s)) => Some(s),
            _ => {
                tracing::warn!("草稿无法读取，忽略");
                None
            }
        }
    }
    /// 到期的阅后即焚消息 (id, conv_id)。
    pub fn expired_messages(&self, now: i64) -> Result<Vec<(String, String)>> {
        let c = self.conn();
        let mut st = c.prepare("SELECT id,conv_id FROM messages WHERE expires_at IS NOT NULL AND expires_at<=?1")?;
        let v = st.query_map([now], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<std::result::Result<Vec<_>, _>>()?;
        Ok(v)
    }

    // ---------- reactions ----------
    pub fn set_reaction(&self, mid: &str, uid: &str, emoji: &str) -> Result<()> {
        self.conn().execute(
            "INSERT OR REPLACE INTO reactions(message_id,user_id,emoji,ts) VALUES(?1,?2,?3,?4)",
            params![mid, uid, emoji, crate::util::now_ms()],
        )?;
        Ok(())
    }
    pub fn remove_reaction(&self, mid: &str, uid: &str, emoji: &str) -> Result<()> {
        self.conn()
            .execute("DELETE FROM reactions WHERE message_id=?1 AND user_id=?2 AND emoji=?3", params![mid, uid, emoji])?;
        Ok(())
    }
    pub fn reactions(&self, mid: &str) -> Result<Vec<ReactionGroup>> {
        let rows: Vec<(String, String)> = {
            let c = self.conn();
            let mut st = c.prepare("SELECT emoji,user_id FROM reactions WHERE message_id=?1 ORDER BY ts")?;
            let v = st.query_map([mid], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        let mut order: Vec<String> = Vec::new();
        let mut map: BTreeMap<String, Vec<String>> = BTreeMap::new();
        for (e, u) in rows {
            if !map.contains_key(&e) {
                order.push(e.clone());
            }
            map.entry(e).or_default().push(u);
        }
        Ok(order.into_iter().map(|e| ReactionGroup { user_ids: map.remove(&e).unwrap_or_default(), emoji: e }).collect())
    }

    // ---------- dedupe ----------
    pub fn is_seen(&self, id: &str) -> Result<bool> {
        Ok(self.conn().query_row("SELECT 1 FROM seen WHERE id=?1", [id], |_| Ok(())).optional()?.is_some())
    }
    pub fn mark_seen(&self, id: &str) -> Result<()> {
        let c = self.conn();
        c.execute("INSERT OR IGNORE INTO seen(id,ts) VALUES(?1,?2)", params![id, crate::util::now_ms()])?;
        Ok(())
    }
    pub fn prune_seen(&self, older_than: i64) -> Result<()> {
        self.conn().execute("DELETE FROM seen WHERE ts<?1", [older_than])?;
        Ok(())
    }

    /// 读取某列原始字节（测试用：验证落盘为密文）。
    #[cfg(any(test, feature = "test-hooks"))]
    pub fn raw_message_body(&self, id: &str) -> Result<Option<Vec<u8>>> {
        Ok(self.conn().query_row("SELECT body FROM messages WHERE id=?1", [id], |r| r.get(0)).optional()?)
    }
}
