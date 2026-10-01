import Foundation

/// C2：聊天页的日期分隔。逐条用 Calendar 比较「是否同一天」很慢（长会话每次刷新都要重算），
/// 这里记住当前这一天的起止时间，连续的同一天消息只做整数比较，跨天时才调用一次 Calendar。
enum DayBreaker {
    /// 每天第一条消息的 id（list 按 sortTs 升序）
    static func dayStartIds(_ list: [MessageItem], calendar: Calendar = Fmt.cal) -> Set<String> {
        var out = Set<String>()
        var lower: Int64 = 0, upper: Int64 = 0   // 当前这一天 [lower, upper) 的毫秒时间戳
        var hasDay = false
        for m in list {
            let ts = m.sortTs
            if hasDay, ts >= lower, ts < upper { continue }
            out.insert(m.id)
            if let day = calendar.dateInterval(of: .day, for: Fmt.date(ts)) {
                lower = Int64(day.start.timeIntervalSince1970 * 1000)
                upper = Int64(day.end.timeIntervalSince1970 * 1000)
                hasDay = true
            } else {
                hasDay = false
            }
        }
        return out
    }
}
