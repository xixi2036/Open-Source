package com.hush.app.util

import uniffi.hushcore.Mention

/**
 * 输入框中的一个 @提及片段：覆盖 `@昵称`（不含其后的空格）。
 * 偏移是 UTF-16 码元：Kotlin String 的下标本身就是 UTF-16 码元，与核心 [Mention] 口径一致。
 */
data class MentionSpan(val userId: String, val start: Int, val length: Int) {
    val end: Int get() = start + length
}

/** 一次编辑后的输入框状态。[atTrigger]：这次编辑刚输入了一个 `@`（在新文本中的下标），用于弹出成员选择。 */
data class MentionEdit(
    val text: String,
    val cursor: Int,
    val spans: List<MentionSpan>,
    val atTrigger: Int? = null,
)

/**
 * @提及输入逻辑（纯函数，JVM 单测见 MentionEditorTest）：
 * - 插入 `@昵称 ` 并记录区间；
 * - 其他位置的编辑让区间随之平移；在片段内部插入文字时该片段失效（保留文字，不再是提及）；
 * - 删除触及片段（包括紧随其后的空格）时整体删除 `@昵称 `。
 */
object MentionEditor {

    /**
     * 输入框从 [oldText] 变成 [newText]（[newCursor] 为变化后的光标位置，选区时取其起点）。
     * 返回处理后的文本、光标与区间；删除触及提及片段时文本会比 [newText] 删得更多。
     */
    fun onEdit(oldText: String, spans: List<MentionSpan>, newText: String, newCursor: Int): MentionEdit {
        if (oldText == newText) return MentionEdit(newText, newCursor, spans)
        val (p, q, ins) = diff(oldText, newText, newCursor)
        val r = ins.length
        if (r == 0) {
            // 纯删除：与任何片段（含尾随空格）相交则扩展为删除整个片段
            var from = p
            var to = q
            val hit = spans.filter { p < zoneEnd(oldText, it) && q > it.start }
            for (s in hit) { from = minOf(from, s.start); to = maxOf(to, zoneEnd(oldText, s)) }
            val text = oldText.removeRange(from, to)
            val removed = to - from
            val rest = spans.filter { it !in hit }.map { if (it.start >= to) it.copy(start = it.start - removed) else it }
            return MentionEdit(text, from, rest)
        }
        val delta = r - (q - p)
        val kept = spans.mapNotNull { s ->
            when {
                s.end <= p -> s
                s.start >= q -> s.copy(start = s.start + delta)
                else -> null // 编辑落在片段内部：该片段不再是提及
            }
        }
        val trigger = if (ins == "@") p else null
        return MentionEdit(newText, newCursor, kept, trigger)
    }

    /** 把 [text] 中 [at] 处的 `@` 替换为 `@昵称 `，并记录区间。 */
    fun insert(text: String, spans: List<MentionSpan>, at: Int, userId: String, name: String): MentionEdit {
        require(at in text.indices && text[at] == '@') { "no @ at $at" }
        val piece = "@$name "
        val newText = text.substring(0, at) + piece + text.substring(at + 1)
        val delta = piece.length - 1
        val shifted = spans.map { if (it.start > at) it.copy(start = it.start + delta) else it }
        val span = MentionSpan(userId, at, 1 + name.length)
        return MentionEdit(newText, at + piece.length, (shifted + span).sortedBy { it.start })
    }

    /**
     * 发送前处理：去掉首尾空白（首部空白会让所有区间前移），丢弃越界或已不是 `@` 开头的区间，
     * 转成核心的 [Mention]。
     */
    fun forSend(text: String, spans: List<MentionSpan>): Pair<String, List<Mention>> {
        val lead = text.length - text.trimStart().length
        val t = text.trim()
        val out = spans.asSequence()
            .map { it.copy(start = it.start - lead) }
            .filter { it.start >= 0 && it.length > 1 && it.end <= t.length && t[it.start] == '@' }
            .distinctBy { it.start to it.length }
            .sortedBy { it.start }
            .map { Mention(it.userId, it.start.toUInt(), it.length.toUInt()) }
            .toList()
        return t to out
    }

    /**
     * 恢复草稿时重建区间（草稿只保存文字）：每个 `@` 后若紧跟某位成员的昵称，取最长的匹配。
     * [members] 为 (user_id, 昵称)。
     */
    fun rehydrate(text: String, members: List<Pair<String, String>>): List<MentionSpan> {
        val named = members.filter { it.second.isNotEmpty() }.sortedByDescending { it.second.length }
        val out = ArrayList<MentionSpan>()
        var i = 0
        while (i < text.length) {
            if (text[i] == '@') {
                val m = named.firstOrNull { text.startsWith(it.second, i + 1) }
                if (m != null) {
                    out += MentionSpan(m.first, i, 1 + m.second.length)
                    i += 1 + m.second.length
                    continue
                }
            }
            i++
        }
        return out
    }

    /** 气泡中需要强调的区间（校验越界，按起点排序）。 */
    fun highlightRanges(text: String, mentions: List<Mention>): List<IntRange> =
        mentions.map { it.start.toInt() to (it.start + it.length).toInt() }
            .filter { (s, e) -> s >= 0 && e <= text.length && s < e }
            .sortedBy { it.first }
            .map { (s, e) -> s until e }

    /** 片段的“删除区”：`@昵称` 加上紧随其后的一个空格。 */
    private fun zoneEnd(text: String, s: MentionSpan): Int = if (s.end < text.length && text[s.end] == ' ') s.end + 1 else s.end

    /**
     * 求编辑区间：旧文本 [p, q) 被替换为 ins。优先按光标位置解释（连续相同字符时定位准确），
     * 否则退回公共前后缀。
     */
    private fun diff(old: String, new: String, cursor: Int): Triple<Int, Int, String> {
        val d = new.length - old.length
        if (d < 0) {
            val p = cursor
            if (p in 0..new.length && p - d <= old.length && old.removeRange(p, p - d) == new) return Triple(p, p - d, "")
        } else if (d > 0) {
            val p = cursor - d
            if (p in 0..old.length && cursor <= new.length && new.removeRange(p, cursor) == old) return Triple(p, p, new.substring(p, cursor))
        }
        var pre = 0
        val max = minOf(old.length, new.length)
        while (pre < max && old[pre] == new[pre]) pre++
        var suf = 0
        while (suf < max - pre && old[old.length - 1 - suf] == new[new.length - 1 - suf]) suf++
        return Triple(pre, old.length - suf, new.substring(pre, new.length - suf))
    }
}
