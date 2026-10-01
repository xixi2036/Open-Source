package com.hush.app.service

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class NotificationTextTest {
    /** 通知不含消息正文，只有数量与是否被 @。 */
    @Test
    fun body() {
        assertEquals("发来一条新消息", NotificationText.body(1, 0))
        assertEquals("3 条新消息", NotificationText.body(3, 0))
        assertEquals("有人@了你", NotificationText.body(1, 1))
        assertEquals("有人@了你 · 5 条新消息", NotificationText.body(5, 2))
    }

    /** 开启屏幕锁时通知不提供直接回复（否则不解锁就能以本人身份发消息）。 */
    @Test
    fun directReply() {
        assertTrue(NotificationText.allowDirectReply(screenLock = false, conversationActive = true))
        assertFalse(NotificationText.allowDirectReply(screenLock = true, conversationActive = true))
        assertFalse(NotificationText.allowDirectReply(screenLock = false, conversationActive = false))
    }
}
