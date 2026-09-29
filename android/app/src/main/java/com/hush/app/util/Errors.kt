package com.hush.app.util

import uniffi.hushcore.HushException

/** 把核心异常转成用户可读文案（debug 构建同时写日志便于排查）。 */
fun Throwable.userMessage(): String {
    HushLog.w("Hush", "error: ${this::class.simpleName}: $message", this)
    return describe()
}

private fun Throwable.describe(): String = when (this) {
    is HushException.Network -> "网络连接失败，请检查服务器地址"
    is HushException.Server -> message.ifBlank { code }
    is HushException.NotLoggedIn -> "请先登录"
    is HushException.InvalidInput -> "输入不合法：$message"
    is HushException.NotFound -> "未找到：$message"
    is HushException.Crypto -> "加密出错：$message"
    is HushException.Storage -> "存储出错：$message"
    else -> message ?: toString()
}

/**
 * 被动登出（核心 LoggedOut 事件）的提示文案（P-G）。
 * session_expired：令牌过期或任一接口 401；logged_in_elsewhere 等：被同账号的新登录顶下线。
 */
fun loggedOutMessage(reason: String): String = when {
    reason == "session_expired" -> "登录已过期，请重新登录"
    reason.contains("elsewhere") || reason.contains("replaced") || reason.contains("other") -> "你的账号已在其他设备登录"
    else -> "登录状态已失效，请重新登录"
}
