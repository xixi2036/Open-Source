package com.hush.app.ui.chat

import android.media.MediaPlayer
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/** 全局语音播放器：同一时刻只播放一条。 */
object VoicePlayer {
    data class Playing(val key: String, val progress: Float)

    private val _state = MutableStateFlow<Playing?>(null)
    val state: StateFlow<Playing?> = _state
    private var player: MediaPlayer? = null
    private var ticker: Job? = null
    private val scope = CoroutineScope(Dispatchers.Main)

    fun toggle(key: String, path: String) {
        if (_state.value?.key == key) { stop(); return }
        stop()
        val mp = MediaPlayer()
        try {
            mp.setDataSource(path)
            mp.setOnCompletionListener { stop() }
            mp.prepare()
            mp.start()
        } catch (e: Exception) {
            mp.release(); return
        }
        player = mp
        _state.value = Playing(key, 0f)
        ticker = scope.launch {
            while (isActive) {
                val p = player ?: break
                val d = p.duration.coerceAtLeast(1)
                _state.value = Playing(key, (runCatching { p.currentPosition }.getOrDefault(0).toFloat() / d).coerceIn(0f, 1f))
                delay(100)
            }
        }
    }

    fun stop() {
        ticker?.cancel(); ticker = null
        player?.let { runCatching { it.stop() }; it.release() }
        player = null
        _state.value = null
    }
}
