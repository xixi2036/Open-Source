//! 统一错误类型，导出到 Kotlin/Swift。

/// hushcore 对外统一错误。
#[derive(Debug, Clone, thiserror::Error, uniffi::Error)]
pub enum HushError {
    /// 网络不可达、超时、连接中断等。
    #[error("网络错误: {message}")]
    Network { message: String },
    /// 服务端返回的错误体 `{"error":code,"message":...}`。
    #[error("服务端错误 {code}: {message}")]
    Server { code: String, message: String },
    /// 加解密、验签、会话建立失败。
    #[error("加密错误: {message}")]
    Crypto { message: String },
    /// 本地 SQLite / 文件读写失败。
    #[error("存储错误: {message}")]
    Storage { message: String },
    /// 需要登录后才能调用。
    #[error("未登录")]
    NotLoggedIn,
    /// 调用参数不合法。
    #[error("参数无效: {message}")]
    InvalidInput { message: String },
    /// 本地找不到对象（消息、会话、群等）。
    #[error("未找到: {message}")]
    NotFound { message: String },
    /// 其他内部错误。
    #[error("内部错误: {message}")]
    Internal { message: String },
    /// 构造 `HushClient` 时 db_key 与已有数据库不符（v2 / C-E）。字段解密失败等其他存储错误仍为 `Storage`。
    #[error("库密钥不匹配: {message}")]
    KeyMismatch { message: String },
}

pub type Result<T> = std::result::Result<T, HushError>;

impl HushError {
    pub fn crypto(m: impl std::fmt::Display) -> Self {
        HushError::Crypto { message: m.to_string() }
    }
    pub fn storage(m: impl std::fmt::Display) -> Self {
        HushError::Storage { message: m.to_string() }
    }
    pub fn invalid(m: impl std::fmt::Display) -> Self {
        HushError::InvalidInput { message: m.to_string() }
    }
    pub fn not_found(m: impl std::fmt::Display) -> Self {
        HushError::NotFound { message: m.to_string() }
    }
    pub fn internal(m: impl std::fmt::Display) -> Self {
        HushError::Internal { message: m.to_string() }
    }
    pub fn network(m: impl std::fmt::Display) -> Self {
        HushError::Network { message: m.to_string() }
    }
    /// 服务端错误码是否为指定值。
    pub fn is_server_code(&self, c: &str) -> bool {
        matches!(self, HushError::Server { code, .. } if code == c)
    }

    /// 是否为临时错误（稍后重试可能成功）：网络错误、未登录，以及服务端 5xx / 429 限流 / 401。
    /// 处理收到的信封或事件时，临时错误不 ack，等待重投；其余错误视为永久失败。
    /// 401 也归为临时：token 失效时连接会收到登出通知，不应先把数据确认丢弃。
    pub fn is_transient(&self) -> bool {
        match self {
            HushError::Network { .. } | HushError::NotLoggedIn => true,
            HushError::Server { code, .. } => {
                matches!(code.as_str(), "internal" | "rate_limited" | "unauthorized" | "http_429")
                    || code.starts_with("http_5")
            }
            _ => false,
        }
    }
}

impl From<rusqlite::Error> for HushError {
    fn from(e: rusqlite::Error) -> Self {
        HushError::storage(e)
    }
}

impl From<std::io::Error> for HushError {
    fn from(e: std::io::Error) -> Self {
        HushError::storage(e)
    }
}

impl From<serde_json::Error> for HushError {
    fn from(e: serde_json::Error) -> Self {
        HushError::internal(format!("JSON: {e}"))
    }
}

impl From<reqwest::Error> for HushError {
    fn from(e: reqwest::Error) -> Self {
        HushError::network(e)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn transient_classification() {
        let s = |c: &str| HushError::Server { code: c.into(), message: String::new() };
        for c in ["internal", "rate_limited", "unauthorized", "http_429", "http_502", "http_503", "http_504"] {
            assert!(s(c).is_transient(), "{c} 应为临时错误");
        }
        for c in ["not_found", "forbidden", "bad_request", "conflict", "not_friends", "blocked", "http_418"] {
            assert!(!s(c).is_transient(), "{c} 应为永久错误");
        }
        assert!(HushError::network("x").is_transient());
        assert!(HushError::NotLoggedIn.is_transient());
        assert!(!HushError::crypto("x").is_transient());
        assert!(!HushError::storage("x").is_transient());
    }
}
