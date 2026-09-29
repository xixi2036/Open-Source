//! hush-server REST 客户端（契约第 3 节）。

use std::time::Duration;

use reqwest::{Method, RequestBuilder, Response};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::error::{HushError, Result};

const JSON_TIMEOUT: Duration = Duration::from_secs(20);
const BLOB_TIMEOUT: Duration = Duration::from_secs(600);

pub fn build_http() -> reqwest::Client {
    reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(10))
        // Android 模拟器等 NAT 环境会静默丢弃空闲约 20 秒的 TCP 连接：
        // 连接池空闲上限压到 8 秒，并开启 TCP keepalive，避免复用已失效的连接导致请求挂起
        .pool_idle_timeout(Duration::from_secs(8))
        .tcp_keepalive(Duration::from_secs(10))
        .user_agent(concat!("hushcore/", env!("CARGO_PKG_VERSION")))
        .build()
        .expect("reqwest client")
}

/// 规范化服务器地址：去掉末尾 `/`，缺省协议补 `http://`。
pub fn normalize_base(url: &str) -> Result<String> {
    let mut s = url.trim().trim_end_matches('/').to_string();
    if s.is_empty() {
        return Err(HushError::invalid("服务器地址为空"));
    }
    if !s.starts_with("http://") && !s.starts_with("https://") {
        s = format!("http://{s}");
    }
    url::Url::parse(&s).map_err(|e| HushError::invalid(format!("服务器地址无效: {e}")))?;
    Ok(s)
}

/// 带 token 的请求收到 401 时的回调（C-G：清除本地登录态）。
pub type UnauthorizedHook = std::sync::Arc<dyn Fn() + Send + Sync>;

#[derive(Clone)]
pub struct Api {
    http: reqwest::Client,
    base: String,
    token: Option<String>,
    on_unauthorized: Option<UnauthorizedHook>,
}

#[derive(Deserialize)]
struct ErrBody {
    #[serde(default)]
    error: String,
    #[serde(default)]
    message: String,
}

impl Api {
    pub fn new(http: reqwest::Client, base: String, token: Option<String>) -> Self {
        Api { http, base, token, on_unauthorized: None }
    }

    /// 设置 401 回调。只对带 token 的请求生效（登录前的 request_code / verify_code 不会触发）。
    pub fn on_unauthorized(mut self, hook: UnauthorizedHook) -> Self {
        self.on_unauthorized = Some(hook);
        self
    }

    fn req(&self, method: Method, path: &str) -> RequestBuilder {
        let mut rb = self.http.request(method, format!("{}{}", self.base, path));
        if let Some(t) = &self.token {
            rb = rb.bearer_auth(t);
        }
        rb
    }

    async fn check(&self, resp: Response) -> Result<Response> {
        let status = resp.status();
        if status.is_success() {
            return Ok(resp);
        }
        if status.as_u16() == 401 && self.token.is_some() {
            if let Some(h) = &self.on_unauthorized {
                h();
            }
        }
        let text = resp.text().await.unwrap_or_default();
        let (code, message) = match serde_json::from_str::<ErrBody>(&text) {
            Ok(b) if !b.error.is_empty() => (b.error, b.message),
            _ => (
                match status.as_u16() {
                    401 => "unauthorized".to_string(),
                    403 => "forbidden".to_string(),
                    404 => "not_found".to_string(),
                    _ => format!("http_{}", status.as_u16()),
                },
                text,
            ),
        };
        Err(HushError::Server { code, message })
    }

    async fn send_json<T: DeserializeOwned>(&self, rb: RequestBuilder) -> Result<T> {
        let resp = self.check(rb.timeout(JSON_TIMEOUT).send().await?).await?;
        let bytes = resp.bytes().await?;
        if bytes.is_empty() {
            return Ok(serde_json::from_value(Value::Object(Default::default()))?);
        }
        serde_json::from_slice(&bytes).map_err(|e| HushError::internal(format!("解析服务端响应失败: {e}")))
    }

