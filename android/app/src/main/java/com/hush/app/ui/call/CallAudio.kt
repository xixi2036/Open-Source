package com.hush.app.ui.call

import android.content.Context
import android.media.AudioAttributes
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioFocusRequest
import android.media.AudioManager
import android.media.Ringtone
import android.media.RingtoneManager
import android.media.ToneGenerator
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.os.VibrationEffect
import android.os.Vibrator
import android.os.VibratorManager
import com.hush.app.ui.chat.VoicePlayer

/**
 * 通话音频：音频焦点、通话模式、听筒/扬声器/蓝牙/有线耳机路由、来电铃声与振动、去电回铃音、距离感应熄屏。
 * 从 [CallManager] 拆出，所有方法都在主线程调用。
 */
class CallAudio(context: Context) {
    private val context: Context = context.applicationContext

    private val audio: AudioManager get() = context.getSystemService(Context.AUDIO_SERVICE) as AudioManager
    private val handler = Handler(Looper.getMainLooper())
    private var focus: AudioFocusRequest? = null
    private var ringtone: Ringtone? = null
    private var ringLoop: Runnable? = null
    private var vibrating = false
    private var tone: ToneGenerator? = null
    private var proximity: PowerManager.WakeLock? = null
    private var deviceCallback: AudioDeviceCallback? = null

    // ---------------- 路由 ----------------

    /** 当前可选的输出设备，按固定顺序（听筒、扬声器、蓝牙、有线）。 */
    fun availableRoutes(): List<AudioRoute> = runCatching {
        val types: List<Int> =
            if (Build.VERSION.SDK_INT >= 31) audio.availableCommunicationDevices.map { it.type }
            else audio.getDevices(AudioManager.GET_DEVICES_OUTPUTS).map { it.type }
        AudioRoute.entries.filter { r -> r == AudioRoute.SPEAKER || types.any { routeOf(it) == r } }
    }.getOrDefault(listOf(AudioRoute.SPEAKER))

    /** 切换输出设备；设备不存在或系统拒绝时返回 false。 */
    fun setRoute(route: AudioRoute): Boolean = runCatching {
        if (Build.VERSION.SDK_INT >= 31) {
            val dev = audio.availableCommunicationDevices.firstOrNull { routeOf(it.type) == route }
            when {
                dev != null -> audio.setCommunicationDevice(dev)
                // 没有听筒的设备（部分平板）：清除指定，交给系统默认路由
                route == AudioRoute.EARPIECE -> { audio.clearCommunicationDevice(); true }
                else -> false
            }
        } else {
            @Suppress("DEPRECATION")
            when (route) {
                AudioRoute.SPEAKER -> { stopSco(); audio.isSpeakerphoneOn = true }
                AudioRoute.BLUETOOTH -> { audio.isSpeakerphoneOn = false; audio.startBluetoothSco(); audio.isBluetoothScoOn = true }
                AudioRoute.EARPIECE, AudioRoute.WIRED -> { stopSco(); audio.isSpeakerphoneOn = false }
            }
            true
        }
    }.getOrDefault(false)

    @Suppress("DEPRECATION")
    private fun stopSco() {
        if (audio.isBluetoothScoOn) { audio.isBluetoothScoOn = false; audio.stopBluetoothSco() }
    }

    /** 耳机插拔、蓝牙连接/断开时回调（通话期间注册）。 */
    fun watchDevices(onChange: () -> Unit) {
        unwatchDevices()
        val cb = object : AudioDeviceCallback() {
            override fun onAudioDevicesAdded(added: Array<out AudioDeviceInfo>?) = onChange()
            override fun onAudioDevicesRemoved(removed: Array<out AudioDeviceInfo>?) = onChange()
        }
        runCatching { audio.registerAudioDeviceCallback(cb, handler) }.onSuccess { deviceCallback = cb }
    }

    private fun unwatchDevices() {
        deviceCallback?.let { runCatching { audio.unregisterAudioDeviceCallback(it) } }
        deviceCallback = null
    }

    /**
     * 进入通话：停止语音消息播放、申请通话音频焦点（后台音乐等会暂停/压低）、切到通话模式并选择默认输出设备。
     * 返回实际使用的输出设备。
     */
    fun start(video: Boolean): AudioRoute {
        VoicePlayer.stop()
        requestFocus()
        runCatching { audio.mode = AudioManager.MODE_IN_COMMUNICATION }
        val route = CallLogic.defaultRoute(video, availableRoutes())
        setRoute(route)
        return route
    }

