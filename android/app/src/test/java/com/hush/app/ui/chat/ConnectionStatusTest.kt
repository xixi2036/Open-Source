package com.hush.app.ui.chat

import com.hush.app.ui.chats.ConnectionStatus
import org.junit.Assert.assertEquals
import org.junit.Test

/** 聊天页 / 会话列表的连接状态提示条。 */
class ConnectionStatusTest {
    @Test
    fun status() {
        assertEquals(ConnectionStatus.OK, ConnectionStatus.of(online = true, connected = true, connecting = false))
        assertEquals(ConnectionStatus.CONNECTING, ConnectionStatus.of(online = true, connected = false, connecting = true))
        assertEquals(ConnectionStatus.DISCONNECTED, ConnectionStatus.of(online = true, connected = false, connecting = false))
        // 没有网络优先于核心的连接状态（核心可能还没感知到断开）
        assertEquals(ConnectionStatus.OFFLINE, ConnectionStatus.of(online = false, connected = true, connecting = false))
        assertEquals(null, ConnectionStatus.OK.text)
    }
}
