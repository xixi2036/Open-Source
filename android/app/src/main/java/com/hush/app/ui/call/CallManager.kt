package com.hush.app.ui.call

import com.hush.app.util.net

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.os.SystemClock
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.service.CallService
import com.hush.app.service.Notifier
import com.hush.app.util.HushLog as Log
import androidx.core.content.ContextCompat
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import org.webrtc.AudioSource
import org.webrtc.AudioTrack
import org.webrtc.Camera2Enumerator
import org.webrtc.CameraVideoCapturer
import org.webrtc.DefaultVideoDecoderFactory
import org.webrtc.DefaultVideoEncoderFactory
import org.webrtc.EglBase
import org.webrtc.IceCandidate
import org.webrtc.MediaConstraints
import org.webrtc.MediaStream
import org.webrtc.PeerConnection
import org.webrtc.PeerConnectionFactory
import org.webrtc.RtpReceiver
import org.webrtc.RtpSender
import org.webrtc.RtpTransceiver
import org.webrtc.SdpObserver
import org.webrtc.SessionDescription
import org.webrtc.SurfaceTextureHelper
import org.webrtc.VideoFrame
import org.webrtc.VideoSink
import org.webrtc.VideoSource
import org.webrtc.VideoTrack
import org.webrtc.audio.JavaAudioDeviceModule
import uniffi.hushcore.CallAction
import uniffi.hushcore.CallSignal
import uniffi.hushcore.CoreEvent
import java.util.UUID
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException
import uniffi.hushcore.IceCandidate as HushIce

data class CallUiState(
    val phase: CallPhase = CallPhase.IDLE,
    val peerId: String = "",
    val video: Boolean = false,
    val callId: String = "",
    val connectedAt: Long = 0,
    val muted: Boolean = false,
    val speaker: Boolean = false,
    val cameraOff: Boolean = false,
    val frontCamera: Boolean = true,
    val endReason: String? = null,
    val remoteVideo: VideoTrack? = null,
    val localVideo: VideoTrack? = null,
    /** 最近 [CallLogic.REMOTE_VIDEO_WINDOW_MS] 内收到过对方视频帧。 */
    val remoteHasVideo: Boolean = false,
    /** 本机无法发送视频时的原因（无摄像头 / 无权限 / 摄像头出错）；null 表示摄像头可用。 */
    val cameraIssue: String? = null,
)

/**
 * 1:1 语音/视频通话：WebRTC PeerConnection + 核心信令（send_call_signal / CallSignalReceived）。
 * 状态机：IDLE → OUTGOING/INCOMING → CONNECTING → ACTIVE → ENDED → IDLE。
 * 判定逻辑在 [CallLogic]（可单测），音频焦点/路由/铃声在 [CallAudio]。
 *
 * 生命周期：每次通话的建立过程（startCall / accept / 处理 ANSWER）运行在 [setupJob] 中，
 * [end] 先取消它再统一释放本通话创建的全部 WebRTC 对象；建立协程在每个挂起点之后
 * 用 [CallLogic.stillCurrent] 复核，已挂断时不再创建连接、打开摄像头或发出 OFFER。
 */
object CallManager {
    private const val TAG = "CallManager"
    private const val RING_TIMEOUT_MS = 30_000L
    /**
     * 被叫侧响铃上限：比主叫的 30 秒稍长，正常情况下由主叫超时发来的 HANGUP 结束来电；
     * 主叫失联时由被叫自己超时发 HANGUP 兜底。两种情况核心都记为“未接来电”（明确拒接只走 REJECT）。
     */
    private const val INCOMING_TIMEOUT_MS = 45_000L
    private const val REMOTE_VIDEO_POLL_MS = 500L

    private val _state = MutableStateFlow(CallUiState())
    val state: StateFlow<CallUiState> = _state

    private lateinit var appContext: Context
    private val scope get() = HushCore.scope
    val eglBase: EglBase by lazy { EglBase.create() }
    private var factory: PeerConnectionFactory? = null

