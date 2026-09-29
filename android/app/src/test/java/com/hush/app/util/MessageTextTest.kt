package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Test
import uniffi.hushcore.CallEndReason

class MessageTextTest {
    /** P-B：通话记录按核心给出的结束原因显示。 */
    @Test
    fun callLogTextByReason() {
        assertEquals("语音通话 3:21", MessageText.callLogText(false, 201, CallEndReason.COMPLETED))
        assertEquals("视频通话 3:21", MessageText.callLogText(true, 201, CallEndReason.COMPLETED))
        assertEquals("未接来电", MessageText.callLogText(false, 0, CallEndReason.MISSED))
        assertEquals("已拒绝", MessageText.callLogText(true, 0, CallEndReason.REJECTED))
        assertEquals("对方忙线", MessageText.callLogText(false, 0, CallEndReason.BUSY))
        assertEquals("已取消", MessageText.callLogText(false, 0, CallEndReason.CANCELLED))
        assertEquals("通话失败", MessageText.callLogText(true, 0, CallEndReason.FAILED))
    }

    /** 会话列表预览没有图标：未接通的短语前补通话类型。 */
    @Test
    fun callLogPreviewInList() {
        assertEquals("[语音通话 已取消]", MessageText.callLogPreview(false, 0, CallEndReason.CANCELLED))
        assertEquals("[视频通话 对方忙线]", MessageText.callLogPreview(true, 0, CallEndReason.BUSY))
        assertEquals("[未接来电]", MessageText.callLogPreview(false, 0, CallEndReason.MISSED))
        assertEquals("[视频通话 1:05]", MessageText.callLogPreview(true, 65, CallEndReason.COMPLETED))
    }
}

class LoggedOutMessageTest {
    /** P-G：登录过期回到登录页时的提示。 */
    @Test
    fun reasons() {
        assertEquals("登录已过期，请重新登录", loggedOutMessage("session_expired"))
        assertEquals("你的账号已在其他设备登录", loggedOutMessage("logged_in_elsewhere"))
        assertEquals("登录状态已失效，请重新登录", loggedOutMessage("revoked"))
    }
}
