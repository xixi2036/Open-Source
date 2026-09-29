import Foundation

/// 时间与文案格式化（ui-guide §2.2 / §2.3 / §2.6）。
enum Fmt {
    static let cal = Calendar.current

    static func date(_ ms: Int64) -> Date { Date(timeIntervalSince1970: TimeInterval(ms) / 1000) }

    private static func f(_ pattern: String) -> DateFormatter {
        let d = DateFormatter()
        d.locale = Locale(identifier: "zh_CN")
        d.dateFormat = pattern
        return d
    }
    private static let hm = f("HH:mm")
    private static let weekday = f("EEEE")
    private static let ymd = f("yyyy/M/d")
    private static let md = f("M月d日")
    private static let ymdCN = f("yyyy年M月d日")

    /// 会话列表时间：今天 HH:mm，昨天“昨天”，一周内星期，其余 yyyy/M/d
    static func listTime(_ ms: Int64) -> String {
        guard ms > 0 else { return "" }
        let d = date(ms)
        if cal.isDateInToday(d) { return hm.string(from: d) }
        if cal.isDateInYesterday(d) { return "昨天" }
        if let days = cal.dateComponents([.day], from: cal.startOfDay(for: d), to: cal.startOfDay(for: Date())).day, days < 7 {
            return weekday.string(from: d)
        }
        return ymd.string(from: d)
    }

    static func bubbleTime(_ ms: Int64) -> String { hm.string(from: date(ms)) }

    /// 聊天日期分隔：今天 / 昨天 / 9月20日（跨年带年份）
    static func daySeparator(_ ms: Int64) -> String {
        let d = date(ms)
        if cal.isDateInToday(d) { return "今天" }
        if cal.isDateInYesterday(d) { return "昨天" }
        if cal.component(.year, from: d) == cal.component(.year, from: Date()) { return md.string(from: d) }
        return ymdCN.string(from: d)
    }

    /// 朋友圈时间：x 分钟前 / x 小时前 / 昨天 / M月d日
    static func momentTime(_ ms: Int64) -> String {
        let d = date(ms)
        let secs = Date().timeIntervalSince(d)
        if secs < 60 { return "刚刚" }
        if secs < 3600 { return "\(Int(secs / 60)) 分钟前" }
        if cal.isDateInToday(d) { return "\(Int(secs / 3600)) 小时前" }
        if cal.isDateInYesterday(d) { return "昨天" }
        return md.string(from: d)
    }

    static func duration(_ secs: Int) -> String {
        String(format: "%d:%02d", secs / 60, secs % 60)
    }

    static func fileSize(_ bytes: UInt64) -> String {
        ByteCountFormatter.string(fromByteCount: Int64(bytes), countStyle: .file)
    }

    /// 阅后即焚选项
    static let disappearingOptions: [(UInt32, String)] = [
        (0, "关闭"), (30, "30 秒"), (300, "5 分钟"), (3600, "1 小时"), (28800, "8 小时"), (86400, "1 天"), (604800, "1 周"),
    ]

    static func disappearingLabel(_ secs: UInt32) -> String {
        if let o = disappearingOptions.first(where: { $0.0 == secs }) { return o.1 }
        if secs % 86400 == 0 { return "\(secs / 86400) 天" }
        if secs % 3600 == 0 { return "\(secs / 3600) 小时" }
        if secs % 60 == 0 { return "\(secs / 60) 分钟" }
        return "\(secs) 秒"
    }
}
