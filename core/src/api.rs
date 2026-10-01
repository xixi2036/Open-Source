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

/// 主机是否在本机 / 内网（B1）：回环、RFC1918 私有网段、链路本地、CGNAT（100.64/10，Tailscale 等）、
/// IPv6 的 ULA 与链路本地，以及只在内网能解析的主机名（`localhost`、单标签主机名、`.local` / `.lan` /
/// `.internal` / `.home.arpa` / `.localhost`）。这些地址允许明文 http（开发机、模拟器的 `10.0.2.2`、局域网自建服务）。
pub fn is_local_host(host: &url::Host<&str>) -> bool {
    fn v4(ip: &std::net::Ipv4Addr) -> bool {
        let o = ip.octets();
        ip.is_loopback() || ip.is_private() || ip.is_link_local() || (o[0] == 100 && (o[1] & 0xC0) == 64)
    }
    match host {
        url::Host::Ipv4(ip) => v4(ip),
        url::Host::Ipv6(ip) => {
            let seg = ip.segments()[0];
            ip.is_loopback() || (seg & 0xFE00) == 0xFC00 || (seg & 0xFFC0) == 0xFE80 || ip.to_ipv4_mapped().is_some_and(|m| v4(&m))
        }
        url::Host::Domain(d) => {
            let d = d.trim_end_matches('.').to_ascii_lowercase();
            d == "localhost"
                || !d.contains('.')
                || [".localhost", ".local", ".lan", ".internal", ".home.arpa"].iter().any(|suffix| d.ends_with(suffix))
        }
    }
}

/// 检查已规范化的服务器地址是否允许使用（B1）：必须是 http / https；**公网地址只允许 https**，
/// 除非 `allow_insecure_http`（调试开关）。明文 http 下 token、元数据都在链路上裸奔，任何中间人都能伪造响应。
pub fn ensure_secure(base: &str, allow_insecure_http: bool) -> Result<()> {
    let u = url::Url::parse(base).map_err(|e| HushError::invalid(format!("服务器地址无效: {e}")))?;
    let host = u.host().ok_or_else(|| HushError::invalid("服务器地址缺少主机名"))?;
    match u.scheme() {
        "https" => Ok(()),
        "http" if allow_insecure_http || is_local_host(&host) => Ok(()),
        "http" => Err(HushError::invalid("公网服务器地址必须使用 https（只有本机与内网地址允许 http）")),
        other => Err(HushError::invalid(format!("不支持的协议 {other}，服务器地址须为 https:// 或 http://"))),
    }
}

