//! 朋友圈（明文，服务端可见）。

use serde_json::json;

use super::Inner;
use crate::api::{self, Empty};
use crate::error::{HushError, Result};
use crate::types::*;
use crate::util::image_dimensions;

fn page_query(before: Option<i64>, before_id: Option<&str>) -> String {
    match (before, before_id) {
        (Some(b), Some(id)) if b > 0 => format!("?before={b}&before_id={}&limit=20", crate::api::enc(id)),
        (Some(b), _) if b > 0 => format!("?before={b}&limit=20"),
        _ => "?limit=20".to_string(),
    }
}

/// 翻页时补上 before_id：导出的 `moments_feed(before)` 只有时间戳参数，平台通常传上一页最后一条的 `created_at`。
/// 若它正好等于上一页最后一条的时间，就带上那条的 id 作为复合游标，同一毫秒的动态跨页时不会漏。
fn cursor_id(before: Option<i64>, last: Option<&(i64, String)>) -> Option<String> {
    match (before, last) {
        (Some(b), Some((ts, id))) if b == *ts => Some(id.clone()),
        _ => None,
    }
}

impl Inner {
    fn cache_moment_profiles(&self, m: &Moment) {
        let _ = self.store.put_profile(&m.author);
        for p in &m.likes {
            let _ = self.store.put_profile(p);
        }
        for c in &m.comments {
            let _ = self.store.put_profile(&c.author);
        }
    }

    fn remembered_cursor(&self, key: &str, before: Option<i64>) -> Option<String> {
        let m = self.moment_cursors.lock().unwrap_or_else(|e| e.into_inner());
        cursor_id(before, m.get(key))
    }

    fn remember_cursor(&self, key: &str, list: &[Moment]) {
        if let Some(last) = list.last() {
            self.moment_cursors.lock().unwrap_or_else(|e| e.into_inner()).insert(key.to_string(), (last.created_at, last.id.clone()));
        }
    }

    pub(crate) async fn moments_feed(&self, before: Option<i64>) -> Result<Vec<Moment>> {
        let bid = self.remembered_cursor("feed", before);
        let list: Vec<Moment> = self.api()?.get(&format!("/v1/moments/feed{}", page_query(before, bid.as_deref()))).await?;
        list.iter().for_each(|m| self.cache_moment_profiles(m));
        self.remember_cursor("feed", &list);
        Ok(list)
    }

    pub(crate) async fn user_moments(&self, uid: &str, before: Option<i64>) -> Result<Vec<Moment>> {
        let key = format!("user:{uid}");
        let bid = self.remembered_cursor(&key, before);
        let list: Vec<Moment> =
            self.api()?.get(&format!("/v1/moments/user/{}{}", api::enc(uid), page_query(before, bid.as_deref()))).await?;
        list.iter().for_each(|m| self.cache_moment_profiles(m));
        self.remember_cursor(&key, &list);
        Ok(list)
    }

    /// 单条动态（C-D，`GET /v1/moments/{id}`）。已删除或对我不可见时服务端返回 404，这里转为 `NotFound`。
    pub(crate) async fn moment(&self, id: &str) -> Result<Moment> {
        if id.is_empty() {
            return Err(HushError::invalid("动态 id 为空"));
        }
        match self.api()?.get::<Moment>(&format!("/v1/moments/{}", api::enc(id))).await {
            Ok(m) => {
                self.cache_moment_profiles(&m);
                Ok(m)
            }
            Err(e) if e.is_server_code("not_found") => Err(HushError::not_found("该动态已不可见")),
            Err(e) => Err(e),
        }
    }

    pub(crate) async fn post_moment(&self, text: String, image_paths: Vec<String>, visibility: Visibility) -> Result<Moment> {
        if text.trim().is_empty() && image_paths.is_empty() {
            return Err(HushError::invalid("动态内容为空"));
        }
        if image_paths.len() > 9 {
            return Err(HushError::invalid("最多 9 张图片"));
        }
        let api = self.api()?;
        let mut media = Vec::new();
        for p in &image_paths {
            let data = tokio::fs::read(p).await.map_err(|e| HushError::invalid(format!("读取图片失败: {e}")))?;
            let (w, h) = image_dimensions(&data);
            let id = self.upload_media(&api, data).await?;
            media.push(MomentMedia { attachment_id: id, width: w, height: h });
        }
        let m: Moment = api.post("/v1/moments", &json!({"text": text, "media": media, "visibility": visibility})).await?;
        Ok(m)
    }

