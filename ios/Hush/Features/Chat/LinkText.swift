import Foundation
import SwiftUI

/// 点击消息里的链接时怎么处理
enum LinkPolicy {
    enum Decision: Equatable {
        /// https：直接用系统浏览器打开
        case open
        /// http：明文连接，打开前先确认
        case confirm
        /// 其他协议（tel、自定义 scheme 等）：不打开
        case block
    }

    static func decision(_ url: URL) -> Decision {
        switch url.scheme?.lowercased() {
        case "https": return .open
        case "http": return .confirm
        default: return .block
        }
    }
}

/// 消息正文里的链接识别与富文本渲染（链接 + @提及）
enum LinkText {
    struct Link: Equatable {
        /// 在所属文字片段里的 UTF-16 区间
        var range: NSRange
        var url: URL
    }

    private static let detector = try? NSDataDetector(types: NSTextCheckingResult.CheckingType.link.rawValue)
    /// 识别结果缓存（NSDataDetector 较慢，气泡每次重绘都会用到）
    nonisolated(unsafe) private static let cache: NSCache<NSString, Box> = {
        let c = NSCache<NSString, Box>()
        c.countLimit = 500
        return c
    }()
    private final class Box { let links: [Link]; init(_ l: [Link]) { links = l } }

    /// 识别文字里的 http / https 链接（没写协议的域名按 http 处理，点击时会先确认）
    static func links(in text: String) -> [Link] {
        // 没有「.」或「:」的文字不可能含链接，直接跳过
        guard text.utf16.count >= 4, text.contains(".") || text.contains(":") else { return [] }
        if let hit = cache.object(forKey: text as NSString) { return hit.links }
        let ns = text as NSString
        var out: [Link] = []
        detector?.enumerateMatches(in: text, options: [], range: NSRange(location: 0, length: ns.length)) { r, _, _ in
            guard let r, let url = r.url, LinkPolicy.decision(url) != .block else { return }
            out.append(Link(range: r.range, url: url))
        }
        cache.setObject(Box(out), forKey: text as NSString)
        return out
    }

    /// 把一段普通文字按链接切开
    static func pieces(_ text: String) -> [(text: String, url: URL?)] {
        let found = links(in: text)
        guard !found.isEmpty else { return [(text, nil)] }
        let ns = text as NSString
        var out: [(String, URL?)] = []
        var cursor = 0
        for l in found where l.range.location >= cursor {
            if l.range.location > cursor {
                out.append((ns.substring(with: NSRange(location: cursor, length: l.range.location - cursor)), nil))
            }
            out.append((ns.substring(with: l.range), l.url))
            cursor = l.range.location + l.range.length
        }
        if cursor < ns.length { out.append((ns.substring(from: cursor), nil)) }
        return out
    }

    /// 气泡正文：@提及加粗着色，链接加下划线并可点击（由聊天页的 openURL 处理）
    static func attributed(_ text: String, _ mentions: [Mention], outgoing: Bool) -> AttributedString {
        let accent = outgoing ? Theme.onAccent : Theme.accent
        var result = AttributedString()
        for seg in MentionText.segments(text, mentions) {
            if seg.userId != nil {
                var a = AttributedString(seg.text)
                var c = AttributeContainer()
                c[AttributeScopes.SwiftUIAttributes.ForegroundColorAttribute.self] = accent
                c[AttributeScopes.SwiftUIAttributes.FontAttribute.self] = Fonts.body.weight(.semibold)
                if outgoing { c[AttributeScopes.SwiftUIAttributes.UnderlineStyleAttribute.self] = .single }
                a.mergeAttributes(c)
                result += a
                continue
            }
            for piece in pieces(seg.text) {
                var a = AttributedString(piece.text)
                if let url = piece.url {
                    var c = AttributeContainer()
                    c[AttributeScopes.FoundationAttributes.LinkAttribute.self] = url
                    c[AttributeScopes.SwiftUIAttributes.ForegroundColorAttribute.self] = accent
                    c[AttributeScopes.SwiftUIAttributes.UnderlineStyleAttribute.self] = .single
                    a.mergeAttributes(c)
                }
                result += a
            }
        }
        return result
    }
}
