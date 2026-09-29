package com.hush.app.util

import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.temporal.ChronoUnit

/** 时间文案（毫秒时间戳）。 */
object TimeFormat {
    private val zone: ZoneId get() = ZoneId.systemDefault()
    private val hm = DateTimeFormatter.ofPattern("HH:mm")
    private val weekNames = arrayOf("星期一", "星期二", "星期三", "星期四", "星期五", "星期六", "星期日")

    private fun date(ts: Long): LocalDate = Instant.ofEpochMilli(ts).atZone(zone).toLocalDate()

    fun hhmm(ts: Long): String = Instant.ofEpochMilli(ts).atZone(zone).format(hm)

    /** 会话列表：今天 HH:mm，昨天“昨天”，一周内星期，其余 yyyy/M/d。 */
    fun conversation(ts: Long): String {
        if (ts <= 0) return ""
        val d = date(ts)
        val today = LocalDate.now(zone)
        val days = ChronoUnit.DAYS.between(d, today)
        return when {
            days <= 0L -> hhmm(ts)
            days == 1L -> "昨天"
            days < 7L -> weekNames[d.dayOfWeek.value - 1]
            else -> "${d.year}/${d.monthValue}/${d.dayOfMonth}"
        }
    }

    /** 聊天页日期分隔条：今天 / 昨天 / 9月20日（跨年带年份）。 */
    fun daySeparator(ts: Long): String {
        val d = date(ts)
        val today = LocalDate.now(zone)
        val days = ChronoUnit.DAYS.between(d, today)
        return when {
            days <= 0L -> "今天"
            days == 1L -> "昨天"
            d.year == today.year -> "${d.monthValue}月${d.dayOfMonth}日"
            else -> "${d.year}年${d.monthValue}月${d.dayOfMonth}日"
        }
    }

    fun sameDay(a: Long, b: Long) = date(a) == date(b)

    /** 朋友圈：刚刚 / x 分钟前 / x 小时前 / 昨天 / M月d日。 */
    fun relative(ts: Long): String {
        val diff = System.currentTimeMillis() - ts
        val min = diff / 60_000
        val d = date(ts)
        val days = ChronoUnit.DAYS.between(d, LocalDate.now(zone))
        return when {
            min < 1 -> "刚刚"
            min < 60 -> "${min} 分钟前"
            days == 0L -> "${min / 60} 小时前"
            days == 1L -> "昨天"
            d.year == LocalDate.now(zone).year -> "${d.monthValue}月${d.dayOfMonth}日"
            else -> "${d.year}年${d.monthValue}月${d.dayOfMonth}日"
        }
    }

    /** 秒数 → m:ss。 */
    fun duration(secs: Long): String {
        val s = secs.coerceAtLeast(0)
        return if (s >= 3600) "%d:%02d:%02d".format(s / 3600, (s % 3600) / 60, s % 60) else "%d:%02d".format(s / 60, s % 60)
    }

    /** 阅后即焚选项文案。 */
    fun disappearing(secs: Int): String = when (secs) {
        0 -> "关闭"
        30 -> "30 秒"
        300 -> "5 分钟"
        3600 -> "1 小时"
        28800 -> "8 小时"
        86400 -> "1 天"
        604800 -> "1 周"
        else -> when {
            secs < 60 -> "$secs 秒"
            secs < 3600 -> "${secs / 60} 分钟"
            secs < 86400 -> "${secs / 3600} 小时"
            else -> "${secs / 86400} 天"
        }
    }

    val disappearingOptions = listOf(0, 30, 300, 3600, 28800, 86400, 604800)
}