    pub(crate) async fn delete_moment(&self, id: &str) -> Result<()> {
        let _: Empty = self.api()?.delete(&format!("/v1/moments/{}", api::enc(id))).await?;
        Ok(())
    }

    pub(crate) async fn like_moment(&self, id: &str, like: bool) -> Result<()> {
        let path = format!("/v1/moments/{}/like", api::enc(id));
        let api = self.api()?;
        let _: Empty = if like { api.post_empty(&path).await? } else { api.delete(&path).await? };
        Ok(())
    }

    pub(crate) async fn comment_moment(&self, id: &str, text: &str, reply_to: Option<String>) -> Result<Comment> {
        if text.trim().is_empty() {
            return Err(HushError::invalid("评论为空"));
        }
        let mut body = json!({"text": text});
        if let Some(r) = reply_to.filter(|r| !r.is_empty()) {
            body["reply_to_user_id"] = json!(r);
        }
        self.api()?.post(&format!("/v1/moments/{}/comments", api::enc(id)), &body).await
    }

    pub(crate) async fn delete_comment(&self, cid: &str) -> Result<()> {
        let _: Empty = self.api()?.delete(&format!("/v1/moments/comments/{}", api::enc(cid))).await?;
        Ok(())
    }

    pub(crate) async fn moment_notifications(&self, before: Option<i64>) -> Result<Vec<MomentNotification>> {
        let q = match before {
            Some(b) if b > 0 => format!("?before={b}"),
            _ => String::new(),
        };
        self.api()?.get(&format!("/v1/moments/notifications{q}")).await
    }

    pub(crate) async fn turn_credentials(&self) -> Result<TurnCredentials> {
        self.api()?.get("/v1/calls/turn").await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// C-D：单条动态；404（已删除 / 不可见）→ NotFound。
    #[tokio::test]
    async fn single_moment_and_not_found() {
        let srv = crate::client::stub::start(std::sync::Arc::new(|_m, path, _b| match path {
            "/v1/moments/m1" => (200, r#"{"id":"m1","author":{"id":"u9","name":"九"},"text":"hi","media":[],"likes":[],"comments":[],"created_at":5}"#.into()),
            _ => (404, r#"{"error":"not_found","message":"动态不存在"}"#.into()),
        }))
        .await;
        let (inner, _dir) = crate::client::test_inner_at(&srv.url);
        let m = inner.moment("m1").await.unwrap();
        assert_eq!((m.id.as_str(), m.text.as_str(), m.author.id.as_str()), ("m1", "hi", "u9"));
        assert_eq!(inner.store.get_profile("u9").unwrap().unwrap().name, "九", "作者资料进缓存");
        assert!(matches!(inner.moment("gone").await, Err(HushError::NotFound { .. })));
        assert_eq!(srv.count("GET", "/v1/moments/gone"), 1);
    }

    #[test]
    fn page_query_with_compound_cursor() {
        assert_eq!(page_query(None, None), "?limit=20");
        assert_eq!(page_query(Some(5), None), "?before=5&limit=20");
        assert_eq!(page_query(Some(5), Some("a-b")), "?before=5&before_id=a-b&limit=20");
        assert_eq!(page_query(Some(0), Some("x")), "?limit=20");
        let last = (1000i64, "m9".to_string());
        assert_eq!(cursor_id(Some(1000), Some(&last)).as_deref(), Some("m9"), "与上一页末条同一时刻：带 before_id");
        assert_eq!(cursor_id(Some(999), Some(&last)), None);
        assert_eq!(cursor_id(None, Some(&last)), None);
        assert_eq!(cursor_id(Some(1000), None), None);
    }
}