    private var pc: PeerConnection? = null
    private var remoteOffer: String? = null
    private val pendingRemoteIce = mutableListOf<IceCandidate>()
    private var remoteDescSet = false
    private val outIce = mutableListOf<HushIce>()
    private var iceFlushJob: Job? = null
    private var timeoutJob: Job? = null
    /** 当前通话的建立协程（发起 / 接听 / 处理应答），挂断时取消。 */
    private var setupJob: Job? = null
    private var audioSource: AudioSource? = null
    private var audioTrack: AudioTrack? = null
    private var videoTrack: VideoTrack? = null
    private var videoSource: VideoSource? = null
    private var videoSender: RtpSender? = null
    private var remoteTrack: VideoTrack? = null
    @Volatile private var lastRemoteFrameAt = 0L
    private var remoteWatchJob: Job? = null
    /** 只记录帧到达时间，不持有帧；运行在解码线程。 */
    private val remoteFrameSink = VideoSink { _: VideoFrame -> lastRemoteFrameAt = SystemClock.elapsedRealtime() }
    private var capturer: CameraVideoCapturer? = null
    private var textureHelper: SurfaceTextureHelper? = null
    // CallAudio 只持有 applicationContext，不会泄漏 Activity
    @SuppressLint("StaticFieldLeak")
    private lateinit var audio: CallAudio

    fun init(context: Context) {
        appContext = context.applicationContext
        audio = CallAudio(appContext)
        scope.launch {
            HushCore.events.collect { ev ->
                if (ev is CoreEvent.CallSignalReceived) withContext(Dispatchers.Main) { onSignal(ev.fromUserId, ev.signal) }
            }
        }
    }

    val isBusy: Boolean get() = CallLogic.isBusy(_state.value.phase)

    init {
        // 呼叫/来电/连接中：请求核心高频补拉信令
        HushCore.scope.launch {
            _state.collect { s -> HushCore.fastSync = CallLogic.needsFastSync(s.phase) }
        }
    }

    /** 建立协程恢复后复核：本通话已结束或已被替换时放弃（以取消的方式退出，不再发 HANGUP）。 */
    private fun ensureCurrent(callId: String) {
        val st = _state.value
        if (!CallLogic.stillCurrent(callId, st.callId, st.phase)) throw CancellationException("call $callId no longer current")
    }

    // ---------------- 对外操作 ----------------

