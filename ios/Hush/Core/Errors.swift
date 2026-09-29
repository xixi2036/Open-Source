import Foundation

/// 把核心库错误转成给用户看的中文文案。
func describe(_ error: Error) -> String {
    if let e = error as? HushError {
        switch e {
        case .Network: return "网络连接失败，请检查服务器地址"
        case let .Server(code, message):
            switch code {
            case "not_friends": return "你们还不是好友"
            case "blocked": return "已被对方拉黑或你已拉黑对方"
            case "rate_limited": return "操作太频繁，请稍后再试"
            case "unauthorized": return "登录已失效，请重新登录"
            case "conflict": return message.isEmpty ? "操作冲突" : message
            case "too_large": return "文件太大"
            default: return message.isEmpty ? "服务器错误（\(code)）" : message
            }
        case let .Crypto(message): return "加密错误：\(message)"
        case let .Storage(message): return "存储错误：\(message)"
        case .NotLoggedIn: return "尚未登录"
        case let .InvalidInput(message): return message
        case let .NotFound(message): return "未找到\(message)"
        case let .Internal(message): return "内部错误：\(message)"
        case .KeyMismatch: return "本地数据库密钥不匹配"
        }
    }
    return error.localizedDescription
}
