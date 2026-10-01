package com.hush.app.core

/**
 * 连接保活的纯判定逻辑（JVM 单测见 ConnectionPolicyTest）。
 * [HushCore] 的看门狗与 HTTP 兜底轮询据此决定“现在要不要做”。
 */
object ConnectionPolicy {
    const val WATCHDOG_IDLE_MS = 45_000L
    const val POLL_FOREGROUND_MS = 5_000L
    const val POLL_UNSTABLE_MS = 3_000L
    /** 后台未连接时的兜底补拉间隔：比前台稀疏，兼顾耗电。 */
    const val POLL_BACKGROUND_MS = 30_000L

    /**
     * 看门狗（B2）：已登录、已启动、持续 Disconnected 超过 [WATCHDOG_IDLE_MS] 时重新 start()。
     * 不再要求应用在前台——后台被同账号连接挤下线两次后核心停止重连，只有平台层能把它拉起来；
     * 没有网络时不做无用功（网络恢复由 NetworkCallback 触发）。
     */
    fun shouldRestart(loggedIn: Boolean, started: Boolean, disconnected: Boolean, idleMs: Long, online: Boolean): Boolean =
        loggedIn && started && disconnected && online && idleMs > WATCHDOG_IDLE_MS

    /** HTTP 兜底补拉是否到期。 */
    fun pollDue(
        fastSync: Boolean,
        foreground: Boolean,
        connected: Boolean,
        unstable: Boolean,
        online: Boolean,
        sinceLastMs: Long,
    ): Boolean = when {
        !online -> false
        fastSync -> true
        foreground && !connected -> sinceLastMs >= POLL_FOREGROUND_MS
        foreground && unstable -> sinceLastMs >= POLL_UNSTABLE_MS
        !foreground && !connected -> sinceLastMs >= POLL_BACKGROUND_MS
        else -> false
    }
}
