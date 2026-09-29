//! 群组管理（服务端维护成员表，客户端据此轮换 Megolm 会话）。

use std::collections::HashSet;

use serde_json::json;

use super::{group_conv, Inner};
use crate::api::{self, Empty};
use crate::error::{HushError, Result};
use crate::types::*;

impl Inner {
    /// 删除群的出站 Megolm 会话（下次发送时轮换）。必须在 crypto_lock 下进行：
    /// 否则正在持锁发送的 send_group 可能随后把旧会话写回，轮换就丢了。
    pub(crate) async fn drop_outbound(&self, gid: &str) -> Result<()> {
        let _g = self.crypto_lock.lock().await;
        self.store.delete_outbound(gid)
    }

    /// 应用服务端下发的群信息：检测成员变化（有人离开 → 轮换出站会话），写系统提示。
    ///
    /// 同一群的应用按群串行（读旧值 → 比较 → 写入是一个临界区），并且丢弃 `updated_at` 早于本地的旧快照：
    /// 连接时的 refresh_groups 与 WS 上的 group_updated 事件会并发到达，否则旧快照可能覆盖新的成员表、
    /// 或者两边各插一条同样的「X 加入了群聊」。
    pub(crate) async fn apply_group(&self, g: GroupInfo) -> Result<GroupInfo> {
        let _apply = self.group_apply.lock(&g.id).await;
        let me = self.my_id()?;
        let old = self.store.get_group(&g.id)?;
        if let Some(o) = &old {
            if g.updated_at < o.updated_at {
                tracing::info!("忽略群 {} 的旧快照（{} < 本地 {}）", g.id, g.updated_at, o.updated_at);
                return Ok(o.clone());
            }
        }
        let conv = group_conv(&g.id);
        let i_am_member = g.is_member(&me);
        let new_ids: HashSet<String> = g.member_ids().into_iter().collect();
        match &old {
            Some(o) => {
                let old_ids: HashSet<String> = o.member_ids().into_iter().collect();
                let removed: Vec<&String> = old_ids.difference(&new_ids).collect();
                let added: Vec<&String> = new_ids.difference(&old_ids).collect();
                if !removed.is_empty() {
                    // 有人被移除或退出：下次发送时轮换 Megolm 会话
                    self.drop_outbound(&g.id).await?;
                }
                if o.active && i_am_member {
                    for uid in &added {
                        let name = self.display_name(uid);
                        self.insert_system(&conv, &format!("{name} 加入了群聊"))?;
                    }
                    for uid in &removed {
                        let name = self.display_name(uid);
                        self.insert_system(&conv, &format!("{name} 离开了群聊"))?;
                    }
                    if o.name != g.name && !g.name.is_empty() {
                        self.insert_system(&conv, &format!("群名已修改为「{}」", g.name))?;
                    }
                }
                if !o.active && i_am_member {
                    self.insert_system(&conv, &format!("你已加入群聊「{}」", g.name))?;
                }
            }
            None if i_am_member => {
                self.store.ensure_conv(&conv, 0)?;
                self.insert_system(&conv, &format!("你已加入群聊「{}」", g.name))?;
            }
            None => {}
        }
        if !i_am_member {
            self.drop_outbound(&g.id).await?;
            if old.as_ref().map(|o| o.active).unwrap_or(false) {
                self.insert_system(&conv, "你已不在此群聊中")?;
            }
        }
        self.store.put_group(&g, i_am_member)?;
        self.emit(CoreEvent::GroupsChanged);
        self.emit(CoreEvent::ConversationsChanged);
        let mut out = g;
        out.active = i_am_member;
        Ok(out)
    }

    /// 被移出 / 群解散。
    pub(crate) async fn on_group_removed(&self, gid: &str) -> Result<()> {
        let _apply = self.group_apply.lock(gid).await;
        self.drop_outbound(gid).await?;
        if let Some(g) = self.store.get_group(gid)? {
            if g.active {
                self.store.set_group_active(gid, false)?;
                self.insert_system(&group_conv(gid), "你已不在此群聊中")?;
            }
        }
        self.emit(CoreEvent::GroupsChanged);
        self.emit(CoreEvent::ConversationsChanged);
        Ok(())
    }

