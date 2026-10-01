package com.hush.app.core

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** B2：看门狗与 HTTP 兜底在后台也工作。 */
class ConnectionPolicyTest {
    @Test
    fun watchdogNoLongerNeedsForeground() {
        // 签名里已经没有“是否前台”这个条件：后台同样重启
        assertTrue(ConnectionPolicy.shouldRestart(loggedIn = true, started = true, disconnected = true, idleMs = 46_000, online = true))
        assertFalse(ConnectionPolicy.shouldRestart(true, true, disconnected = true, idleMs = 44_000, online = true))
        assertFalse(ConnectionPolicy.shouldRestart(true, true, disconnected = false, idleMs = 90_000, online = true))
        assertFalse("没有网络时不做无用功", ConnectionPolicy.shouldRestart(true, true, true, 90_000, online = false))
        assertFalse(ConnectionPolicy.shouldRestart(loggedIn = false, started = true, disconnected = true, idleMs = 90_000, online = true))
        assertFalse(ConnectionPolicy.shouldRestart(loggedIn = true, started = false, disconnected = true, idleMs = 90_000, online = true))
    }

    @Test
    fun fallbackPolling() {
        fun due(fast: Boolean = false, fg: Boolean, connected: Boolean, unstable: Boolean = false, online: Boolean = true, since: Long) =
            ConnectionPolicy.pollDue(fast, fg, connected, unstable, online, since)
        assertTrue("通话建立期间每轮都拉", due(fast = true, fg = false, connected = true, since = 0))
        assertTrue(due(fg = true, connected = false, since = 5_000))
        assertFalse(due(fg = true, connected = false, since = 4_000))
        assertTrue(due(fg = true, connected = true, unstable = true, since = 3_000))
        assertFalse(due(fg = true, connected = true, since = 60_000))
        assertTrue("后台未连接也兜底补拉", due(fg = false, connected = false, since = 30_000))
        assertFalse("后台间隔更长", due(fg = false, connected = false, since = 10_000))
        assertFalse(due(fg = false, connected = true, since = 60_000))
        assertFalse(due(fast = true, fg = true, connected = false, online = false, since = 60_000))
    }
}
