package com.hush.app.util

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** 合并高频刷新请求：执行期间到来的多次 trigger 只会再执行一次。 */
class Coalescer(scope: CoroutineScope, private val minGapMs: Long = 80, private val block: suspend () -> Unit) {
    private val ch = Channel<Unit>(Channel.CONFLATED)

    init {
        scope.launch {
            for (x in ch) {
                runCatching { block() }
                delay(minGapMs)
            }
        }
    }

    fun trigger() {
        ch.trySend(Unit)
    }
}
