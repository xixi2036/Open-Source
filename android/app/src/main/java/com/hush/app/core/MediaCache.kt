package com.hush.app.core

import com.hush.app.util.net

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import java.util.concurrent.ConcurrentHashMap

/** 头像与朋友圈媒体（明文）本地缓存：id → 本地路径。 */
object MediaCache {
    private val store = com.hush.app.util.BatchedMapFlow<String, String>(HushCore.scope)
    /** C2：一屏头像 / 朋友圈图片陆续下载完成时，按 50ms 时间窗合并后一次发射。 */
    val paths: StateFlow<Map<String, String>> = store.flow
    private val inFlight = ConcurrentHashMap.newKeySet<String>()
    private val failed = ConcurrentHashMap<String, Long>()

    /** 当前已缓存的本地路径（不触发下载）。 */
    fun pathOf(id: String): String? = store[id]

    fun requestAvatar(avatarId: String) = request(avatarId) { net { HushCore.client.fetchAvatar(avatarId) } }

    fun requestMedia(attachmentId: String) = request(attachmentId) { net { HushCore.client.fetchMedia(attachmentId) } }

    private fun request(id: String, fetch: suspend () -> String) {
        if (id.isEmpty() || store.containsKey(id)) return
        val lastFail = failed[id]
        if (lastFail != null && System.currentTimeMillis() - lastFail < 10_000) return
        if (!inFlight.add(id)) return
        HushCore.scope.launch(Dispatchers.IO) {
            try {
                val path = fetch()
                store.put(id, path)
            } catch (_: Exception) {
                failed[id] = System.currentTimeMillis()
            } finally {
                inFlight.remove(id)
            }
        }
    }

    fun clear() {
        store.clear()
        failed.clear()
    }
}
