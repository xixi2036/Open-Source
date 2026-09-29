package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class TypingTrackerTest {
    private var now = 1_000L
    private val t = TypingTracker(ttlMs = 15_000, clock = { now })

    /** A10：typing=false 丢失时 15 秒后自动过期，不会一直显示“正在输入…”。 */
    @Test
    fun expiresWithoutStopEvent() {
        t.onTyping("g:1", "alice", true)
        assertEquals(setOf("g:1"), t.conversations())
        now += 14_999
        assertEquals(setOf("g:1"), t.conversations())
        now += 1
        assertEquals(emptySet<String>(), t.conversations())
        assertNull(t.millisUntilNextExpiry())
    }

    /** A10：群里两人同时输入，一人停止时会话仍显示正在输入。 */
    @Test
    fun perUserInGroup() {
        t.onTyping("g:1", "alice", true)
        t.onTyping("g:1", "bob", true)
        t.onTyping("g:1", "alice", false)
        assertEquals(setOf("g:1"), t.conversations())
        assertEquals(setOf("bob"), t.users("g:1"))
        t.clear("g:1", "bob")
        assertEquals(emptySet<String>(), t.conversations())
    }

    @Test
    fun refreshExtendsExpiryAndReportsNextDeadline() {
        t.onTyping("u:a", "a", true)
        now += 10_000
        t.onTyping("u:b", "b", true)
        assertEquals(5_000L, t.millisUntilNextExpiry())
        t.onTyping("u:a", "a", true)
        assertEquals(15_000L, t.millisUntilNextExpiry())
    }
}
