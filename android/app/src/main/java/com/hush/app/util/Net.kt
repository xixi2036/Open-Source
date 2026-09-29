package com.hush.app.util

import kotlinx.coroutines.delay
import uniffi.hushcore.HushException

/**
 * 网络调用失败重试一次。
 * 模拟器 NAT 会悄悄断开空闲的 keep-alive 连接，核心库复用连接池里的旧连接时首个请求报
 * “error sending request”，立即重试即可成功（见 docs/issues/android.md）。只用于幂等的请求。
 */
suspend inline fun <T> net(block: () -> T): T = try {
    block()
} catch (e: HushException.Network) {
    delay(300)
    block()
}