    fun startCall(peerId: String, video: Boolean) {
        if (isBusy) return
        val callId = UUID.randomUUID().toString()
        _state.value = CallUiState(phase = CallPhase.OUTGOING, peerId = peerId, video = video, callId = callId)
        ContactsStore.ensure(peerId)
        setupJob?.cancel()
        setupJob = scope.launch(Dispatchers.Main) {
            try {
                startMedia(video)
                val conn = createPeer(callId)
                addLocalTracks(conn, video, isCaller = true)
                val offer = conn.awaitCreate(offer = true, video = video)
                ensureCurrent(callId)
                conn.awaitSetLocal(offer)
                ensureCurrent(callId)
                send(CallSignal(callId, CallAction.OFFER, video, offer.description, emptyList()))
                armTimeout(callId) {
                    send(CallSignal(callId, CallAction.HANGUP, video, null, emptyList()))
                    end("无人接听")
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                Log.w(TAG, "startCall", e)
                send(CallSignal(callId, CallAction.HANGUP, video, null, emptyList()))
                end("通话建立失败")
            }
        }
    }

    fun accept() {
        val st = _state.value
        if (st.phase != CallPhase.INCOMING) return
        audio.stopRinging()
        timeoutJob?.cancel()
        Notifier.cancelIncomingCall(appContext)
        _state.update { it.copy(phase = CallPhase.CONNECTING) }
        setupJob?.cancel()
        setupJob = scope.launch(Dispatchers.Main) {
            try {
                startMedia(st.video)
                val conn = createPeer(st.callId)
                addLocalTracks(conn, st.video, isCaller = false)
                conn.awaitSetRemote(SessionDescription(SessionDescription.Type.OFFER, remoteOffer ?: error("no offer")))
                ensureCurrent(st.callId)
                remoteDescSet = true
                drainRemoteIce()
                val answer = conn.awaitCreate(offer = false, video = st.video)
                ensureCurrent(st.callId)
                conn.awaitSetLocal(answer)
                ensureCurrent(st.callId)
                send(CallSignal(st.callId, CallAction.ANSWER, st.video, answer.description, emptyList()))
                armTimeout(st.callId) {
                    if (_state.value.phase == CallPhase.CONNECTING) {
                        send(CallSignal(st.callId, CallAction.HANGUP, st.video, null, emptyList())); end("连接超时")
                    }
                }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                Log.w(TAG, "accept", e)
                send(CallSignal(st.callId, CallAction.HANGUP, st.video, null, emptyList()))
                end("通话建立失败")
            }
        }
    }

    /**
     * App 退到后台：来电在前台时到达（前台不发通知，只显示来电页），用户随即切走的话要补发来电通知，
     * 否则后台看不到任何提示（ProcessLifecycle 的 ON_STOP 有约 700ms 延迟，这段时间内到达的来电都会遇到）。
     */
    fun onAppBackground() {
        val st = _state.value
        if (st.phase == CallPhase.INCOMING) Notifier.showIncomingCall(appContext, st.peerId, st.video, st.callId)
    }

    /** 来电通知上的“拒绝”：只在仍是同一通来电时生效。 */
    fun rejectFromNotification(callId: String) {
        if (_state.value.callId == callId) reject()
        else Notifier.cancelIncomingCall(appContext)
    }

    fun reject() {
        val st = _state.value
        if (st.phase != CallPhase.INCOMING) return
        send(CallSignal(st.callId, CallAction.REJECT, st.video, null, emptyList()))
        end("已拒绝")
    }

    fun hangup() {
        val st = _state.value
        if (!isBusy) return
        send(CallSignal(st.callId, CallAction.HANGUP, st.video, null, emptyList()))
        end("通话已结束")
    }

    fun toggleMute() {
        val m = !_state.value.muted
        audioTrack?.setEnabled(!m)
        _state.update { it.copy(muted = m) }
    }

    fun toggleSpeaker() {
        val s = !_state.value.speaker
        audio.routeSpeaker(s)
        _state.update { it.copy(speaker = s) }
    }

    /**
     * 关摄像头：从 sender 上摘掉轨道并停止采集，对端收不到任何帧（而不是黑帧）；
     * 开摄像头：重新采集并把轨道挂回 sender。Unified Plan 下 setTrack 不需要重新协商。
     */
    fun toggleCamera() {
        val st = _state.value
        if (!st.video || st.cameraIssue != null) return
        val off = !st.cameraOff
        _state.update { it.copy(cameraOff = off) }
        // sender 尚未创建（本地轨道还在建立中）时只记录状态，addLocalTracks 会据此处理
        val sender = videoSender ?: return
        if (off) {
            runCatching { sender.setTrack(null, false) }.onFailure { Log.w(TAG, "setTrack(null)", it) }
            runCatching { capturer?.stopCapture() }.onFailure { Log.w(TAG, "stopCapture", it) }
        } else {
            runCatching { capturer?.startCapture(CAP_W, CAP_H, CAP_FPS) }.onFailure { Log.w(TAG, "startCapture", it) }
            runCatching { sender.setTrack(videoTrack, false) }.onFailure { Log.w(TAG, "setTrack(video)", it) }
        }
    }

    fun switchCamera() {
        if (_state.value.cameraOff || _state.value.cameraIssue != null) return
        capturer?.switchCamera(object : CameraVideoCapturer.CameraSwitchHandler {
            override fun onCameraSwitchDone(front: Boolean) { _state.update { it.copy(frontCamera = front) } }
            override fun onCameraSwitchError(error: String?) {}
        })
    }

    // ---------------- 信令 ----------------

    private fun onSignal(from: String, sig: CallSignal) {
        val st = _state.value
        when (sig.action) {
            CallAction.OFFER -> {
                if (isBusy) {
                    if (sig.callId != st.callId) send(CallSignal(sig.callId, CallAction.BUSY, sig.video, null, emptyList()), to = from)
                    return
                }
                remoteOffer = sig.sdp
                remoteDescSet = false
                pendingRemoteIce.clear()
                _state.value = CallUiState(phase = CallPhase.INCOMING, peerId = from, video = sig.video, callId = sig.callId)
                ContactsStore.ensure(from)
                audio.startRinging()
                Notifier.showIncomingCall(appContext, from, sig.video, sig.callId)
                armTimeout(sig.callId, INCOMING_TIMEOUT_MS) {
                    if (_state.value.phase == CallPhase.INCOMING) {
                        // 30 秒未接听：通知对方结束，核心据此记为未接来电
                        send(CallSignal(sig.callId, CallAction.HANGUP, sig.video, null, emptyList()))
                        end("未接听")
                    }
                }
            }
            CallAction.ANSWER -> {
                if (sig.callId != st.callId || st.phase != CallPhase.OUTGOING) return
                timeoutJob?.cancel()
                _state.update { it.copy(phase = CallPhase.CONNECTING) }
                val conn = pc ?: return
                setupJob?.cancel()
                setupJob = scope.launch(Dispatchers.Main) {
                    try {
                        conn.awaitSetRemote(SessionDescription(SessionDescription.Type.ANSWER, sig.sdp ?: error("no sdp")))
                        ensureCurrent(sig.callId)
                        remoteDescSet = true
                        drainRemoteIce()
                    } catch (e: CancellationException) {
                        throw e
                    } catch (e: Exception) {
                        Log.w(TAG, "answer", e); hangup()
                    }
                }
            }
            CallAction.ICE -> {
                if (sig.callId != st.callId) return
                sig.candidates.forEach { c ->
                    val ice = IceCandidate(c.sdpMid ?: "", c.sdpMlineIndex ?: 0, c.sdp)
                    if (remoteDescSet) pc?.addIceCandidate(ice) else pendingRemoteIce += ice
                }
            }
            CallAction.HANGUP -> if (sig.callId == st.callId && isBusy) end(if (st.phase == CallPhase.INCOMING) "对方已取消" else "对方已挂断")
            CallAction.REJECT -> if (sig.callId == st.callId && isBusy) end("对方已拒绝")
            CallAction.BUSY -> if (sig.callId == st.callId && isBusy) end("对方忙线中")
        }
    }

    private fun send(sig: CallSignal, to: String = _state.value.peerId) {
        if (to.isEmpty()) return
        scope.launch(Dispatchers.IO) { runCatching { net { HushCore.client.sendCallSignal(to, sig) } }.onFailure { Log.w(TAG, "send ${sig.action}", it) } }
    }

    private fun armTimeout(callId: String, timeoutMs: Long = RING_TIMEOUT_MS, onTimeout: () -> Unit) {
        timeoutJob?.cancel()
        timeoutJob = scope.launch(Dispatchers.Main) {
            delay(timeoutMs)
            if (_state.value.callId == callId && isBusy) onTimeout()
        }
    }

    // ---------------- WebRTC ----------------

    private fun ensureFactory(): PeerConnectionFactory {
        factory?.let { return it }
        PeerConnectionFactory.initialize(PeerConnectionFactory.InitializationOptions.builder(appContext).createInitializationOptions())
        val adm = JavaAudioDeviceModule.builder(appContext)
            .setUseHardwareAcousticEchoCanceler(true).setUseHardwareNoiseSuppressor(true)
            .createAudioDeviceModule()
        val f = PeerConnectionFactory.builder()
            .setAudioDeviceModule(adm)
            .setVideoEncoderFactory(DefaultVideoEncoderFactory(eglBase.eglBaseContext, true, true))
            .setVideoDecoderFactory(DefaultVideoDecoderFactory(eglBase.eglBaseContext))
            .createPeerConnectionFactory()
        factory = f
        return f
    }

    /** 创建 PeerConnection；等待 TURN 凭证期间挂断会在此处以取消退出，不会创建连接。 */
    private suspend fun createPeer(callId: String): PeerConnection {
        val turn = cachedTurn() ?: withContext(Dispatchers.IO) { fetchTurn() }
        ensureCurrent(callId)
        val servers = mutableListOf<PeerConnection.IceServer>()
        if (turn != null && turn.urls.isNotEmpty()) {
            servers += PeerConnection.IceServer.builder(turn.urls).setUsername(turn.username).setPassword(turn.credential).createIceServer()
        }
        val cfg = PeerConnection.RTCConfiguration(servers).apply {
            sdpSemantics = PeerConnection.SdpSemantics.UNIFIED_PLAN
            continualGatheringPolicy = PeerConnection.ContinualGatheringPolicy.GATHER_CONTINUALLY
        }
        val conn = ensureFactory().createPeerConnection(cfg, observer) ?: error("createPeerConnection failed")
        pc = conn
        return conn
    }

    private const val CAP_W = 1280
    private const val CAP_H = 720
    private const val CAP_FPS = 30

    private fun addLocalTracks(conn: PeerConnection, video: Boolean, isCaller: Boolean) {
        val f = ensureFactory()
        val aSource = f.createAudioSource(MediaConstraints())
        audioSource = aSource
        audioTrack = f.createAudioTrack("hush-a0", aSource).also { conn.addTrack(it, listOf("hush")) }
        if (video) addLocalVideo(conn, f, isCaller)
    }

    /**
     * 打开摄像头并添加视频轨道。没有摄像头 / 没有权限 / 打开失败时降级为只收不发：
     * 主叫方显式加一个 recvonly 视频收发器（被叫方由 setRemoteDescription 自动创建），
     * 保证仍能看到对方画面，并在界面上提示原因。
     */
    private fun addLocalVideo(conn: PeerConnection, f: PeerConnectionFactory, isCaller: Boolean) {
        var cap: CameraVideoCapturer? = null
        var helper: SurfaceTextureHelper? = null
        var src: VideoSource? = null
        val issue: String? = try {
            if (ContextCompat.checkSelfPermission(appContext, Manifest.permission.CAMERA) != PackageManager.PERMISSION_GRANTED) {
                "未获得摄像头权限"
            } else {
                val enumerator = Camera2Enumerator(appContext)
                val names = enumerator.deviceNames.orEmpty()
                val name = names.firstOrNull { enumerator.isFrontFacing(it) } ?: names.firstOrNull()
                if (name == null) {
                    "无可用摄像头"
                } else {
                    cap = enumerator.createCapturer(name, cameraEvents) ?: error("createCapturer($name) returned null")
                    helper = SurfaceTextureHelper.create("hush-cap", eglBase.eglBaseContext) ?: error("SurfaceTextureHelper failed")
                    src = f.createVideoSource(false)
                    cap.initialize(helper, appContext, src.capturerObserver)
                    val track = f.createVideoTrack("hush-v0", src)
                    val sender = conn.addTrack(track, listOf("hush"))
                    capturer = cap; textureHelper = helper; videoSource = src; videoTrack = track; videoSender = sender
                    if (_state.value.cameraOff) {
                        // 建立过程中用户已点了“关摄像头”
                        sender.setTrack(null, false)
                    } else {
                        cap.startCapture(CAP_W, CAP_H, CAP_FPS)
                    }
                    _state.update { it.copy(localVideo = track, frontCamera = enumerator.isFrontFacing(name)) }
                    null
                }
            }
        } catch (e: Exception) {
            Log.w(TAG, "camera setup failed", e)
            if (capturer == null) {
                runCatching { cap?.dispose() }; runCatching { src?.dispose() }; runCatching { helper?.dispose() }
            } else {
                videoSender?.let { s -> runCatching { s.setTrack(null, false) } }
            }
            "无可用摄像头"
        }
        if (issue != null) {
            Log.w(TAG, "video call without local camera: $issue")
            if (videoSender == null && isCaller) {
                runCatching {
                    conn.addTransceiver(
                        org.webrtc.MediaStreamTrack.MediaType.MEDIA_TYPE_VIDEO,
                        RtpTransceiver.RtpTransceiverInit(RtpTransceiver.RtpTransceiverDirection.RECV_ONLY),
                    )
                }.onFailure { Log.w(TAG, "addTransceiver(recvonly)", it) }
            }
            _state.update { it.copy(cameraIssue = issue, localVideo = null) }
        }
    }

    /** 采集运行中出错（被占用、断开等）：停止发送视频并提示。 */
    private val cameraEvents = object : CameraVideoCapturer.CameraEventsHandler {
        override fun onCameraError(err: String?) = onCameraFailure("error: $err")
        override fun onCameraDisconnected() = onCameraFailure("disconnected")
        override fun onCameraFreezed(err: String?) { Log.w(TAG, "camera freezed: $err") }
        override fun onCameraOpening(name: String?) {}
        override fun onFirstFrameAvailable() {}
        override fun onCameraClosed() {}
    }

    private fun onCameraFailure(what: String) {
        Log.w(TAG, "camera $what")
        scope.launch(Dispatchers.Main) {
            if (!isBusy || capturer == null || _state.value.cameraIssue != null) return@launch
            videoSender?.let { runCatching { it.setTrack(null, false) } }
            _state.update { it.copy(cameraIssue = "摄像头不可用", localVideo = null) }
        }
    }

    /** 给远端视频轨道挂一个只记时间的 sink，每 500ms 汇总一次到 remoteHasVideo。 */
    private fun watchRemoteVideo(track: VideoTrack) {
        if (remoteTrack === track) return
        remoteTrack?.let { old -> runCatching { old.removeSink(remoteFrameSink) } }
        remoteTrack = track
        lastRemoteFrameAt = 0L
        runCatching { track.addSink(remoteFrameSink) }.onFailure { Log.w(TAG, "addSink(remote)", it) }
        remoteWatchJob?.cancel()
        remoteWatchJob = scope.launch(Dispatchers.Main) {
            while (true) {
                val has = CallLogic.hasRecentFrame(lastRemoteFrameAt, SystemClock.elapsedRealtime())
                if (_state.value.remoteHasVideo != has) _state.update { it.copy(remoteHasVideo = has) }
                delay(REMOTE_VIDEO_POLL_MS)
            }
        }
    }

    // ---------------- TURN 凭证缓存 ----------------

    @Volatile private var turnCache: uniffi.hushcore.TurnCredentials? = null
    @Volatile private var turnFetchedAt = 0L

    private fun cachedTurn(): uniffi.hushcore.TurnCredentials? {
        val t = turnCache ?: return null
        // ttl 单位为秒；留 10 分钟余量
        return t.takeIf { System.currentTimeMillis() - turnFetchedAt < (t.ttl - 600).coerceAtLeast(60) * 1000 }
    }

    private suspend fun fetchTurn(): uniffi.hushcore.TurnCredentials? =
        runCatching { net { HushCore.client.turnCredentials() } }.getOrNull()?.also { turnCache = it; turnFetchedAt = System.currentTimeMillis() }

    /** 登录后预取 TURN 凭证，发起/接听通话时无需等待网络请求。 */
    fun prefetchTurn() {
        if (cachedTurn() != null) return
        scope.launch(Dispatchers.IO) { fetchTurn() }
    }

    private fun drainRemoteIce() {
        pendingRemoteIce.forEach { pc?.addIceCandidate(it) }
        pendingRemoteIce.clear()
    }

    private val observer = object : PeerConnection.Observer {
        override fun onIceCandidate(c: IceCandidate) {
            scope.launch(Dispatchers.Main) {
                outIce += HushIce(c.sdp, c.sdpMid, c.sdpMLineIndex)
                if (iceFlushJob?.isActive != true) iceFlushJob = scope.launch(Dispatchers.Main) {
                    delay(250)
                    val batch = outIce.toList(); outIce.clear()
                    val st = _state.value
                    if (batch.isNotEmpty() && isBusy) send(CallSignal(st.callId, CallAction.ICE, st.video, null, batch))
                }
            }
        }

        override fun onIceConnectionChange(s: PeerConnection.IceConnectionState) {
            scope.launch(Dispatchers.Main) {
                when (s) {
                    PeerConnection.IceConnectionState.CONNECTED, PeerConnection.IceConnectionState.COMPLETED -> {
                        timeoutJob?.cancel()
                        if (_state.value.phase == CallPhase.CONNECTING || _state.value.phase == CallPhase.OUTGOING) {
                            _state.update { it.copy(phase = CallPhase.ACTIVE, connectedAt = System.currentTimeMillis()) }
                        }
                    }
                    PeerConnection.IceConnectionState.FAILED -> if (isBusy) hangup()
                    else -> Unit
                }
            }
        }

        override fun onTrack(transceiver: RtpTransceiver) {
            val track = transceiver.receiver.track()
            if (track is VideoTrack) scope.launch(Dispatchers.Main) {
                if (pc == null) return@launch
                watchRemoteVideo(track)
                _state.update { it.copy(remoteVideo = track) }
            }
        }

        override fun onAddTrack(receiver: RtpReceiver, streams: Array<out MediaStream>) {}
        override fun onSignalingChange(s: PeerConnection.SignalingState) {}
        override fun onIceConnectionReceivingChange(b: Boolean) {}
        override fun onIceGatheringChange(s: PeerConnection.IceGatheringState) {}
        override fun onIceCandidatesRemoved(c: Array<out IceCandidate>) {}
        override fun onAddStream(s: MediaStream) {}
        override fun onRemoveStream(s: MediaStream) {}
        override fun onDataChannel(d: org.webrtc.DataChannel) {}
        override fun onRenegotiationNeeded() {}
    }

    private suspend fun PeerConnection.awaitCreate(offer: Boolean, video: Boolean): SessionDescription =
        suspendCancellableCoroutine { cont ->
            val c = MediaConstraints().apply {
                mandatory += MediaConstraints.KeyValuePair("OfferToReceiveAudio", "true")
                mandatory += MediaConstraints.KeyValuePair("OfferToReceiveVideo", video.toString())
            }
            val obs = object : SdpObserver {
                override fun onCreateSuccess(sdp: SessionDescription) { cont.resume(sdp) }
                override fun onCreateFailure(e: String?) { cont.resumeWithException(IllegalStateException(e)) }
                override fun onSetSuccess() {}
                override fun onSetFailure(e: String?) {}
            }
            if (offer) createOffer(obs, c) else createAnswer(obs, c)
        }

    private suspend fun PeerConnection.awaitSetLocal(sdp: SessionDescription) = awaitSet { setLocalDescription(it, sdp) }
    private suspend fun PeerConnection.awaitSetRemote(sdp: SessionDescription) = awaitSet { setRemoteDescription(it, sdp) }

    private suspend fun awaitSet(block: (SdpObserver) -> Unit): Unit = suspendCancellableCoroutine { cont ->
        block(object : SdpObserver {
            override fun onCreateSuccess(sdp: SessionDescription?) {}
            override fun onCreateFailure(e: String?) {}
            override fun onSetSuccess() { cont.resume(Unit) }
            override fun onSetFailure(e: String?) { cont.resumeWithException(IllegalStateException(e)) }
        })
    }

    // ---------------- 音频 / 前台服务 ----------------

    /** 进入通话媒体阶段：音频焦点与路由（接耳机时不强制外放），并启动通话前台服务（此时 App 在前台）。 */
    private fun startMedia(video: Boolean) {
        val speaker = audio.start(video)
        _state.update { it.copy(speaker = speaker) }
        CallService.start(appContext, video)
    }

    // ---------------- 结束 ----------------

    private fun end(reason: String) {
        setupJob?.cancel(); setupJob = null
        timeoutJob?.cancel()
        iceFlushJob?.cancel()
        audio.stopRinging()
        Notifier.cancelIncomingCall(appContext)
        remoteWatchJob?.cancel(); remoteWatchJob = null
        // 须在 pc.dispose() 之前摘掉 sink（之后原生轨道已释放）
        remoteTrack?.let { t -> runCatching { t.removeSink(remoteFrameSink) } }
        remoteTrack = null; lastRemoteFrameAt = 0L
        val endedCallId = _state.value.callId
        // 先让界面释放渲染器，再销毁 WebRTC 对象
        _state.update { it.copy(phase = CallPhase.ENDED, endReason = reason, remoteVideo = null, localVideo = null, remoteHasVideo = false) }
        val oldCap = capturer; val oldHelper = textureHelper; val oldSrc = videoSource; val oldPc = pc
        val oldAudioTrack = audioTrack; val oldVideoTrack = videoTrack; val oldAudioSrc = audioSource
        capturer = null; textureHelper = null; videoSource = null; videoTrack = null; videoSender = null
        audioTrack = null; audioSource = null; pc = null
        remoteOffer = null; remoteDescSet = false; pendingRemoteIce.clear(); outIce.clear()
        audio.stop()
        CallService.stop(appContext)
        scope.launch(Dispatchers.Main) {
            delay(300)
            runCatching { oldCap?.stopCapture() }
            runCatching { oldCap?.dispose() }
            // PeerConnection.dispose 只释放 sender 自己持有的轨道引用；本地创建的轨道与音频源须自行释放
            runCatching { oldPc?.dispose() }
            runCatching { oldVideoTrack?.dispose() }
            runCatching { oldAudioTrack?.dispose() }
            runCatching { oldSrc?.dispose() }
            runCatching { oldAudioSrc?.dispose() }
            runCatching { oldHelper?.dispose() }
            delay(1500)
            if (_state.value.phase == CallPhase.ENDED && _state.value.callId == endedCallId) _state.value = CallUiState()
        }
    }
}
