package com.hush.app.service

/** 消息通知的纯文本与判定逻辑（JVM 单测见 NotificationTextTest）。不含消息正文，保护隐私。 */
object NotificationText {
    fun body(unread: Int, unreadMentions: Int): String = when {
        unreadMentions > 0 && unread > 1 -> "有人@了你 · $unread 条新消息"
        unreadMentions > 0 -> "有人@了你"
        unread > 1 -> "$unread 条新消息"
        else -> "发来一条新消息"
    }

    /**
     * 是否在通知上提供“直接回复”：开启屏幕锁时不提供（否则不解锁应用就能以本人身份发消息）；
     * 已不在其中的群聊也不提供。
     */
    fun allowDirectReply(screenLock: Boolean, conversationActive: Boolean): Boolean = !screenLock && conversationActive
}
