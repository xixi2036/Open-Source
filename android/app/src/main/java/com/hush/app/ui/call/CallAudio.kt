package com.hush.app.ui.call

import android.content.Context
import android.media.AudioAttributes
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.media.Ringtone
import android.media.RingtoneManager
import android.os.Build
import com.hush.app.ui.chat.VoicePlayer

/**
 * 通话音频：音频焦点、通话模式、听筒/扬声器/耳机路由、来电铃声。
 * 从 [CallManager] 拆出，所有方法都在主线程调用。
 */
class CallAudio(context: Context) {
    private val context: Context = context.applicationContext

    private val audio: AudioManager get() = context.getSystemService(Context.AUDIO_SERVICE) as AudioManager
    private var focus: AudioFocusRequest? = null
    private var ringtone: Ringtone? = null

    /** 是否接了有线 / 蓝牙 / USB 耳机（此时视频通话不强制外放）。 */
    fun headsetConnected(): Boolean = runCatching {
        audio.getDevices(AudioManager.GET_DEVICES_OUTPUTS).any { it.type in HEADSET_TYPES }
    }.getOrDefault(false)

    /**
     * 进入通话：停止语音消息播放、申请通话音频焦点（后台音乐等会暂停/压低）、切到通话模式并按需外放。
     * 返回实际是否外放。
     */
    fun start(video: Boolean): Boolean {
        VoicePlayer.stop()
        requestFocus()
        val speaker = CallLogic.defaultSpeaker(video, headsetConnected())
        runCatching { audio.mode = AudioManager.MODE_IN_COMMUNICATION }
        routeSpeaker(speaker)
        return speaker
    }

    /** 结束通话：恢复模式与路由，释放音频焦点。 */
    fun stop() {
        runCatching { audio.mode = AudioManager.MODE_NORMAL }
        routeSpeaker(false)
        focus?.let { f -> runCatching { audio.abandonAudioFocusRequest(f) } }
        focus = null
    }

    private fun requestFocus() {
        if (focus != null) return
        val attrs = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
            .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
            .build()
        val req = AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT)
            .setAudioAttributes(attrs)
            .setOnAudioFocusChangeListener { }
            .build()
        if (runCatching { audio.requestAudioFocus(req) }.getOrNull() != null) focus = req
    }

    fun routeSpeaker(on: Boolean) {
        runCatching {
            if (Build.VERSION.SDK_INT >= 31) {
                if (on) audio.availableCommunicationDevices.firstOrNull { it.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER }?.let { audio.setCommunicationDevice(it) }
                else audio.clearCommunicationDevice()
            } else {
                @Suppress("DEPRECATION")
                audio.isSpeakerphoneOn = on
            }
        }
    }

    fun startRinging() {
        stopRinging()
        runCatching {
            ringtone = RingtoneManager.getRingtone(context, RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE))?.also { it.play() }
        }
    }

    fun stopRinging() {
        runCatching { ringtone?.stop() }
        ringtone = null
    }

    private companion object {
        val HEADSET_TYPES = buildSet {
            add(AudioDeviceInfo.TYPE_WIRED_HEADSET)
            add(AudioDeviceInfo.TYPE_WIRED_HEADPHONES)
            add(AudioDeviceInfo.TYPE_BLUETOOTH_SCO)
            add(AudioDeviceInfo.TYPE_BLUETOOTH_A2DP)
            add(AudioDeviceInfo.TYPE_USB_HEADSET)
            if (Build.VERSION.SDK_INT >= 31) add(AudioDeviceInfo.TYPE_BLE_HEADSET)
        }
    }
}
