package com.hush.app.ui.chat

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ChatPagingTest {
    /**
     * A01 复现：旧实现把第一次组合时 rows.size=0 闭包进阈值，条件变成 index >= -3，
     * 只要有任一可见项就恒为 true。正确判定必须随总项数变化。
     */
    @Test
    fun staleEmptyThresholdWouldAlwaysLoad() {
        val staleRowsSize = 0
        val oldNearTop = { lastVisible: Int -> lastVisible >= staleRowsSize - 3 }
        assertTrue("旧实现：第 10 项可见、共 41 项时也判定为到顶", oldNearTop(10))
        assertFalse(ChatPaging.nearTop(lastVisibleIndex = 10, totalItems = 41))
    }

    @Test
    fun nearTopOnlyWithinMargin() {
        assertFalse(ChatPaging.nearTop(null, 0))
        assertFalse(ChatPaging.nearTop(null, 41))
        assertFalse(ChatPaging.nearTop(37, 41))
        assertTrue(ChatPaging.nearTop(38, 41))
        assertTrue(ChatPaging.nearTop(40, 41))
    }

    @Test
    fun afterPrependingAPageNoLongerNearTop() {
        // reverseLayout：加载更早的一页后可见项下标不变，总数增加 → 不再触发，需用户继续上滑
        assertTrue(ChatPaging.nearTop(40, 41))
        assertFalse(ChatPaging.nearTop(40, 81))
    }

    @Test
    fun mergeLatestReplacesTailFromAnchor() {
        val current = listOf("a", "b", "c", "d")
        // 断线期间 d 被撤回删除、e/f 新到
        assertEquals(listOf("a", "b", "c", "e", "f"), ChatPaging.mergeLatest(current, listOf("c", "e", "f")) { it })
    }

    @Test
    fun mergeLatestWithGapReturnsNull() {
        assertNull(ChatPaging.mergeLatest(listOf("a", "b"), listOf("x", "y")) { it })
    }

    @Test
    fun mergeLatestEdgeCases() {
        assertEquals(listOf("a"), ChatPaging.mergeLatest(listOf("a"), emptyList<String>()) { it })
        assertEquals(listOf("x"), ChatPaging.mergeLatest(emptyList(), listOf("x")) { it })
    }
}