    pub async fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.send_json(self.req(Method::GET, path)).await
    }

    pub async fn post<T: DeserializeOwned, B: Serialize + ?Sized>(&self, path: &str, body: &B) -> Result<T> {
        self.send_json(self.req(Method::POST, path).json(body)).await
    }

    pub async fn post_empty<T: DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.send_json(self.req(Method::POST, path).json(&serde_json::json!({}))).await
    }

    pub async fn put<T: DeserializeOwned, B: Serialize + ?Sized>(&self, path: &str, body: &B) -> Result<T> {
        self.send_json(self.req(Method::PUT, path).json(body)).await
    }

    pub async fn delete<T: DeserializeOwned>(&self, path: &str) -> Result<T> {
        self.send_json(self.req(Method::DELETE, path)).await
    }

    /// 上传原始字节 → `{id, size}`。
    pub async fn upload(&self, data: Vec<u8>) -> Result<(String, u64)> {
        #[derive(Deserialize)]
        struct R {
            id: String,
            #[serde(default)]
            size: u64,
        }
        let rb = self
            .req(Method::POST, "/v1/attachments")
            .header(reqwest::header::CONTENT_TYPE, "application/octet-stream")
            .body(data)
            .timeout(BLOB_TIMEOUT);
        let resp = self.check(rb.send().await?).await?;
        let r: R = serde_json::from_slice(&resp.bytes().await?)?;
        Ok((r.id, r.size))
    }

    /// 流式上传文件（请求体直接读文件，内存占用与文件大小无关）→ `{id, size}`。
    /// 显式带上 Content-Length：服务端按声明长度预占上传配额、提前拒绝超限附件。
    pub async fn upload_file(&self, path: &std::path::Path, len: u64) -> Result<(String, u64)> {
        #[derive(Deserialize)]
        struct R {
            id: String,
            #[serde(default)]
            size: u64,
        }
        let f = tokio::fs::File::open(path).await?;
        let rb = self
            .req(Method::POST, "/v1/attachments")
            .header(reqwest::header::CONTENT_TYPE, "application/octet-stream")
            .header(reqwest::header::CONTENT_LENGTH, len)
            .body(reqwest::Body::from(f))
            .timeout(BLOB_TIMEOUT);
        let resp = self.check(rb.send().await?).await?;
        let r: R = serde_json::from_slice(&resp.bytes().await?)?;
        Ok((r.id, r.size))
    }

    /// 流式下载到文件，边写边算 SHA-256，返回 (digest, 字节数)。
    pub async fn download_to(&self, id: &str, path: &std::path::Path) -> Result<([u8; 32], u64)> {
        use futures_util::StreamExt;
        use sha2::{Digest, Sha256};
        use tokio::io::AsyncWriteExt;
        let rb = self.req(Method::GET, &format!("/v1/attachments/{}", enc(id))).timeout(BLOB_TIMEOUT);
        let resp = self.check(rb.send().await?).await?;
        let mut f = tokio::fs::File::create(path).await?;
        let mut hasher = Sha256::new();
        let mut n = 0u64;
        let mut body = resp.bytes_stream();
        while let Some(chunk) = body.next().await {
            let chunk = chunk?;
            hasher.update(&chunk);
            f.write_all(&chunk).await?;
            n += chunk.len() as u64;
        }
        f.flush().await?;
        f.sync_all().await?;
        Ok((hasher.finalize().into(), n))
    }

    pub async fn download(&self, id: &str) -> Result<Vec<u8>> {
        let rb = self.req(Method::GET, &format!("/v1/attachments/{}", enc(id))).timeout(BLOB_TIMEOUT);
        let resp = self.check(rb.send().await?).await?;
        Ok(resp.bytes().await?.to_vec())
    }
}

/// 路径段编码。
pub fn enc(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect::<String>().replace('+', "%20")
}

// ---------- 服务端 JSON 结构 ----------

#[derive(Debug, Deserialize)]
pub struct VerifyResp {
    pub user_id: String,
    pub token: String,
    #[serde(default)]
    pub is_new: bool,
}

