package com.hush.app.ui.chat

/**
 * 聊天页的纯逻辑（不依赖 Android，JVM 单测见 ChatLogicTest）。
 */

/** B11：消息列表按 sort_ts 升序维护。 */
object MessageOrder {
    /**
     * 插入或替换一条消息并保持按 [sortTs] 升序。
     * 新消息通常就是最新的一条（直接追加）；但重连补拉的积压、两端几乎同时发出的消息、
     * 以及发出的消息被服务端接受后 sort_ts 从本地时间改为服务端时间（K3）时，位置可能不在末尾。
     */
    fun <T> upsert(list: List<T>, item: T, id: (T) -> String, sortTs: (T) -> Long): List<T> {
        val key = id(item)
        val ts = sortTs(item)
        val idx = list.indexOfFirst { id(it) == key }
        if (idx >= 0) {
            val prevOk = idx == 0 || sortTs(list[idx - 1]) <= ts
            val nextOk = idx == list.lastIndex || sortTs(list[idx + 1]) >= ts
            // 位置没变：原地替换（绝大多数的状态更新、回应、回执）
            if (prevOk && nextOk) return list.toMutableList().also { it[idx] = item }
            return insert(list.filterIndexed { i, _ -> i != idx }, item, ts, sortTs)
        }
        if (list.isEmpty() || sortTs(list.last()) <= ts) return list + item
        return insert(list, item, ts, sortTs)
    }

    private fun <T> insert(list: List<T>, item: T, ts: Long, sortTs: (T) -> Long): List<T> {
        // 第一个 sort_ts 大于它的位置之前（相同时间戳的排在已有消息之后）
        var lo = 0
        var hi = list.size
        while (lo < hi) {
            val mid = (lo + hi) ushr 1
            if (sortTs(list[mid]) <= ts) lo = mid + 1 else hi = mid
        }
        return ArrayList<T>(list.size + 1).apply { addAll(list.subList(0, lo)); add(item); addAll(list.subList(lo, list.size)) }
    }
}

/** B10：附件自动下载策略。 */
object AutoDownload {
    const val MAX_AUTO_BYTES = 20_000_000L
    /** 计量网络（蜂窝、计量 Wi-Fi）下自动下载图片的上限；更大的图片显示缩略图，点按后再下载。 */
    const val MAX_METERED_IMAGE_BYTES = 1_000_000L

    fun shouldAuto(isImage: Boolean, isVoice: Boolean, size: Long, metered: Boolean): Boolean = when {
        size >= MAX_AUTO_BYTES -> false
        // 语音很小，而且不下载就没法播放
        isVoice -> true
        isImage -> !metered || size <= MAX_METERED_IMAGE_BYTES
        else -> false
    }
}

/** 附件下载状态（只记录进行中与失败；成功后以消息里的 local_path 为准）。 */
enum class DownloadState { LOADING, FAILED }

/** C7：语音录制限制。 */
object RecordLimit {
    const val MAX_MS = 60_000L
    const val MIN_MS = 800L
    /** 最后 10 秒开始倒计时提示。 */
    const val WARN_LAST_MS = 10_000L

    fun reachedMax(elapsedMs: Long): Boolean = elapsedMs >= MAX_MS
    /** 剩余秒数提示；不在最后 10 秒内时为 null。 */
    fun remainingHint(elapsedMs: Long): Int? {
        val left = MAX_MS - elapsedMs
        return if (left in 0..WARN_LAST_MS) ((left + 999) / 1000).toInt() else null
    }
}

/** 视频附件限制。 */
object VideoLimit {
    const val MAX_BYTES = 100L * 1024 * 1024
    fun tooLarge(size: Long): Boolean = size > MAX_BYTES
}

/** 消息文字中的链接。 */
object Links {
    data class Link(val start: Int, val end: Int, val url: String)

    // 只识别显式写出 http:// 或 https:// 的地址，避免把“a.b”之类的普通文字误判为链接
    private val regex = Regex("""https?://[^\s<>"'“”‘’，。；、！？（）【】《》]+""", RegexOption.IGNORE_CASE)
    private const val TRAILING = ".,;:!?)]}'\""

    fun find(text: String): List<Link> {
        if (!text.contains("http", ignoreCase = true)) return emptyList()
        return regex.findAll(text).mapNotNull { m ->
            var end = m.range.last + 1
            // 句末标点不属于链接；右括号只有在链接内没有配对的左括号时才去掉
            while (end > m.range.first) {
                val c = text[end - 1]
                if (c !in TRAILING) break
                if (c == ')' && text.substring(m.range.first, end).count { it == '(' } >= text.substring(m.range.first, end).count { it == ')' }) break
                end--
            }
            val url = text.substring(m.range.first, end)
            // 至少要有主机名
            if (url.substringAfter("://").isEmpty()) null else Link(m.range.first, end, url)
        }.toList()
    }

    /** 非 https 的链接（明文传输，可能被窃听或篡改）打开前需要用户确认。 */
    fun needsConfirm(url: String): Boolean = !url.startsWith("https://", ignoreCase = true)
}

/** C6：图片查看器的缩放与平移边界。 */
object ZoomMath {
    const val MIN_SCALE = 1f
    const val MAX_SCALE = 5f

    fun clampScale(scale: Float): Float = scale.coerceIn(MIN_SCALE, MAX_SCALE)

    /**
     * 平移上限：内容（按 Fit 显示在容器里的图片，尺寸 [contentW]×[contentH]）放大 [scale] 倍后，
     * 超出容器的部分的一半；没超出的方向不允许平移。图片边缘因此不会被拖离屏幕边缘。
     */
    fun maxOffset(scale: Float, containerW: Float, containerH: Float, contentW: Float, contentH: Float): Pair<Float, Float> {
        val mx = ((contentW * scale - containerW) / 2f).coerceAtLeast(0f)
        val my = ((contentH * scale - containerH) / 2f).coerceAtLeast(0f)
        return mx to my
    }

    fun clampOffset(x: Float, y: Float, max: Pair<Float, Float>): Pair<Float, Float> =
        x.coerceIn(-max.first, max.first) to y.coerceIn(-max.second, max.second)

    /** 图片按 Fit 放进容器后的显示尺寸；尺寸未知时按铺满容器处理。 */
    fun fitSize(containerW: Float, containerH: Float, imageW: Float, imageH: Float): Pair<Float, Float> {
        if (imageW <= 0f || imageH <= 0f || containerW <= 0f || containerH <= 0f) return containerW to containerH
        val s = minOf(containerW / imageW, containerH / imageH)
        return imageW * s to imageH * s
    }
}

/** 多选：哪些消息可以被选中 / 转发。 */
object Selection {
    /** 系统提示与已撤回的占位不可选。 */
    fun selectable(m: uniffi.hushcore.MessageItem): Boolean = !m.deleted && m.body !is uniffi.hushcore.MessageBody.System

    /** 文字与已上传的附件可以转发（发送中 / 失败的附件还没有服务端 id；通话记录、无法解密的消息不可转发）。 */
    fun forwardable(m: uniffi.hushcore.MessageItem): Boolean =
        !m.deleted && (m.body is uniffi.hushcore.MessageBody.Text || m.body is uniffi.hushcore.MessageBody.Attachments) &&
            m.status != uniffi.hushcore.MessageStatus.SENDING && m.status != uniffi.hushcore.MessageStatus.FAILED
}
