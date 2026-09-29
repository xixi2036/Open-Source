package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Test

class ConversationPreviewTest {
    @Test
    fun draftWinsOverMentionAndLastMessage() {
        assertEquals(PreviewParts("[草稿]", "明天见"), ConversationPreview.of(false, "明天见", 2, "小美: 你好", true))
    }

    @Test
    fun multiLineDraftIsSingleLine() {
        assertEquals(PreviewParts("[草稿]", "第一行 第二行"), ConversationPreview.of(false, " 第一行\n\n第二行 ", 0, null, true))
    }

    @Test
    fun blankDraftIgnored() {
        assertEquals(PreviewParts(null, "你好"), ConversationPreview.of(false, "  ", 0, "你好", true))
    }

    @Test
    fun unreadMentionPrefix() {
        assertEquals(PreviewParts("[有人@我]", "小美: @我 收到@"), ConversationPreview.of(false, null, 1, "小美: @我 收到@", true))
    }

    @Test
    fun typingFirst() {
        assertEquals(PreviewParts(null, "正在输入…", accent = true), ConversationPreview.of(true, "草稿", 1, "x", true))
    }

    @Test
    fun inactiveGroupWithoutMessages() {
        assertEquals(PreviewParts(null, "你已不在此群聊中"), ConversationPreview.of(false, null, 0, null, false))
        assertEquals(PreviewParts(null, ""), ConversationPreview.of(false, null, 0, null, true))
    }

    @Test
    fun displayTsPrefersLastMessage() {
        assertEquals(100L, ConversationPreview.displayTs(100L, 999L))
        assertEquals(999L, ConversationPreview.displayTs(null, 999L))
    }
}