#[derive(Debug, Deserialize)]
pub struct KeyCount {
    #[serde(default)]
    pub one_time_key_count: i64,
}

#[derive(Debug, Deserialize)]
pub struct ClaimedKey {
    pub key_id: String,
    pub key: String,
    pub signature: String,
    #[serde(default)]
    pub fallback: bool,
}

#[derive(Debug, Deserialize)]
pub struct ClaimResp {
    pub user_id: String,
    pub identity_key: String,
    pub signing_key: String,
    #[serde(default)]
    pub device_generation: i64,
    pub one_time_key: ClaimedKey,
}

#[derive(Debug, Deserialize)]
pub struct IdentityResp {
    pub user_id: String,
    pub identity_key: String,
    pub signing_key: String,
    #[serde(default)]
    pub device_generation: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Envelope {
    pub id: String,
    pub kind: String,
    pub from: String,
    #[serde(rename = "type", default)]
    pub msg_type: u8,
    #[serde(default)]
    pub group_id: Option<String>,
    #[serde(default)]
    pub session_id: Option<String>,
    pub body: String,
    #[serde(default)]
    pub server_ts: i64,
}

#[derive(Debug, Deserialize)]
pub struct OlmSendResult {
    #[serde(default)]
    pub to: String,
    #[serde(default)]
    pub client_id: String,
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub server_ts: i64,
    #[serde(default)]
    pub error: Option<Value>,
}

impl OlmSendResult {
    pub fn error_code(&self) -> Option<String> {
        match &self.error {
            None | Some(Value::Null) => None,
            Some(Value::String(s)) if s.is_empty() => None,
            Some(Value::String(s)) => Some(s.clone()),
            Some(v) => Some(v.get("error").and_then(|e| e.as_str()).unwrap_or("error").to_string()),
        }
    }
}

#[derive(Debug, Deserialize)]
pub struct OlmSendResp {
    #[serde(default)]
    pub results: Vec<OlmSendResult>,
}

#[derive(Debug, Deserialize)]
pub struct GroupSendResp {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub server_ts: i64,
}

/// `GET /v1/messages` 响应。信封逐条解析（见 `Inner::poll_http`），单条格式错误不会让整页解析失败。
#[derive(Debug, Default, Deserialize)]
pub struct FetchResp {
    #[serde(default)]
    pub messages: Vec<Value>,
    #[serde(default)]
    pub events: Vec<Value>,
}

#[derive(Debug, Deserialize)]
pub struct Empty {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_normalization() {
        assert_eq!(normalize_base("127.0.0.1:8080/").unwrap(), "http://127.0.0.1:8080");
        assert_eq!(normalize_base("https://a.b").unwrap(), "https://a.b");
        assert!(normalize_base("  ").is_err());
    }

    #[test]
    fn olm_result_error_codes() {
        let r: OlmSendResult = serde_json::from_str(r#"{"to":"a","client_id":"c","id":"","server_ts":0,"error":"not_friends"}"#).unwrap();
        assert_eq!(r.error_code().as_deref(), Some("not_friends"));
        let r: OlmSendResult = serde_json::from_str(r#"{"to":"a","client_id":"c","id":"x","server_ts":1}"#).unwrap();
        assert_eq!(r.error_code(), None);
    }

    #[test]
    fn envelope_parse() {
        let e: Envelope = serde_json::from_str(
            r#"{"id":"1","kind":"megolm","from":"u","type":0,"group_id":"g","session_id":"s","body":"AA==","server_ts":5}"#,
        )
        .unwrap();
        assert_eq!(e.group_id.as_deref(), Some("g"));
        let e: Envelope =
            serde_json::from_str(r#"{"id":"1","kind":"olm","from":"u","type":1,"group_id":null,"session_id":null,"body":"AA==","server_ts":5}"#)
                .unwrap();
        assert_eq!(e.msg_type, 1);
        assert!(e.session_id.is_none());
    }

    #[test]
    fn path_encoding() {
        assert_eq!(enc("a b/c"), "a%20b%2Fc");
    }
}
