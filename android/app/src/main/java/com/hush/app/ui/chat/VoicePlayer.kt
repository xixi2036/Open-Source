package com.hush.app.ui.chat

import android.media.AudioAttributes
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.media.MediaPlayer
import com.hush.app.core.HushCore
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/**
 * 全局语音播放器：同一时刻只播放一条。
 * A5：以媒体用途播放并申请短暂音频焦点（其他 App 的音乐会暂停，播完恢复）；失去焦点（来电、其他 App 播放）
 * 或离开聊天页（[ChatViewModel.setVisible]）时停止。
 */
object VoicePlayer {
    data class Playing(val key: String, val progress: Float)

    private val _state = MutableStateFlow<Playing?>(null)
    val state: StateFlow<Playing?> = _state
    private var player: MediaPlayer? = null
    private var ticker: Job? = null
    private var focus: AudioFocusRequest? = null
    private val scope = CoroutineScope(Dispatchers.Main)

    private val attrs: AudioAttributes = AudioAttributes.Builder()
        .setUsage(AudioAttributes.USAGE_MEDIA)
        .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
        .build()

    private fun audioManager(): AudioManager? =
        runCatching { HushCore.appContext.getSystemService(AudioManager::class.java) }.getOrNull()

    private fun requestFocus(): Boolean {
        val am = audioManager() ?: return true
        val req = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT)
            .setAudioAttributes(attrs)
            .setOnAudioFocusChangeListener { change ->
                // 被其他音频打断（来电、别的 App 开始播放）：直接停止，不自动续播
                if (change < 0) scope.launch { stop() }
            }
            .build()
        val granted = runCatching { am.requestAudioFocus(req) == AudioManager.AUDIOFOCUS_REQUEST_GRANTED }.getOrDefault(true)
        if (granted) focus = req
        return granted
    }

    private fun abandonFocus() {
        val f = focus ?: return
        focus = null
        runCatching { audioManager()?.abandonAudioFocusRequest(f) }
    }

    fun toggle(key: String, path: String) {
        if (_state.value?.key == key) { stop(); return }
        stop()
        // 通话中等情况下系统拒绝焦点：不播放
        if (!requestFocus()) return
        val mp = MediaPlayer()
        try {
            mp.setAudioAttributes(attrs)
            mp.setDataSource(path)
            mp.setOnCompletionListener { stop() }
            mp.prepare()
            mp.start()
        } catch (e: Exception) {
            mp.release(); abandonFocus(); return
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
        abandonFocus()
        _state.value = null
    }
}
