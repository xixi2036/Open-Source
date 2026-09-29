package com.hush.app.ui.call

enum class CallPhase { IDLE, OUTGOING, INCOMING, CONNECTING, ACTIVE, ENDED }

/**
 * 通话的纯判定逻辑（不依赖 Android / WebRTC，JVM 单测覆盖，见 CallLogicTest）。
 * [CallManager] 负责信令与媒体，这里只放“该不该做”的决策。
 */
object CallLogic {
    const val REMOTE_VIDEO_WINDOW_MS = 1_500L

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

    /** 视频通话默认外放；接了有线/蓝牙/USB 耳机时走耳机，不强制外放。 */
    fun defaultSpeaker(video: Boolean, headsetConnected: Boolean): Boolean = video && !headsetConnected

    /** 通话前台服务需要的类型：麦克风（已授权时）与摄像头（视频通话且已授权时）。 */
    data class ServiceTypes(val microphone: Boolean, val camera: Boolean) {
        val any: Boolean get() = microphone || camera
    }

    fun serviceTypes(video: Boolean, micGranted: Boolean, cameraGranted: Boolean): ServiceTypes =
        ServiceTypes(microphone = micGranted, camera = video && cameraGranted)
}
