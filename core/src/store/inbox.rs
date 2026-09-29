//! 收件箱（C-H）：收到的信封先原样落库，再解密处理。
//!
//! 解密成功时，推进后的棘轮状态（Olm 会话 / 账户、Megolm 入站会话）与明文在**同一事务**写入：
//! 明文存进该信封的 inbox 行（库密钥字段加密）。之后进程若在明文落成消息之前被杀，重启后（或服务端重投时）
//! 直接用保存的明文继续处理，不会再去解密一个棘轮已推进的信封而得到「无法解密」。
//! 处理完成（或确定永久失败）时，在同一事务内记 seen 并删除该行。

use super::*;
use crate::api::Envelope;

const LABEL_INBOX: &str = "inbox.plaintext";

/// 已解密、尚未处理完的明文及解密上下文。
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct InboxPlain {
    /// 解密得到的明文（base64）
    pub plaintext: String,
    /// 经 Olm 解密时所用会话 id（session_reset 需要）
    #[serde(default)]
    pub olm_session_id: Option<String>,
}

impl Store {
    /// 信封先落库（已存在则保留原行，包括其中已保存的明文）。
    pub fn inbox_put(&self, env: &Envelope) -> Result<()> {
        self.conn().execute(
            "INSERT OR IGNORE INTO inbox(envelope_id,envelope,received_at) VALUES(?1,?2,?3)",
            params![env.id, serde_json::to_string(env)?, crate::util::now_ms()],
        )?;
        Ok(())
    }

    /// 该信封此前已解密并保存的明文。明文记录损坏时按未解密处理（记日志）。
    pub fn inbox_plaintext(&self, envelope_id: &str) -> Result<Option<InboxPlain>> {
        let v: Option<Option<Vec<u8>>> = self
            .conn()
            .query_row("SELECT plaintext FROM inbox WHERE envelope_id=?1", [envelope_id], |r| r.get(0))
            .optional()?;
        let Some(Some(blob)) = v else { return Ok(None) };
        match self.cipher.open(LABEL_INBOX, &blob).and_then(|p| serde_json::from_slice::<InboxPlain>(&p).map_err(HushError::from)) {
            Ok(p) => Ok(Some(p)),
            Err(e) => {
                tracing::warn!("收件箱 {envelope_id} 的明文无法读取，按未解密处理: {e}");
                Ok(None)
            }
        }
    }

    /// 在调用方的事务内把明文写入 inbox 行（该信封不在收件箱中时不做任何事，例如挂起后重试的群消息）。
    pub(super) fn write_inbox_plain(&self, c: &Connection, envelope_id: &str, p: &InboxPlain) -> Result<()> {
        c.execute(
            "UPDATE inbox SET plaintext=?2 WHERE envelope_id=?1",
            params![envelope_id, self.cipher.seal(LABEL_INBOX, &serde_json::to_vec(p)?)],
        )?;
        Ok(())
    }

    /// 残留的收件箱信封（按收到顺序）。无法解析的行直接删除。
    pub fn inbox_list(&self) -> Result<Vec<Envelope>> {
        let rows: Vec<(String, String)> = {
            let c = self.conn();
            let mut st = c.prepare("SELECT envelope_id,envelope FROM inbox ORDER BY received_at, rowid")?;
            let v = st.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?.collect::<std::result::Result<Vec<_>, _>>()?;
            v
        };
        let mut out = Vec::with_capacity(rows.len());
        for (id, json) in rows {
            match serde_json::from_str::<Envelope>(&json) {
                Ok(e) => out.push(e),
                Err(e) => {
                    tracing::warn!("收件箱 {id} 无法解析，删除: {e}");
                    self.inbox_delete(&id)?;
                }
            }
        }
        Ok(out)
    }

    pub fn inbox_delete(&self, envelope_id: &str) -> Result<()> {
        self.conn().execute("DELETE FROM inbox WHERE envelope_id=?1", [envelope_id])?;
        Ok(())
    }

    /// 信封处理完：同一事务内记 seen 并删除 inbox 行。
    pub fn finish_envelope(&self, seen_key: &str, envelope_id: &str) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        tx.execute("INSERT OR IGNORE INTO seen(id,ts) VALUES(?1,?2)", params![seen_key, crate::util::now_ms()])?;
        tx.execute("DELETE FROM inbox WHERE envelope_id=?1", [envelope_id])?;
        tx.commit()?;
        Ok(())
    }

    #[cfg(any(test, feature = "test-hooks"))]
    pub fn inbox_count(&self) -> Result<i64> {
        Ok(self.conn().query_row("SELECT COUNT(*) FROM inbox", [], |r| r.get(0))?)
    }
}
