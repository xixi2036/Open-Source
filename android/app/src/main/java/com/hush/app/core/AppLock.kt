package com.hush.app.core

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** 屏幕锁状态：冷启动或在后台超过 [GRACE_MS] 后回到前台时上锁。 */
object AppLock {
    private const val GRACE_MS = 60_000L
    private val _locked = MutableStateFlow(false)
    val locked: StateFlow<Boolean> = _locked
    private var backgroundAt = 0L

    fun init(enabled: Boolean) { _locked.value = enabled }

    fun onBackground() { backgroundAt = System.currentTimeMillis() }

    fun onForeground(enabled: Boolean) {
        if (enabled && backgroundAt > 0 && System.currentTimeMillis() - backgroundAt > GRACE_MS) _locked.value = true
    }

    fun unlock() { _locked.value = false }
}