/// 规范化服务器地址：去掉末尾 `/`；没写协议时，本机 / 内网地址补 `http://`，其余补 `https://`（B1）。
/// 公网地址显式写 `http://` 会被拒绝，除非 `allow_insecure_http`。
pub fn normalize_base(url: &str, allow_insecure_http: bool) -> Result<String> {
    let mut s = url.trim().trim_end_matches('/').to_string();
    if s.is_empty() {
        return Err(HushError::invalid("服务器地址为空"));
    }
    if !s.contains("://") {
        let local = url::Url::parse(&format!("http://{s}"))
            .map_err(|e| HushError::invalid(format!("服务器地址无效: {e}")))?
            .host()
            .is_some_and(|h| is_local_host(&h));
        s = format!("{}://{s}", if local || allow_insecure_http { "http" } else { "https" });
    }
    ensure_secure(&s, allow_insecure_http)?;
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
    /// `max_bytes`（B2）：响应声明的长度或实际收到的字节数超过它就中止——调用方按消息里声明的附件大小给出上限。
    pub async fn download_to(&self, id: &str, path: &std::path::Path, max_bytes: u64) -> Result<([u8; 32], u64)> {
        use futures_util::StreamExt;
        use sha2::{Digest, Sha256};
        use tokio::io::AsyncWriteExt;
        let rb = self.req(Method::GET, &format!("/v1/attachments/{}", enc(id))).timeout(BLOB_TIMEOUT);
        let resp = self.check(rb.send().await?).await?;
        let too_large = || HushError::invalid("附件比消息里声明的大小大，已中止下载");
        if resp.content_length().is_some_and(|n| n > max_bytes) {
            return Err(too_large());
        }
        let mut f = tokio::fs::File::create(path).await?;
        let mut hasher = Sha256::new();
        let mut n = 0u64;
        let mut body = resp.bytes_stream();
        while let Some(chunk) = body.next().await {
            let chunk = chunk?;
            n += chunk.len() as u64;
            if n > max_bytes {
                return Err(too_large());
            }
            hasher.update(&chunk);
            f.write_all(&chunk).await?;
        }
        f.flush().await?;
        f.sync_all().await?;
        Ok((hasher.finalize().into(), n))
    }

    /// 下载到内存（头像、朋友圈图片等明文媒体）。超过 `max_bytes` 中止（B2）。
    pub async fn download(&self, id: &str, max_bytes: u64) -> Result<Vec<u8>> {
        use futures_util::StreamExt;
        let rb = self.req(Method::GET, &format!("/v1/attachments/{}", enc(id))).timeout(BLOB_TIMEOUT);
        let resp = self.check(rb.send().await?).await?;
        let too_large = || HushError::invalid("媒体文件过大，已中止下载");
        if resp.content_length().is_some_and(|n| n > max_bytes) {
            return Err(too_large());
        }
        let mut out = Vec::new();
        let mut body = resp.bytes_stream();
        while let Some(chunk) = body.next().await {
            let chunk = chunk?;
            if (out.len() + chunk.len()) as u64 > max_bytes {
                return Err(too_large());
            }
            out.extend_from_slice(&chunk);
        }
        Ok(out)
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
        assert_eq!(normalize_base("127.0.0.1:8080/", false).unwrap(), "http://127.0.0.1:8080");
        assert_eq!(normalize_base("https://a.b", false).unwrap(), "https://a.b");
        assert!(normalize_base("  ", false).is_err());
    }

    /// B1：本机与内网地址照常可以用 http（两端与本地部署都这么用）；公网地址强制 https。
    #[test]
    fn public_hosts_require_https() {
        let n = |u: &str| normalize_base(u, false);
        // 现有用法保持可用
        for ok in [
            "http://10.0.2.2:8080",
            "http://127.0.0.1:8080",
            "http://localhost:8080",
            "http://192.168.1.23:8080",
            "http://172.16.5.4",
            "http://172.31.255.1:80",
            "http://169.254.10.1",
            "http://100.100.1.2:8080",
            "http://[::1]:8080",
            "http://[fd12:3456::1]:8080",
            "http://[fe80::1]:8080",
            "http://hush.local:8080",
            "http://nas.lan",
            "http://hush-server:8080",
            "https://hush.example.com",
        ] {
            assert_eq!(n(ok).unwrap(), ok, "{ok}");
        }
        // 没写协议：内网补 http，公网补 https
        assert_eq!(n("192.168.1.23:8080").unwrap(), "http://192.168.1.23:8080");
        assert_eq!(n("10.0.2.2:8080").unwrap(), "http://10.0.2.2:8080");
        assert_eq!(n("hush.example.com").unwrap(), "https://hush.example.com");
        assert_eq!(n("8.8.8.8:8443").unwrap(), "https://8.8.8.8:8443");
        // 公网明文 http：拒绝
        for bad in ["http://hush.example.com", "http://8.8.8.8:8080", "http://172.32.0.1", "http://100.128.0.1", "http://[2001:db8::1]", "http://evil.localhost.example.com"] {
            assert!(matches!(n(bad), Err(HushError::InvalidInput { .. })), "{bad}");
            assert!(ensure_secure(bad, false).is_err());
        }
        assert!(n("ftp://a.b").is_err());
        assert!(n("ws://127.0.0.1").is_err());
        // 调试开关
        assert_eq!(normalize_base("http://hush.example.com", true).unwrap(), "http://hush.example.com");
        assert_eq!(normalize_base("hush.example.com", true).unwrap(), "http://hush.example.com", "开关打开时保持旧的缺省协议");
        assert!(ensure_secure("http://8.8.8.8", true).is_ok());
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