    pub(crate) async fn refresh_group(&self, gid: &str) -> Result<GroupInfo> {
        match self.api()?.get::<GroupInfo>(&format!("/v1/groups/{}", api::enc(gid))).await {
            Ok(g) => self.apply_group(g).await,
            Err(e) if e.is_server_code("not_found") || e.is_server_code("forbidden") => {
                self.on_group_removed(gid).await?;
                Err(e)
            }
            Err(e) => Err(e),
        }
    }

    pub(crate) async fn create_group(&self, name: &str, member_ids: Vec<String>) -> Result<GroupInfo> {
        if name.trim().is_empty() {
            return Err(HushError::invalid("群名不能为空"));
        }
        let g: GroupInfo = self.api()?.post("/v1/groups", &json!({"name": name, "member_ids": member_ids})).await?;
        self.apply_group(g).await
    }

    pub(crate) async fn refresh_groups(&self) -> Result<Vec<GroupInfo>> {
        let list: Vec<GroupInfo> = self.api()?.get("/v1/groups").await?;
        let ids: HashSet<String> = list.iter().map(|g| g.id.clone()).collect();
        let mut out = Vec::new();
        for g in list {
            out.push(self.apply_group(g).await?);
        }
        for g in self.store.list_groups()? {
            if g.active && !ids.contains(&g.id) {
                self.on_group_removed(&g.id).await?;
            }
        }
        Ok(out)
    }

    pub(crate) fn groups(&self) -> Result<Vec<GroupInfo>> {
        self.store.list_groups()
    }

    pub(crate) async fn update_group(
        &self,
        gid: &str,
        name: Option<String>,
        announcement: Option<String>,
        avatar_path: Option<String>,
    ) -> Result<GroupInfo> {
        let api = self.api()?;
        let mut body = serde_json::Map::new();
        if let Some(n) = name {
            body.insert("name".into(), json!(n));
        }
        if let Some(a) = announcement {
            body.insert("announcement".into(), json!(a));
        }
        if let Some(p) = avatar_path.filter(|p| !p.is_empty()) {
            let data = tokio::fs::read(&p).await.map_err(|e| HushError::invalid(format!("读取头像失败: {e}")))?;
            body.insert("avatar_id".into(), json!(self.upload_media(&api, data).await?));
        }
        let g: GroupInfo = api.put(&format!("/v1/groups/{}", api::enc(gid)), &body).await?;
        self.apply_group(g).await
    }

    pub(crate) async fn add_members(&self, gid: &str, ids: Vec<String>) -> Result<GroupInfo> {
        let g: GroupInfo = self.api()?.post(&format!("/v1/groups/{}/members", api::enc(gid)), &json!({"user_ids": ids})).await?;
        self.apply_group(g).await
    }

    pub(crate) async fn remove_member(&self, gid: &str, uid: &str) -> Result<()> {
        let _: Empty = self.api()?.delete(&format!("/v1/groups/{}/members/{}", api::enc(gid), api::enc(uid))).await?;
        // 立即轮换，不依赖事件到达
        self.drop_outbound(gid).await?;
        if uid == self.my_id()? {
            self.on_group_removed(gid).await?;
        } else {
            let _ = self.refresh_group(gid).await;
        }
        Ok(())
    }

    pub(crate) async fn leave_group(&self, gid: &str) -> Result<()> {
        let me = self.my_id()?;
        self.remove_member(gid, &me).await
    }

