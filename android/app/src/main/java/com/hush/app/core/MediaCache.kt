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
    private val _paths = MutableStateFlow<Map<String, String>>(emptyMap())
    val paths: StateFlow<Map<String, String>> = _paths
    private val inFlight = ConcurrentHashMap.newKeySet<String>()
    private val failed = ConcurrentHashMap<String, Long>()

    /** 当前已缓存的本地路径（不触发下载）。 */
    fun pathOf(id: String): String? = _paths.value[id]

    fun requestAvatar(avatarId: String) = request(avatarId) { net { HushCore.client.fetchAvatar(avatarId) } }

    fun requestMedia(attachmentId: String) = request(attachmentId) { net { HushCore.client.fetchMedia(attachmentId) } }

    private fun request(id: String, fetch: suspend () -> String) {
        if (id.isEmpty() || _paths.value.containsKey(id)) return
        val lastFail = failed[id]
        if (lastFail != null && System.currentTimeMillis() - lastFail < 10_000) return
        if (!inFlight.add(id)) return
        HushCore.scope.launch(Dispatchers.IO) {
            try {
                val path = fetch()
                _paths.update { it + (id to path) }
            } catch (_: Exception) {
                failed[id] = System.currentTimeMillis()
            } finally {
                inFlight.remove(id)
            }
        }
    }

    fun clear() {
        _paths.value = emptyMap()
        failed.clear()
    }
}
