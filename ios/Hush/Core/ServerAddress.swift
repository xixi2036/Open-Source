import Foundation

/// 登录时填写的服务器地址：默认值与安全性检查（B11）
enum ServerAddress {
    /// 调试构建预填本机地址方便联调；Release 留空，由用户填写自己的服务器
    static var defaultURL: String {
        #if DEBUG
        return "http://127.0.0.1:8080"
        #else
        return ""
        #endif
    }

    static var placeholder: String {
        #if DEBUG
        return "http://127.0.0.1:8080"
        #else
        return "https://hush.example.com"
        #endif
    }

    enum Check: Equatable {
        /// 不是合法的 http(s) 地址
        case invalid
        /// https，或指向本机 / 私网的 http
        case ok
        /// 指向公网主机的明文 http：验证码、令牌、元数据都会明文经过网络，必须先向用户确认
        case insecure(host: String)
    }

    static func check(_ raw: String) -> Check {
        let s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let comps = URLComponents(string: s), let scheme = comps.scheme?.lowercased(),
              let host = comps.host, !host.isEmpty else { return .invalid }
        switch scheme {
        case "https": return .ok
        case "http":
            let bare = host.hasPrefix("[") && host.hasSuffix("]") ? String(host.dropFirst().dropLast()) : host
            return isLocal(bare) ? .ok : .insecure(host: bare)
        default: return .invalid
        }
    }

    /// 本机或私网地址：回环、RFC1918、链路本地、CGNAT、.local / .localhost / 无点主机名、IPv6 回环 / ULA / 链路本地
    static func isLocal(_ rawHost: String) -> Bool {
        var host = rawHost.lowercased()
        if host.hasPrefix("[") && host.hasSuffix("]") { host = String(host.dropFirst().dropLast()) }
        if host == "localhost" || host.hasSuffix(".localhost") || host.hasSuffix(".local") { return true }
        let parts = host.split(separator: ".", omittingEmptySubsequences: false)
        if parts.count == 4, parts.allSatisfy({ !$0.isEmpty && $0.allSatisfy(\.isNumber) }) {
            let o = parts.compactMap { Int($0) }
            guard o.count == 4, o.allSatisfy({ $0 >= 0 && $0 <= 255 }) else { return false }
            switch (o[0], o[1]) {
            case (127, _), (10, _), (192, 168), (169, 254): return true
            case (172, 16...31): return true
            case (100, 64...127): return true
            default: return false
            }
        }
        if host.contains(":") {
            if host == "::1" { return true }
            let first = host.split(separator: ":").first.map(String.init) ?? ""
            if let v = UInt16(first, radix: 16) {
                if v & 0xfe00 == 0xfc00 { return true }   // fc00::/7
                if v & 0xffc0 == 0xfe80 { return true }   // fe80::/10
            }
            return false
        }
        // 没有点的主机名（局域网内的机器名）
        return !host.contains(".") && !host.isEmpty && !host.allSatisfy(\.isNumber)
    }
}