    /** 结束通话：恢复模式与路由，释放音频焦点、回铃音与距离感应。 */
    fun stop() {
        stopRingback()
        setProximity(false)
        unwatchDevices()
        runCatching {
            if (Build.VERSION.SDK_INT >= 31) audio.clearCommunicationDevice()
            else { stopSco(); @Suppress("DEPRECATION") run { audio.isSpeakerphoneOn = false } }
        }
        runCatching { audio.mode = AudioManager.MODE_NORMAL }
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

    // ---------------- 来电铃声 / 振动（B6） ----------------

    /**
     * 来电提醒，遵循系统情景模式：响铃模式 = 循环铃声 + 振动；振动模式 = 只振动；静音模式 = 都不做。
     */
    fun startRinging() {
        stopRinging()
        val mode = runCatching { audio.ringerMode }.getOrDefault(AudioManager.RINGER_MODE_NORMAL)
        if (CallLogic.ringSound(mode)) runCatching {
            val r = RingtoneManager.getRingtone(context, RingtoneManager.getDefaultUri(RingtoneManager.TYPE_RINGTONE)) ?: return@runCatching
            r.audioAttributes = RING_ATTRS
            if (Build.VERSION.SDK_INT >= 28) r.isLooping = true
            r.play()
            ringtone = r
            if (Build.VERSION.SDK_INT < 28) {
                // Android 8.x 没有 isLooping：播完后重新播放
                val loop = object : Runnable {
                    override fun run() {
                        val cur = ringtone ?: return
                        if (!cur.isPlaying) runCatching { cur.play() }
                        handler.postDelayed(this, 1000)
                    }
                }
                ringLoop = loop
                handler.postDelayed(loop, 1000)
            }
        }
        if (CallLogic.ringVibrate(mode)) runCatching {
            val v = vibrator() ?: return@runCatching
            if (!v.hasVibrator()) return@runCatching
            // 带铃声用途的属性：App 在后台时也允许振动
            @Suppress("DEPRECATION")
            v.vibrate(VibrationEffect.createWaveform(RING_PATTERN, 0), RING_ATTRS)
            vibrating = true
        }
    }

    fun stopRinging() {
        ringLoop?.let { handler.removeCallbacks(it) }
        ringLoop = null
        runCatching { ringtone?.stop() }
        ringtone = null
        if (vibrating) { runCatching { vibrator()?.cancel() }; vibrating = false }
    }

    private fun vibrator(): Vibrator? =
        if (Build.VERSION.SDK_INT >= 31) context.getSystemService(VibratorManager::class.java)?.defaultVibrator
        else @Suppress("DEPRECATION") (context.getSystemService(Context.VIBRATOR_SERVICE) as? Vibrator)

    // ---------------- 去电回铃音 / 忙音（B6） ----------------

    /** 去电等待接听时的回铃音（走通话音量，从当前通话输出设备播放）。 */
    fun startRingback() {
        stopRingback()
        runCatching {
            tone = ToneGenerator(AudioManager.STREAM_VOICE_CALL, 70).also { it.startTone(ToneGenerator.TONE_SUP_RINGTONE) }
        }
    }

    fun stopRingback() {
        tone?.let { t -> runCatching { t.stopTone() }; runCatching { t.release() } }
        tone = null
    }

    /** 对方忙线 / 拒接时的短促忙音。 */
    fun playBusyTone() {
        runCatching {
            val t = ToneGenerator(AudioManager.STREAM_VOICE_CALL, 70)
            t.startTone(ToneGenerator.TONE_SUP_BUSY, 1200)
            handler.postDelayed({ runCatching { t.release() } }, 1500)
        }
    }

    // ---------------- 距离感应（B7） ----------------

    /** 语音通话用听筒时贴近耳朵熄屏并屏蔽触摸；设备不支持时为空操作。 */
    fun setProximity(on: Boolean) {
        if (on) {
            if (proximity?.isHeld == true) return
            runCatching {
                val pm = context.getSystemService(PowerManager::class.java) ?: return@runCatching
                if (!pm.isWakeLockLevelSupported(PowerManager.PROXIMITY_SCREEN_OFF_WAKE_LOCK)) return@runCatching
                val wl = proximity ?: pm.newWakeLock(PowerManager.PROXIMITY_SCREEN_OFF_WAKE_LOCK, "hush:call-proximity").also {
                    it.setReferenceCounted(false); proximity = it
                }
                // 兜底上限：通话跟踪最长 2 小时
                wl.acquire(2 * 60 * 60 * 1000L)
            }
        } else {
            runCatching { if (proximity?.isHeld == true) proximity?.release() }
        }
    }

    private companion object {
        val RING_PATTERN = longArrayOf(0, 1000, 1000)
        val RING_ATTRS: AudioAttributes = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_NOTIFICATION_RINGTONE)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()

        fun routeOf(type: Int): AudioRoute? = when (type) {
            AudioDeviceInfo.TYPE_BUILTIN_EARPIECE -> AudioRoute.EARPIECE
            AudioDeviceInfo.TYPE_BUILTIN_SPEAKER -> AudioRoute.SPEAKER
            AudioDeviceInfo.TYPE_BLUETOOTH_SCO, AudioDeviceInfo.TYPE_BLE_HEADSET -> AudioRoute.BLUETOOTH
            AudioDeviceInfo.TYPE_WIRED_HEADSET, AudioDeviceInfo.TYPE_WIRED_HEADPHONES, AudioDeviceInfo.TYPE_USB_HEADSET -> AudioRoute.WIRED
            else -> null
        }
    }
}
