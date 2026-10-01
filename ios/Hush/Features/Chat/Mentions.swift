import Foundation
import SwiftUI

/// 输入框里的一个 @提及片段：区间覆盖「@昵称」（不含后面的空格），单位是 UTF-16 码元（与核心 Mention 一致）
struct MentionSpan: Equatable {
    var userId: String
    var start: Int
    var length: Int
    var end: Int { start + length }
}

/// @提及输入状态（纯逻辑，全部按 UTF-16 计算）。
/// 每次文本变化时与上一次的文本做前后缀比较，推出被替换的区间（只依赖文本，不依赖输入框回报的光标）：
/// - 删除碰到某个提及片段 → 整个片段一起删除；
/// - 在片段内部插入 / 替换 → 该片段失效，按普通文字保留；
/// - 片段之后的编辑不影响它，之前的编辑让它整体平移；
/// - 只新插入一个「@」时报告触发位置（群聊弹出成员选择）。
/// 同时推算编辑后的光标位置 `cursor`（UTF-16）：插入之后 / 删除处 / 插入的「@昵称 」之后。
/// 输入框被程序改写（插入提及、整段删除）时按它把光标放回原处，而不是跳到末尾。
struct MentionComposer: Equatable {
    private(set) var text: String = ""
    private(set) var spans: [MentionSpan] = []
    /// 最近一次编辑后的光标位置（UTF-16 偏移，0...text.utf16.count）
    private(set) var cursor: Int = 0

    static let triggers: Set<UInt16> = [0x40, 0xFF20]   // @ 与全角 ＠

    /// 外部直接设置文本（恢复草稿、发送后清空）：不带提及
    mutating func reset(_ text: String = "") {
        self.text = text
        spans = []
        cursor = text.utf16.count
    }

    /// 文本框内容变为 new 之后调用。返回应显示的文本（整体删除提及片段时与 new 不同）和新输入的 @ 的位置
    mutating func textChanged(to new: String) -> (text: String, trigger: Int?) {
        let old = Array(text.utf16)
        let nw = Array(new.utf16)
        if old == nw { return (text, nil) }
        var p = 0
        let maxP = min(old.count, nw.count)
        while p < maxP && old[p] == nw[p] { p += 1 }
        // 不把代理对从中间切开
        if p > 0 && UTF16.isLeadSurrogate(old[p - 1]) { p -= 1 }
        var s = 0
        let maxS = min(old.count, nw.count) - p
        while s < maxS && old[old.count - 1 - s] == nw[nw.count - 1 - s] { s += 1 }
        if s > 0 && s < old.count && UTF16.isTrailSurrogate(old[old.count - s]) { s -= 1 }
        var lower = p
        var upper = old.count - s            // 旧文本中被替换的区间 [lower, upper)
        let inserted = Array(nw[p..<(nw.count - s)])

        var result = nw
        var kept: [MentionSpan] = []
        if inserted.isEmpty {
            // 纯删除：碰到的提及片段整体删除
            let hit = spans.filter { $0.start < upper && $0.end > lower }
            if !hit.isEmpty {
                lower = min(lower, hit.map(\.start).min()!)
                upper = max(upper, hit.map(\.end).max()!)
                result = Array(old[0..<lower]) + Array(old[upper...])
            }
            let delta = -(upper - lower)
            for sp in spans where !hit.contains(sp) {
                if sp.end <= lower { kept.append(sp) } else if sp.start >= upper {
                    var m = sp; m.start += delta; kept.append(m)
                }
            }
        } else {
            let delta = inserted.count - (upper - lower)
            for sp in spans {
                if sp.end <= lower {
                    kept.append(sp)
                } else if sp.start >= upper {
                    var m = sp; m.start += delta; kept.append(m)
                }
                // 其余：编辑落在片段内部，片段失效（文字保留）
            }
        }
        spans = kept
        text = String(decoding: result, as: UTF16.self)
        // 纯删除（含整段删除提及）停在删除处；插入 / 替换停在新插入的内容之后
        cursor = lower + inserted.count
        let trigger = (upper == lower && inserted.count == 1 && Self.triggers.contains(inserted[0])) ? p : nil
        return (text, trigger)
    }

