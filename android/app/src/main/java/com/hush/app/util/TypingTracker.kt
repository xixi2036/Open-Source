package com.hush.app.util

/**
 * “正在输入”状态：按 (会话, 用户) 记录，每条 [ttlMs] 后自动过期。
 * 对方掉线或 typing=false 丢失时不会一直显示；群里多人同时输入时一人停止不影响其他人。
 * 非线程安全，调用方在同一线程（主线程）使用。JVM 单测见 TypingTrackerTest。
 */
class TypingTracker(private val ttlMs: Long = DEFAULT_TTL_MS, private val clock: () -> Long = System::currentTimeMillis) {
    private val expiry = HashMap<String, HashMap<String, Long>>()

    fun onTyping(convId: String, userId: String, typing: Boolean) {
        if (typing) expiry.getOrPut(convId) { HashMap() }[userId] = clock() + ttlMs
        else clear(convId, userId)
    }

    /** 收到该用户在该会话的消息：视为停止输入。 */
    fun clear(convId: String, userId: String) {
        val m = expiry[convId] ?: return
        m.remove(userId)
        if (m.isEmpty()) expiry.remove(convId)
    }

    private fun prune(now: Long) {
        val it = expiry.values.iterator()
        while (it.hasNext()) {
            val m = it.next()
            m.values.removeAll { exp -> exp <= now }
            if (m.isEmpty()) it.remove()
        }
    }

    /** 当前有人在输入的会话。 */
    fun conversations(): Set<String> {
        prune(clock())
        return expiry.keys.toSet()
    }

    /** 某会话中正在输入的用户。 */
    fun users(convId: String): Set<String> {
        prune(clock())
        return expiry[convId]?.keys?.toSet() ?: emptySet()
    }

    /** 距最近一条过期还有多久（毫秒）；没有记录时为 null。调用方据此安排下一次刷新。 */
    fun millisUntilNextExpiry(): Long? {
        val now = clock()
        prune(now)
        return expiry.values.flatMap { it.values }.minOrNull()?.let { (it - now).coerceAtLeast(0) }
    }

    companion object {
        const val DEFAULT_TTL_MS = 15_000L
    }
}
