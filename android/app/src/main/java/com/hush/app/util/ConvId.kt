package com.hush.app.util

/** 会话 id 约定：单聊 "u:<对方 user_id>"，群聊 "g:<group_id>"（与核心一致）。JVM 单测见 ConvIdTest。 */
object ConvId {
    private const val DIRECT = "u:"
    private const val GROUP = "g:"

    fun direct(userId: String) = DIRECT + userId
    fun group(groupId: String) = GROUP + groupId

    /** 单聊的对方 id；不是单聊时为 null。 */
    fun peerOf(convId: String): String? = convId.takeIf { it.startsWith(DIRECT) }?.removePrefix(DIRECT)

    /** 群聊的群 id；不是群聊时为 null。 */
    fun groupOf(convId: String): String? = convId.takeIf { it.startsWith(GROUP) }?.removePrefix(GROUP)
}