    /// 用「@昵称 」替换 [start, end) 区间（通常是刚输入的「@」加上已输入的筛选文字），记录提及区间
    mutating func insertMention(userId: String, name: String, replacing start: Int, _ end: Int) -> String {
        var units = Array(text.utf16)
        let lo = max(0, min(start, units.count))
        let hi = max(lo, min(end, units.count))
        let tag = Array("@\(name)".utf16)
        let insert = tag + Array(" ".utf16)
        let delta = insert.count - (hi - lo)
        var kept: [MentionSpan] = []
        for sp in spans {
            if sp.end <= lo { kept.append(sp) } else if sp.start >= hi {
                var m = sp; m.start += delta; kept.append(m)
            }
        }
        units.replaceSubrange(lo..<hi, with: insert)
        kept.append(MentionSpan(userId: userId, start: lo, length: tag.count))
        spans = kept.sorted { $0.start < $1.start }
        text = String(decoding: units, as: UTF16.self)
        cursor = lo + insert.count
        return text
    }

    /// 发送内容：去掉首尾空白，提及区间随之平移；没有提及时 mentions 为空
    func payload() -> (text: String, mentions: [Mention]) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return (trimmed, []) }
        let lead = String(text.unicodeScalars.prefix { CharacterSet.whitespacesAndNewlines.contains($0) }).utf16.count
        let count = trimmed.utf16.count
        let mentions = spans
            .filter { $0.start >= lead && $0.end <= lead + count && $0.length > 0 }
            .sorted { $0.start < $1.start }
            .map { Mention(userId: $0.userId, start: UInt32($0.start - lead), length: UInt32($0.length)) }
        return (trimmed, mentions)
    }

    /// 触发位置之后到光标处的筛选文字（在文字中间输入 @ 时，光标后面的原有文字不算筛选词）；
    /// 「@」已被删掉、光标移到了「@」之前、或筛选文字里有空白时返回 nil（关闭成员选择）
    func query(after trigger: Int) -> String? {
        let units = Array(text.utf16)
        guard trigger >= 0, trigger < units.count, Self.triggers.contains(units[trigger]),
              cursor > trigger, cursor <= units.count else { return nil }
        let rest = String(decoding: units[(trigger + 1)..<cursor], as: UTF16.self)
        if rest.contains(where: { $0.isWhitespace }) { return nil }
        return rest
    }
}

/// 气泡里的提及渲染：按 UTF-16 区间切分文本
enum MentionText {
    struct Segment: Equatable {
        var text: String
        var userId: String?
    }

    /// 非法区间（越界、重叠、切开代理对）按普通文字处理
    static func segments(_ text: String, _ mentions: [Mention]) -> [Segment] {
        let u = text.utf16
        let total = u.count
        var out: [Segment] = []
        var cursor = 0
        func slice(_ a: Int, _ b: Int) -> String? {
            let i = u.index(u.startIndex, offsetBy: a), j = u.index(u.startIndex, offsetBy: b)
            guard let si = i.samePosition(in: text.unicodeScalars), let sj = j.samePosition(in: text.unicodeScalars) else { return nil }
            return String(text.unicodeScalars[si..<sj])
        }
        for m in mentions.sorted(by: { $0.start < $1.start }) {
            let a = Int(m.start), b = Int(m.start) + Int(m.length)
            guard m.length > 0, a >= cursor, b <= total, let tag = slice(a, b) else { continue }
            if a > cursor, let before = slice(cursor, a) { out.append(Segment(text: before)) } else if a > cursor { continue }
            out.append(Segment(text: tag, userId: m.userId))
            cursor = b
        }
        if cursor < total, let rest = slice(cursor, total) { out.append(Segment(text: rest)) }
        if out.isEmpty && !text.isEmpty { out = [Segment(text: text)] }
        return out
    }

    static func attributed(_ text: String, _ mentions: [Mention], color: Color, underline: Bool) -> AttributedString {
        var result = AttributedString()
        for seg in segments(text, mentions) {
            var a = AttributedString(seg.text)
            if seg.userId != nil {
                // B12：用属性容器而不是 AttributedString 的动态成员写法（后者在 Swift 6 下因 key path 不是 Sendable 报错）
                var c = AttributeContainer()
                c[AttributeScopes.SwiftUIAttributes.ForegroundColorAttribute.self] = color
                c[AttributeScopes.SwiftUIAttributes.FontAttribute.self] = Fonts.body.weight(.semibold)
                if underline { c[AttributeScopes.SwiftUIAttributes.UnderlineStyleAttribute.self] = .single }
                a.mergeAttributes(c)
            }
            result += a
        }
        return result
    }

    static func mentions(_ m: MessageItem, user: String) -> Bool {
        !m.isOutgoing && !user.isEmpty && m.mentions.contains { $0.userId == user }
    }
}
