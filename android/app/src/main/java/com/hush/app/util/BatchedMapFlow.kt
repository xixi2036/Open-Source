package com.hush.app.util

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch

/**
 * 以 StateFlow<Map> 形式对外暴露的缓存，写入按时间窗合并（C2）。
 *
 * 原来每写入一项就复制整张表并发射一次：打开一个 200 人的群要复制 200 次、触发 200 轮订阅方重组。
 * 现在写入先进入待合并区，[windowMs] 内的多次写入合并为一次复制、一次发射；
 * [get] 同时查待合并区，所以“刚写入就读取”仍然读得到。
 */
class BatchedMapFlow<K : Any, V : Any>(scope: CoroutineScope, private val windowMs: Long = 50) {
    private val _flow = MutableStateFlow<Map<K, V>>(emptyMap())
    val flow: StateFlow<Map<K, V>> = _flow
    private val lock = Any()
    private var pending = HashMap<K, V>()
    private val signal = Channel<Unit>(Channel.CONFLATED)

    init {
        scope.launch {
            for (x in signal) {
                delay(windowMs)
                flush()
            }
        }
    }

    fun put(key: K, value: V) {
        synchronized(lock) { pending[key] = value }
        signal.trySend(Unit)
    }

    /** 一批写入：直接合并并发射一次。 */
    fun putAll(entries: Map<K, V>) {
        if (entries.isEmpty()) return
        synchronized(lock) { pending.putAll(entries) }
        flush()
    }

    operator fun get(key: K): V? = synchronized(lock) { pending[key] } ?: _flow.value[key]

    fun containsKey(key: K): Boolean = get(key) != null

    /** 把待合并区并入对外的表（测试与 [putAll] 直接调用，平时由时间窗触发）。 */
    fun flush() {
        val batch = synchronized(lock) {
            if (pending.isEmpty()) return
            pending.also { pending = HashMap() }
        }
        // 值没有变化的写入不产生新表，也就不触发订阅方
        val cur = _flow.value
        if (batch.all { (k, v) -> cur[k] == v }) return
        synchronized(lock) { _flow.value = _flow.value + batch }
    }

    fun clear() {
        synchronized(lock) { pending = HashMap(); _flow.value = emptyMap() }
    }
}
