package com.hush.app.ui.call

enum class CallPhase { IDLE, OUTGOING, INCOMING, CONNECTING, ACTIVE, ENDED }

/** 通话音频输出设备。声明顺序即界面上的展示顺序。 */
enum class AudioRoute(val label: String) {
    EARPIECE("听筒"), SPEAKER("扬声器"), BLUETOOTH("蓝牙耳机"), WIRED("有线耳机")
}

/**
 * 通话的纯判定逻辑（不依赖 Android / WebRTC，JVM 单测覆盖，见 CallLogicTest）。
 * [CallManager] 负责信令与媒体，这里只放“该不该做”的决策。
 */
object CallLogic {
    const val REMOTE_VIDEO_WINDOW_MS = 1_500L
    /** 媒体连接断开后等待恢复（含 ICE 重启）的上限，超时挂断。 */
    const val RECONNECT_TIMEOUT_MS = 25_000L

    /** 响铃、建立、接通中都算占线；IDLE / ENDED 空闲。 */
    fun isBusy(phase: CallPhase): Boolean = phase != CallPhase.IDLE && phase != CallPhase.ENDED

    /** 整个通话期间（含接听后的建立与通话中）都允许在锁屏之上显示通话界面，否则接听后会被锁屏盖住。 */
    fun showWhenLocked(phase: CallPhase): Boolean = isBusy(phase)

    /** 只有来电响铃时点亮屏幕。 */
    fun turnScreenOn(phase: CallPhase): Boolean = phase == CallPhase.INCOMING

    /** 通话建立期间需要高频补拉信令。 */
    fun needsFastSync(phase: CallPhase): Boolean =
        phase == CallPhase.OUTGOING || phase == CallPhase.INCOMING || phase == CallPhase.CONNECTING

    /**
     * 建立通话的协程在挂起点恢复后，是否仍属于当前通话：
     * 期间用户挂断（ENDED/IDLE）或已开始另一通话（callId 变化）时必须放弃，不能再创建连接、开摄像头或发 OFFER。
     */
    fun stillCurrent(expectedCallId: String, currentCallId: String, phase: CallPhase): Boolean =
        expectedCallId == currentCallId && isBusy(phase)

    /** 对方画面判定：lastFrameAt 为最后一帧到达时刻（elapsedRealtime），0 表示从未收到。 */
    fun hasRecentFrame(lastFrameAt: Long, now: Long, windowMs: Long = REMOTE_VIDEO_WINDOW_MS): Boolean =
        lastFrameAt > 0 && now - lastFrameAt < windowMs

    /**
     * 默认输出设备：接了有线耳机走有线，其次蓝牙；都没有时视频通话外放、语音通话用听筒
     * （没有听筒的设备退回扬声器）。
     */
    fun defaultRoute(video: Boolean, available: List<AudioRoute>): AudioRoute = when {
        AudioRoute.WIRED in available -> AudioRoute.WIRED
        AudioRoute.BLUETOOTH in available -> AudioRoute.BLUETOOTH
        video || AudioRoute.EARPIECE !in available -> AudioRoute.SPEAKER
        else -> AudioRoute.EARPIECE
    }

    /**
     * 只有听筒和扬声器两种选择时，按钮直接在两者之间切换（返回目标设备）；
     * 还接着耳机 / 蓝牙时返回 null，由界面弹出设备列表。
     */
    fun toggleTarget(current: AudioRoute, available: List<AudioRoute>): AudioRoute? {
        if (available.any { it == AudioRoute.BLUETOOTH || it == AudioRoute.WIRED }) return null
        return if (current == AudioRoute.SPEAKER && AudioRoute.EARPIECE in available) AudioRoute.EARPIECE else AudioRoute.SPEAKER
    }

    /**
     * 输出设备列表变化（耳机插拔、蓝牙连断）后应使用的设备：
     * 当前设备已消失时回到默认；新接入了耳机 / 蓝牙时自动切过去；否则保持用户的选择。
     */
    fun routeAfterDeviceChange(current: AudioRoute, before: List<AudioRoute>, after: List<AudioRoute>, video: Boolean): AudioRoute {
        if (current !in after) return defaultRoute(video, after)
        val added = after.filter { it !in before }
        return when {
            AudioRoute.WIRED in added -> AudioRoute.WIRED
            AudioRoute.BLUETOOTH in added -> AudioRoute.BLUETOOTH
            else -> current
        }
    }

    /** 距离感应熄屏（B7）：只在语音通话、用听筒、且已进入呼叫/通话阶段时启用。 */
    fun proximityOn(phase: CallPhase, video: Boolean, route: AudioRoute): Boolean =
        !video && route == AudioRoute.EARPIECE &&
            (phase == CallPhase.OUTGOING || phase == CallPhase.CONNECTING || phase == CallPhase.ACTIVE)

    // 系统情景模式取值与 AudioManager.RINGER_MODE_* 相同：0 静音、1 振动、2 响铃
    fun ringSound(ringerMode: Int): Boolean = ringerMode == 2
    fun ringVibrate(ringerMode: Int): Boolean = ringerMode == 1 || ringerMode == 2

    /**
     * 双方同时互拨（B9）：两边各自处于呼出状态时又收到对方的呼叫。
     * user_id 较大的一方让步——放弃自己的呼叫并直接接听对方的；较小的一方保持呼出、忽略对方的呼叫。
     * 两端用同一规则，结果必然一致，不会再出现两边都提示“对方忙线中”。
     */
    fun yieldsOnGlare(myId: String, peerId: String): Boolean = myId > peerId

    /** 通话页能否最小化为悬浮条：来电响铃必须留在全屏页上接听/拒绝，其余阶段都可以。 */
    fun canMinimize(phase: CallPhase): Boolean =
        phase == CallPhase.OUTGOING || phase == CallPhase.CONNECTING || phase == CallPhase.ACTIVE

    /** 通话前台服务需要的类型：麦克风（已授权时）与摄像头（视频通话且已授权时）。 */
    data class ServiceTypes(val microphone: Boolean, val camera: Boolean) {
        val any: Boolean get() = microphone || camera
    }

    fun serviceTypes(video: Boolean, micGranted: Boolean, cameraGranted: Boolean): ServiceTypes =
        ServiceTypes(microphone = micGranted, camera = video && cameraGranted)
}
