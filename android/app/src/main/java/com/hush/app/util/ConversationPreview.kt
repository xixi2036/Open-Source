package com.hush.app.util

/**
 * 会话列表第二行预览：可选的红色前缀（「[草稿]」「[有人@我]」）+ 正文。
 * [accent]：正文用强调色（“正在输入…”）。JVM 单测见 ConversationPreviewTest。
 */
data class PreviewParts(val redPrefix: String?, val body: String, val accent: Boolean = false)

object ConversationPreview {
    const val DRAFT = "[草稿]"
    const val MENTIONED = "[有人@我]"

    /**
     * 优先级：对方正在输入 > 草稿 > 未读提及 > 最后一条消息 > 已退群提示。
     * [lastPreview] 为最后一条消息的摘要（没有消息时为 null）。
     */
    fun of(typing: Boolean, draft: String?, unreadMentions: Int, lastPreview: String?, active: Boolean): PreviewParts {
        if (typing) return PreviewParts(null, "正在输入…", accent = true)
        val d = draft?.let(::singleLine)
        if (!d.isNullOrEmpty()) return PreviewParts(DRAFT, d)
        val body = lastPreview ?: if (!active) "你已不在此群聊中" else ""
        if (unreadMentions > 0) return PreviewParts(MENTIONED, body)
        return PreviewParts(null, body)
    }

    /** 列表中显示的时间：取最后一条消息的时间（有草稿时核心的 last_activity_ts 是草稿时间），没有消息时退回 [lastActivityTs]。 */
    fun displayTs(lastMessageSortTs: Long?, lastActivityTs: Long): Long = lastMessageSortTs ?: lastActivityTs

    /** 多行草稿在单行预览中用空格连接。 */
    fun singleLine(s: String): String = s.trim().replace(Regex("\\s*\\n\\s*"), " ")
}