    pub(crate) async fn set_admin(&self, gid: &str, uid: &str, admin: bool) -> Result<GroupInfo> {
        let g: GroupInfo =
            self.api()?.put(&format!("/v1/groups/{}/admins/{}", api::enc(gid), api::enc(uid)), &json!({"admin": admin})).await?;
        self.apply_group(g).await
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::time::Duration;

    use super::*;
    use crate::client::{stub, test_group, test_inner, test_inner_at};
    use crate::crypto::megolm;
    use crate::store::StoredBody;

    fn snap(members: &[&str], updated_at: i64) -> GroupInfo {
        GroupInfo { updated_at, ..test_group("g1", members) }
    }

    fn system_texts(inner: &Inner, needle: &str) -> usize {
        inner
            .store
            .messages("g:g1", None, 100)
            .unwrap()
            .iter()
            .filter(|m| matches!(&m.msg.body, StoredBody::System { text } if text.contains(needle)))
            .count()
    }

    /// 旧快照（updated_at 更早）不能覆盖更新的成员表。
    #[tokio::test]
    async fn older_snapshot_ignored() {
        let (inner, _dir) = test_inner();
        inner.apply_group(snap(&["me", "alice", "bob"], 10)).await.unwrap();
        inner.apply_group(snap(&["me", "alice"], 20)).await.unwrap();
        let g = inner.apply_group(snap(&["me", "alice", "bob"], 15)).await.unwrap();
        assert!(!g.is_member("bob"), "返回的是本地较新的群信息");
        assert!(!inner.store.get_group("g1").unwrap().unwrap().is_member("bob"), "被移出的成员不能因旧快照回来");
        assert_eq!(system_texts(&inner, "离开了群聊"), 1);
        // 更新的快照照常应用
        inner.apply_group(snap(&["me", "alice", "bob"], 30)).await.unwrap();
        assert!(inner.store.get_group("g1").unwrap().unwrap().is_member("bob"));
    }

    /// refresh_groups 与 group_updated 并发应用同一变化：只插入一条系统消息。
    #[tokio::test]
    async fn concurrent_apply_inserts_one_system_message() {
        let (inner, _dir) = test_inner();
        inner.apply_group(snap(&["me", "alice", "bob"], 10)).await.unwrap();
        // 持有 crypto_lock，让两次应用都在 drop_outbound 处等待（旧实现中两者此时都已读到旧成员表）
        let guard = inner.crypto_lock.lock().await;
        let (a, b) = (inner.clone(), inner.clone());
        let t1 = tokio::spawn(async move { a.apply_group(snap(&["me", "alice"], 20)).await });
        let t2 = tokio::spawn(async move { b.apply_group(snap(&["me", "alice"], 20)).await });
        tokio::time::sleep(Duration::from_millis(50)).await;
        drop(guard);
        t1.await.unwrap().unwrap();
        t2.await.unwrap().unwrap();
        assert_eq!(system_texts(&inner, "离开了群聊"), 1, "同一变化只应提示一次");
    }

    fn group_json(members: &[&str], updated_at: i64) -> String {
        let ms: Vec<_> = members.iter().map(|u| serde_json::json!({"user_id": u, "role": "member", "joined_at": 0})).collect();
        serde_json::json!({"id": "g1", "name": "g", "announcement": "", "owner_id": "me", "members": ms, "updated_at": updated_at})
            .to_string()
    }

    /// room_key 先于 group_updated 到达：按本地信息发送者还不是成员，刷新群信息后接受；刷新后仍不是成员的拒绝；
    /// 服务端说我们不在群里（404）时也拒绝。
    #[tokio::test]
    async fn room_key_from_new_member_accepted_after_refresh() {
        let srv = stub::start(Arc::new(|_m, path, _b| match path {
            "/v1/groups/g1" => (200, group_json(&["me", "alice", "dave"], 5)),
            _ => (404, r#"{"error":"not_found","message":"stub"}"#.into()),
        }))
        .await;
        let (inner, _dir) = test_inner_at(&srv.url);
        inner.apply_group(snap(&["me", "alice"], 1)).await.unwrap();

        let out = megolm::new_outbound();
        let (sid, key) = (out.session_id(), megolm::export_key(&out));
        inner.accept_room_key("dave", "g1", &sid, &key).await.unwrap();
        assert!(inner.store.has_inbound("dave", &sid).unwrap(), "刷新后 dave 是成员，应接受");
        assert!(inner.store.get_group("g1").unwrap().unwrap().is_member("dave"));

        inner.accept_room_key("mallory", "g1", &sid, &key).await.unwrap();
        assert!(!inner.store.has_inbound("mallory", &sid).unwrap(), "刷新后仍不是成员，拒绝");

        inner.accept_room_key("alice", "g2", &sid, &key).await.unwrap();
        assert!(!inner.store.has_inbound("alice", &sid).unwrap(), "服务端说我们不在该群，拒绝");
        assert_eq!(srv.count("GET", "/v1/groups/g1"), 2);
    }
}
