//! WebSocket 连接建立（ws / wss）。TLS 使用 rustls + ring + webpki-roots。

use std::sync::Arc;
use std::time::Duration;

use tokio::io::{AsyncRead, AsyncWrite};
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::http::HeaderValue;
use tokio_tungstenite::tungstenite::protocol::WebSocketConfig;
use tokio_tungstenite::WebSocketStream;

/// 单个服务端帧的大小上限，与服务端 `ws.MaxFrameBytes`（1MB 信封 body + 64KB 余量）一致。
pub const MAX_FRAME_BYTES: usize = (1 << 20) + (64 << 10);

/// WS 读取配置：单帧与单条消息都不超过 `max_frame` 字节。
pub fn ws_config(max_frame: usize) -> WebSocketConfig {
    WebSocketConfig::default().max_message_size(Some(max_frame)).max_frame_size(Some(max_frame))
}

pub trait AsyncRw: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send> AsyncRw for T {}

pub type WsStream = WebSocketStream<Box<dyn AsyncRw>>;

#[derive(Debug)]
pub enum ConnectError {
    /// token 无效（HTTP 401）
    Unauthorized,
    Other(String),
}

pub fn tls_config() -> Arc<rustls::ClientConfig> {
    let roots = rustls::RootCertStore { roots: webpki_roots::TLS_SERVER_ROOTS.to_vec() };
    let cfg = rustls::ClientConfig::builder_with_provider(Arc::new(rustls::crypto::ring::default_provider()))
        .with_safe_default_protocol_versions()
        .expect("rustls 协议版本")
        .with_root_certificates(roots)
        .with_no_client_auth();
    Arc::new(cfg)
}

/// 由 http(s) 基址构造 ws(s)://…/v1/ws?token=…
pub fn ws_url(base: &str, token: &str) -> Result<url::Url, ConnectError> {
    let mut u = url::Url::parse(base).map_err(|e| ConnectError::Other(e.to_string()))?;
    let scheme = if u.scheme() == "https" { "wss" } else { "ws" };
    u.set_scheme(scheme).map_err(|_| ConnectError::Other("无法设置 ws 协议".into()))?;
    let path = format!("{}/v1/ws", u.path().trim_end_matches('/'));
    u.set_path(&path);
    u.query_pairs_mut().clear().append_pair("token", token);
    Ok(u)
}

pub async fn connect(base: &str, token: &str) -> Result<WsStream, ConnectError> {
    connect_with_limit(base, token, MAX_FRAME_BYTES).await
}

/// 建连各步骤（TCP、TLS、WS 握手）各自的超时。
const STEP_TIMEOUT: Duration = Duration::from_secs(15);

/// 与 `connect` 相同，但可指定读帧上限（测试用来模拟超限帧）。
pub async fn connect_with_limit(base: &str, token: &str, max_frame: usize) -> Result<WsStream, ConnectError> {
    connect_with_timeout(base, token, max_frame, STEP_TIMEOUT).await
}

/// 建立连接；TCP、TLS、WS 握手每一步都有超时，任何一步卡住（强制门户、代理停滞、NAT 回收）都不会让重连循环永远挂起。
async fn connect_with_timeout(base: &str, token: &str, max_frame: usize, step: Duration) -> Result<WsStream, ConnectError> {
    let url = ws_url(base, token)?;
    let host = url.host_str().ok_or_else(|| ConnectError::Other("缺少主机名".into()))?.to_string();
    let tls = url.scheme() == "wss";
    let port = url.port_or_known_default().unwrap_or(if tls { 443 } else { 80 });

    let tcp = tokio::time::timeout(step, TcpStream::connect((host.as_str(), port)))
        .await
        .map_err(|_| ConnectError::Other("连接超时".into()))?
        .map_err(|e| ConnectError::Other(e.to_string()))?;
    let _ = tcp.set_nodelay(true);

    let stream: Box<dyn AsyncRw> = if tls {
        let connector = tokio_rustls::TlsConnector::from(tls_config());
        let name = rustls::pki_types::ServerName::try_from(host.clone()).map_err(|e| ConnectError::Other(e.to_string()))?;
        let s = tokio::time::timeout(step, connector.connect(name, tcp))
            .await
            .map_err(|_| ConnectError::Other("TLS 握手超时".into()))?
            .map_err(|e| ConnectError::Other(format!("TLS: {e}")))?;
        Box::new(s)
    } else {
        Box::new(tcp)
    };

    let mut req = url.as_str().into_client_request().map_err(|e| ConnectError::Other(e.to_string()))?;
    if let Ok(v) = HeaderValue::from_str(&format!("Bearer {token}")) {
        req.headers_mut().insert("Authorization", v);
    }
    match tokio::time::timeout(step, tokio_tungstenite::client_async_with_config(req, stream, Some(ws_config(max_frame)))).await {
        Err(_) => Err(ConnectError::Other("握手超时".into())),
        Ok(Ok((ws, _resp))) => Ok(ws),
        Ok(Err(tokio_tungstenite::tungstenite::Error::Http(resp))) if resp.status().as_u16() == 401 => {
            Err(ConnectError::Unauthorized)
        }
        Ok(Err(e)) => Err(ConnectError::Other(e.to_string())),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn url_build() {
        assert_eq!(ws_url("http://127.0.0.1:8080", "t").unwrap().as_str(), "ws://127.0.0.1:8080/v1/ws?token=t");
        assert_eq!(ws_url("https://h.example/api", "a b").unwrap().as_str(), "wss://h.example/api/v1/ws?token=a+b");
    }

    #[test]
    fn frame_limit_matches_server() {
        // 与 server/internal/ws/frame.go 的 MaxFrameBytes 保持一致
        assert_eq!(MAX_FRAME_BYTES, 1_114_112);
        let c = ws_config(MAX_FRAME_BYTES);
        assert_eq!(c.max_message_size, Some(MAX_FRAME_BYTES));
        assert_eq!(c.max_frame_size, Some(MAX_FRAME_BYTES));
    }

    #[test]
    fn tls_config_builds_with_ring() {
        let _ = tls_config();
    }

    /// 服务端接受 TCP 连接后一言不发：TLS 握手必须超时返回，而不是永远挂起。
    #[tokio::test]
    async fn stalled_tls_handshake_times_out() {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let hold = tokio::spawn(async move {
            let mut socks = Vec::new();
            while let Ok((s, _)) = listener.accept().await {
                socks.push(s); // 保持连接打开但不回应
            }
        });
        let r = tokio::time::timeout(
            Duration::from_secs(5),
            connect_with_timeout(&format!("https://127.0.0.1:{port}"), "t", MAX_FRAME_BYTES, Duration::from_millis(300)),
        )
        .await
        .expect("TLS 握手不能无限等待");
        match r {
            Err(ConnectError::Other(m)) => assert!(m.contains("TLS"), "{m}"),
            other => panic!("应为 TLS 超时: {:?}", other.map(|_| ())),
        }
        hold.abort();
    }
}
