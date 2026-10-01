package com.hush.app.util

import java.net.URI

/**
 * 服务器地址校验（B1，JVM 单测见 ServerUrlPolicyTest）。
 *
 * 核心库的 HTTP / WebSocket 由 Rust 直接发出，不经过 Android 的网络栈，manifest 里的
 * networkSecurityConfig 管不到它；所以“正式版不允许对公网使用明文”必须在这里、在登录之前强制执行，
 * networkSecurityConfig 只作为 Java 网络栈（WebView、图片加载等）的纵深防御。
 *
 * 规则：https 一律允许；http 只允许回环地址、私有网段与 .local 主机名（本地 / 局域网自建部署），其余必须 https。
 * 核心库自身也按同样的规则拒绝公网 http（返回 InvalidInput），这里先行校验以便给出明确提示。
 * debug 构建（[allowAnyCleartext] = true）保持原状，任何 http 地址都可以。
 */
object ServerUrlPolicy {
    sealed interface Result {
        /** 规范化后的地址（去掉末尾的 `/`）。 */
        data class Ok(val url: String) : Result
        data class Rejected(val reason: String) : Result
    }

    fun check(input: String, allowAnyCleartext: Boolean): Result {
        val url = input.trim().trimEnd('/')
        val uri = runCatching { URI(url) }.getOrNull()
        val scheme = uri?.scheme?.lowercase()
        if (uri == null || (scheme != "http" && scheme != "https")) return Result.Rejected("地址需以 http:// 或 https:// 开头")
        val host = hostOf(uri)
        if (host.isNullOrEmpty()) return Result.Rejected("地址缺少主机名")
        if (uri.userInfo != null) return Result.Rejected("地址中不能包含用户名或密码")
        if (scheme == "http" && !allowAnyCleartext && !isLocalHost(host)) {
            return Result.Rejected("公网服务器必须使用 https（明文 http 只允许本机与局域网地址）")
        }
        return Result.Ok(url)
    }

    private fun hostOf(uri: URI): String? {
        uri.host?.let { return it.removeSurrounding("[", "]").lowercase() }
        // java.net.URI 对带下划线等的主机名返回 null host：退回到 authority 里取
        val auth = uri.rawAuthority ?: return null
        val noUser = auth.substringAfterLast('@')
        return (if (noUser.startsWith("[")) noUser.substringAfter('[').substringBefore(']') else noUser.substringBefore(':')).lowercase()
    }

    /** 回环或私有网段：localhost、*.local、127/8、10/8、172.16/12、192.168/16、169.254/16、::1、fc00::/7、fe80::/10。 */
    fun isLocalHost(host: String): Boolean {
        val h = host.lowercase().removeSurrounding("[", "]")
        if (h == "localhost" || h.endsWith(".localhost")) return true
        // 局域网 mDNS 主机名（与核心库的规则一致：回环 / 私网 / .local 允许明文）
        if (h.endsWith(".local")) return true
        ipv4(h)?.let { (a, b) ->
            return a == 127 || a == 10 || (a == 172 && b in 16..31) || (a == 192 && b == 168) || (a == 169 && b == 254)
        }
        if (':' in h) {
            val addr = h.substringBefore('%')
            if (addr == "::1") return true
            val first = addr.substringBefore(':').toIntOrNull(16) ?: return false
            return (first and 0xfe00) == 0xfc00 || (first and 0xffc0) == 0xfe80
        }
        return false
    }

    /** 严格的点分十进制 IPv4（不做 DNS 解析，也不接受 `0x7f.1`、`127.1` 这类写法），返回前两段。 */
    private fun ipv4(h: String): Pair<Int, Int>? {
        val parts = h.split('.')
        if (parts.size != 4) return null
        val nums = parts.map { p -> if (p.isEmpty() || p.length > 3 || !p.all(Char::isDigit)) return null else p.toInt() }
        if (nums.any { it > 255 }) return null
        return nums[0] to nums[1]
    }
}
