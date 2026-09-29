package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import uniffi.hushcore.Mention

class MentionEditorTest {
    private fun typed(old: String, spans: List<MentionSpan>, new: String, cursor: Int = new.length) =
        MentionEditor.onEdit(old, spans, new, cursor)

    @Test
    fun typingAtTriggersPicker() {
        val e = typed("你好", emptyList(), "你好@")
        assertEquals(2, e.atTrigger)
        // 在中间输入 @ 也触发，位置按光标计算
        val mid = MentionEditor.onEdit("ab", emptyList(), "a@b", 2)
        assertEquals(1, mid.atTrigger)
        // 粘贴一段含 @ 的文字不触发
        assertNull(typed("", emptyList(), "a@b").atTrigger)
    }

    @Test
    fun insertReplacesAtWithNameAndSpace() {
        val e = MentionEditor.insert("你好@", emptyList(), 2, "u1", "小美")
        assertEquals("你好@小美 ", e.text)
        assertEquals(listOf(MentionSpan("u1", 2, 3)), e.spans)
        assertEquals(e.text.length, e.cursor)
    }

    @Test
    fun insertBeforeExistingShiftsIt() {
        val first = MentionEditor.insert("@", emptyList(), 0, "u1", "小美") // "@小美 "
        val withAt = typed(first.text, first.spans, "@" + first.text, 1)
        assertEquals(0, withAt.atTrigger)
        assertEquals(listOf(MentionSpan("u1", 1, 3)), withAt.spans)
        val second = MentionEditor.insert(withAt.text, withAt.spans, 0, "u2", "助手")
        assertEquals("@助手 @小美 ", second.text)
        assertEquals(listOf(MentionSpan("u2", 0, 3), MentionSpan("u1", 4, 3)), second.spans)
    }

    @Test
    fun emojiNameUsesUtf16Length() {
        val e = MentionEditor.insert("hi @", emptyList(), 3, "u1", "😀小美")
        assertEquals(MentionSpan("u1", 3, 5), e.spans.single()) // "@" + 2 个代理对码元 + 2
        val (text, mentions) = MentionEditor.forSend(e.text, e.spans)
        assertEquals("hi @😀小美", text)
        assertEquals(listOf(Mention("u1", 3u, 5u)), mentions)
    }

    @Test
    fun backspaceInsideOrAfterMentionDeletesWholePiece() {
        val base = MentionEditor.insert("你好@", emptyList(), 2, "u1", "小美") // "你好@小美 "
        // 删掉尾随空格
        val a = typed(base.text, base.spans, "你好@小美", 5)
        assertEquals("你好", a.text); assertEquals(2, a.cursor); assertTrue(a.spans.isEmpty())
        // 删掉昵称最后一个字
        val b = typed(base.text, base.spans, "你好@小 ", 4)
        assertEquals("你好", b.text); assertTrue(b.spans.isEmpty())
        // 删掉 @ 本身
        val c = typed(base.text, base.spans, "你好小美 ", 2)
        assertEquals("你好", c.text)
    }

    @Test
    fun deletingElsewhereShiftsSpans() {
        val base = MentionEditor.insert("你好@", emptyList(), 2, "u1", "小美") // "你好@小美 "
        val e = typed(base.text, base.spans, "你@小美 ", 1)
        assertEquals("你@小美 ", e.text)
        assertEquals(listOf(MentionSpan("u1", 1, 3)), e.spans)
        // 在片段之后追加文字不影响
        val f = typed(e.text, e.spans, e.text + "吃饭")
        assertEquals(listOf(MentionSpan("u1", 1, 3)), f.spans)
    }

    @Test
    fun typingInsideMentionDropsSpanKeepsText() {
        val base = MentionEditor.insert("@", emptyList(), 0, "u1", "小美") // "@小美 "
        val e = typed(base.text, base.spans, "@小x美 ", 3)
        assertEquals("@小x美 ", e.text)
        assertTrue(e.spans.isEmpty())
    }

    @Test
    fun fullReplacementDropsAllSpans() {
        val base = MentionEditor.insert("@", emptyList(), 0, "u1", "小美")
        val e = typed(base.text, base.spans, "全新文字")
        assertTrue(e.spans.isEmpty())
    }

    @Test
    fun forSendTrimsAndShifts() {
        val spans = listOf(MentionSpan("u1", 2, 3))
        val (text, mentions) = MentionEditor.forSend("  @小美 你好  ", spans)
        assertEquals("@小美 你好", text)
        assertEquals(listOf(Mention("u1", 0u, 3u)), mentions)
        // 越界或不再以 @ 开头的区间被丢弃
        assertTrue(MentionEditor.forSend("abc", listOf(MentionSpan("u1", 1, 5))).second.isEmpty())
        assertTrue(MentionEditor.forSend("abcd", listOf(MentionSpan("u1", 1, 2))).second.isEmpty())
    }

    @Test
    fun rehydrateFromDraftPrefersLongestName() {
        val members = listOf("u1" to "小美", "u2" to "小美丽", "u3" to "助手")
        val spans = MentionEditor.rehydrate("@小美丽 你好 @助手 @陌生人", members)
        assertEquals(listOf(MentionSpan("u2", 0, 4), MentionSpan("u3", 8, 3)), spans)
    }

    @Test
    fun highlightRangesValidated() {
        val r = MentionEditor.highlightRanges("@小美 你好", listOf(Mention("u1", 0u, 3u), Mention("u2", 5u, 9u)))
        assertEquals(listOf(0 until 3), r)
    }
}
