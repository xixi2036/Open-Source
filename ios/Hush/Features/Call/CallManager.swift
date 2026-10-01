import Foundation
import Observation
import AVFoundation
import WebRTC
import UIKit

/// 因缺少权限而无法通话
enum CallBlock: Equatable {
    case microphone, camera

    var endReason: String { self == .microphone ? "未获得麦克风权限" : "未获得摄像头权限" }
    var title: String { self == .microphone ? "无法使用麦克风" : "无法使用摄像头" }
    var message: String {
        self == .microphone
            ? "通话需要麦克风。请在系统「设置 › Hush」中允许使用麦克风后重试。"
            : "视频通话需要摄像头。请在系统「设置 › Hush」中允许使用相机后重试。"
    }
}

enum CallPhase: Equatable {
    case idle
    case outgoing      // 正在呼叫
    case incoming      // 来电响铃
    case connecting    // 已接听，建立媒体中
    case active        // 通话中
    case ended(String) // 结束提示（短暂显示）
}

/// 1:1 语音/视频通话：WebRTC + 核心信令（send_call_signal / CallSignalReceived）
@MainActor @Observable
final class CallManager {
    static let shared = CallManager()

    var phase: CallPhase = .idle {
        didSet {
            if !isLive { minimized = false; reconnecting = false }
            applyDeviceState()
            applyTone()
        }
    }
    /// 通话页最小化为悬浮条（可以回到聊天继续操作）
    var minimized = false
    /// 麦克风 / 摄像头权限被拒绝，通话已中止：通话页显示说明并引导去系统设置（A4）
    var blocked: CallBlock?
    /// 媒体连接中断（ICE disconnected / failed），正在尝试恢复（B7）
    var reconnecting = false
    var peerId = ""
    var callId = ""
    var video = false
    var isCaller = false
    var connectedAt: Date?
    var muted = false
    var speaker = false {
        didSet { applyDeviceState() }
    }
    /// 被系统电话 / Siri 等打断，本地麦克风暂停
    var interrupted = false
    var cameraOff = false
    var frontCamera = true
    var localVideo: RTCVideoTrack?
    var remoteVideo: RTCVideoTrack?
    /// 对方最近 1.5 秒内是否有画面（关闭摄像头或没有摄像头时为 false）
    var remoteHasVideo = false
    /// 本机没有摄像头（如模拟器），正在发送合成的测试画面
    var syntheticCamera = false

    @ObservationIgnored private static let factory: RTCPeerConnectionFactory = {
        RTCInitializeSSL()
        return RTCPeerConnectionFactory(encoderFactory: RTCDefaultVideoEncoderFactory(),
                                        decoderFactory: RTCDefaultVideoDecoderFactory())
    }()
    @ObservationIgnored private var pc: RTCPeerConnection?
    @ObservationIgnored private var pcDelegate: PeerDelegate?
    @ObservationIgnored private var localAudio: RTCAudioTrack?
    @ObservationIgnored private var capturer: RTCCameraVideoCapturer?
    @ObservationIgnored private var synthetic: SyntheticCameraCapturer?
    @ObservationIgnored private var videoSender: RTCRtpSender?
    @ObservationIgnored private var remoteWatcher: VideoFrameWatcher?
    @ObservationIgnored private var watchTimer: Timer?
    @ObservationIgnored private var remoteOfferSdp: String?
    @ObservationIgnored private var pendingRemoteCandidates: [RTCIceCandidate] = []
    @ObservationIgnored private var outgoingCandidates: [IceCandidate] = []
    @ObservationIgnored private var flushScheduled = false
    @ObservationIgnored private var timeoutWork: DispatchWorkItem?
    /// 连接中断后的恢复期限；到期仍未恢复则结束通话
    @ObservationIgnored private var reconnectWork: DispatchWorkItem?
    /// 主叫正在做 ICE restart（已发出新的 offer，等待 answer）
    @ObservationIgnored private var restarting = false
    /// 去电的 offer 已发出（此后播放回铃音）
    @ObservationIgnored private var offerSent = false
    @ObservationIgnored private let tones = CallTones()
    /// 超时时长（测试中缩短）
    @ObservationIgnored var ringTimeout: TimeInterval = 30
    @ObservationIgnored var connectTimeout: TimeInterval = 30
    @ObservationIgnored var reconnectTimeout: TimeInterval = 20
    /// 每次新通话 / 结束通话都会递增；异步建立流程在每个 await 之后核对，过期即放弃
    @ObservationIgnored private(set) var currentAttempt = 0
    @ObservationIgnored private var setupTask: Task<Void, Never>?
    @ObservationIgnored private var interruptionObserver: NSObjectProtocol?
    @ObservationIgnored private var core: HushCore { HushCore.shared }

    init() {
        interruptionObserver = NotificationCenter.default.addObserver(
            forName: AVAudioSession.interruptionNotification, object: nil, queue: .main
        ) { [weak self] note in
            let raw = note.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt
            MainActor.assumeIsolated { self?.onAudioInterruption(raw.flatMap(AVAudioSession.InterruptionType.init)) }
        }
    }

    var isPresented: Bool { phase != .idle || blocked != nil }

    /// 通话仍在进行（响铃、呼叫、连接、通话中）
    var isLive: Bool {
        switch phase {
        case .idle, .ended: return false
        default: return true
        }
    }

    /// 某次异步建立流程是否仍对应当前通话
    func isCurrent(_ attempt: Int) -> Bool { attempt == currentAttempt && isLive }

    private func beginAttempt() -> Int {
        setupTask?.cancel()
        setupTask = nil
        currentAttempt += 1
        return currentAttempt
    }
    var peerName: String { core.contacts.displayName(peerId) }

    // MARK: - 发起 / 接听 / 挂断

    func startCall(to userId: String, video: Bool) {
        guard !isLive else { core.toast = "当前正在通话中"; return }
        let attempt = beginAttempt()
        blocked = nil
        stopOtherAudio()
        peerId = userId
        callId = UUID().uuidString.lowercased()
        self.video = video
        isCaller = true
        resetMediaFlags()
        phase = .outgoing
        armTimeout(ringTimeout) { [weak self] in
            guard let self, self.phase == .outgoing else { return }
            self.send(.hangup)
            self.end("对方无应答")
        }
        let cid = callId
        setupTask = Task { [weak self] in
            guard let self else { return }
            let block = await self.requestPermissions()
            guard self.isCurrent(attempt) else { return }
            if let block {
                // A4：没有权限就不呼叫（还没发出 offer，对方不会响铃）
                self.end(block.endReason)
                self.blocked = block
                return
            }
            do {
                guard let pc = try await self.setupPeerConnection(attempt: attempt) else { return }
                let offer = try await pc.offer(for: Self.constraints)
                guard self.isCurrent(attempt) else { return }
                try await pc.setLocalDescription(offer)
                guard self.isCurrent(attempt) else { return }
                try await self.core.sendCallSignal(to: userId, CallSignal(
                    callId: cid, action: .offer, video: video, sdp: offer.sdp, candidates: []))
                guard self.isCurrent(attempt) else { return }
                self.offerSent = true
                self.applyTone()
            } catch {
                guard self.isCurrent(attempt) else { return }
                self.core.toast = "呼叫失败：\(describe(error))"
                self.send(.hangup)
                self.end("呼叫失败")
            }
        }
    }

    func accept() {
        guard phase == .incoming, let sdp = remoteOfferSdp else { return }
        phase = .connecting
        NotificationService.shared.withdrawIncomingCall(callId: callId)
        armConnectTimeout()
        let attempt = currentAttempt
        let (uid, cid, v) = (peerId, callId, video)
        setupTask?.cancel()
        setupTask = Task { [weak self] in
            guard let self else { return }
            let block = await self.requestPermissions()
            guard self.isCurrent(attempt) else { return }
            if let block {
                // A4：没有权限无法通话，告知对方并说明原因
                self.send(.hangup)
                self.end(block.endReason)
                self.blocked = block
                return
            }
            do {
                guard let pc = try await self.setupPeerConnection(attempt: attempt) else { return }
                try await pc.setRemoteDescription(RTCSessionDescription(type: .offer, sdp: sdp))
                guard self.isCurrent(attempt) else { return }
                self.flushRemoteCandidates()
                let answer = try await pc.answer(for: Self.constraints)
                guard self.isCurrent(attempt) else { return }
                try await pc.setLocalDescription(answer)
                guard self.isCurrent(attempt) else { return }
                try await self.core.sendCallSignal(to: uid, CallSignal(
                    callId: cid, action: .answer, video: v, sdp: answer.sdp, candidates: []))
                if self.isCurrent(attempt), self.connectedAt == nil { self.connectedAt = Date() }
            } catch {
                guard self.isCurrent(attempt) else { return }
                self.core.toast = "通话建立失败"
                self.send(.hangup)
                self.end("通话建立失败")
            }
        }
    }

    func reject() {
        guard phase == .incoming else { return }
        send(.reject)
        end("已拒绝")
    }

    /// 挂断（任何阶段）
    func hangup() {
        switch phase {
        case .idle, .ended: return
        case .incoming: send(.reject)
        default: send(.hangup)
        }
        end("通话已结束")
    }

    // MARK: - 信令

    func handleSignal(from userId: String, signal: CallSignal) {
        switch signal.action {
        case .offer:
            // 结束提示显示期间（.ended）视为空闲，可以接新来电
            if signal.callId == callId, userId == peerId, !isCaller, phase == .active || phase == .connecting,
               let sdp = signal.sdp, let pc, pc.remoteDescription != nil {
                // B7：对方（主叫）做 ICE restart 发来的新 offer——在同一路通话上重新协商
                renegotiate(pc, offer: sdp)
                return
            }
            if isLive || signal.callId == callId {
                if signal.callId != callId {
                    let core = core
                    Task { try? await core.sendCallSignal(to: userId, CallSignal(
                        callId: signal.callId, action: .busy, video: signal.video, sdp: nil, candidates: [])) }
                }
                return
            }
            _ = beginAttempt()
            peerId = userId
            callId = signal.callId
            video = signal.video
            isCaller = false
            remoteOfferSdp = signal.sdp
            resetMediaFlags()
            blocked = nil
            stopOtherAudio()
            phase = .incoming
            NotificationService.shared.notifyIncomingCall(callId: callId, from: peerName, video: video)
            armTimeout(ringTimeout) { [weak self] in
                guard let self, self.phase == .incoming else { return }
                self.send(.hangup)
                self.end("未接来电")
            }
        case .answer:
            guard signal.callId == callId, userId == peerId, isCaller, let sdp = signal.sdp, let pc else { return }
            if restarting, phase == .active || phase == .connecting {
                // B7：ICE restart 的应答
                restarting = false
                let attempt = currentAttempt
                Task { [weak self] in
                    try? await pc.setRemoteDescription(RTCSessionDescription(type: .answer, sdp: sdp))
                    guard let self, self.isCurrent(attempt) else { return }
                    self.flushRemoteCandidates()
                }
                return
            }
            guard phase == .outgoing else { return }
            phase = .connecting
            armConnectTimeout()
            let attempt = currentAttempt
            Task { [weak self] in
                do {
                    try await pc.setRemoteDescription(RTCSessionDescription(type: .answer, sdp: sdp))
                    guard let self, self.isCurrent(attempt) else { return }
                    self.flushRemoteCandidates()
                    if self.connectedAt == nil { self.connectedAt = Date() }
                } catch {
                    guard let self, self.isCurrent(attempt) else { return }
                    self.send(.hangup)
                    self.end("通话建立失败")
                }
            }
        case .ice:
            guard signal.callId == callId, userId == peerId, isLive else { return }
            for c in signal.candidates {
                let ice = RTCIceCandidate(sdp: c.sdp, sdpMLineIndex: c.sdpMlineIndex ?? 0, sdpMid: c.sdpMid)
                if let pc, pc.remoteDescription != nil {
                    pc.add(ice) { _ in }
                } else {
                    pendingRemoteCandidates.append(ice)
                }
            }
        case .hangup:
            guard signal.callId == callId, userId == peerId, isLive else { return }
            end(phase == .incoming ? "对方已取消" : "对方已挂断")
        case .reject:
            guard signal.callId == callId, userId == peerId, isLive else { return }
            end("对方已拒绝")
        case .busy:
            guard signal.callId == callId, userId == peerId, isLive else { return }
            end("对方忙线中")
        }
    }

    private func send(_ action: CallAction) {
        let (uid, cid, v) = (peerId, callId, video)
        guard !uid.isEmpty else { return }
        let core = core
        Task { try? await core.sendCallSignal(to: uid, CallSignal(callId: cid, action: action, video: v, sdp: nil, candidates: [])) }
    }

    // MARK: - 媒体控制

    func toggleMute() {
        muted.toggle()
        localAudio?.isEnabled = !muted && !interrupted
    }

    func toggleSpeaker() {
        speaker.toggle()
        applyAudioRoute()
    }

    /// 关闭摄像头时从发送端摘掉视频轨道（对方停止收到帧，显示“对方未开启摄像头”），而不是发送黑帧
    func toggleCamera() {
        cameraOff.toggle()
        if cameraOff {
            videoSender?.track = nil
            capturer?.stopCapture()
            synthetic?.stop()
        } else {
            videoSender?.track = localVideo
            if let synthetic { synthetic.start() } else { startCapture() }
        }
    }

    func switchCamera() {
        guard !syntheticCamera else { return }
        frontCamera.toggle()
        startCapture()
    }

    // MARK: - 内部

    private static let constraints = RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: ["DtlsSrtpKeyAgreement": "true"])

    private func resetMediaFlags() {
        muted = false; cameraOff = false; frontCamera = true
        remoteHasVideo = false; syntheticCamera = false
        speaker = video
        interrupted = false
        connectedAt = nil
        pendingRemoteCandidates = []
        outgoingCandidates = []
        minimized = false
        reconnecting = false
        restarting = false
        offerSent = false
    }

    /// 权限检查（可在测试中替换）：返回 (麦克风, 摄像头) 是否已授权；未决定时会弹系统授权框
    @ObservationIgnored var permissionRequest: (_ video: Bool) async -> (mic: Bool, camera: Bool) = { video in
        let mic = await AVCaptureDevice.requestAccess(for: .audio)
        let cam = video ? await AVCaptureDevice.requestAccess(for: .video) : true
        return (mic, cam)
    }

    /// A4：检查授权结果；被拒绝时返回原因，调用方中止通话
    private func requestPermissions() async -> CallBlock? {
        let r = await permissionRequest(video)
        return Self.permissionBlock(mic: r.mic, camera: r.camera, video: video)
    }

    nonisolated static func permissionBlock(mic: Bool, camera: Bool, video: Bool) -> CallBlock? {
        if !mic { return .microphone }
        if video && !camera { return .camera }
        return nil
    }

    /// 通话开始（呼出 / 来电）：停止正在播放的语音消息与进行中的录音（B7）
    private func stopOtherAudio() {
        VoicePlayer.shared.stop()
        VoiceRecorder.cancelActive()
    }

    private func applyTone() {
        tones.apply(CallTones.tone(phase: phase, isCaller: isCaller, offerSent: offerSent))
    }

    /// 被叫：主叫 ICE restart 后在同一路通话上重新协商
    private func renegotiate(_ pc: RTCPeerConnection, offer sdp: String) {
        let attempt = currentAttempt
        let (uid, cid, v) = (peerId, callId, video)
        Task { [weak self] in
            guard let self else { return }
            do {
                try await pc.setRemoteDescription(RTCSessionDescription(type: .offer, sdp: sdp))
                guard self.isCurrent(attempt) else { return }
                let answer = try await pc.answer(for: Self.constraints)
                guard self.isCurrent(attempt) else { return }
                try await pc.setLocalDescription(answer)
                guard self.isCurrent(attempt) else { return }
                try await self.core.sendCallSignal(to: uid, CallSignal(
                    callId: cid, action: .answer, video: v, sdp: answer.sdp, candidates: []))
            } catch {
                // 重新协商失败不主动挂断：原连接可能自行恢复，否则由恢复期限结束通话
            }
        }
    }

    /// 主叫：连接中断后做一次 ICE restart（只由主叫发起，避免双方同时发 offer）
    private func restartIce() {
        guard isCaller, !restarting, isLive, let pc, pc.remoteDescription != nil else { return }
        restarting = true
        let attempt = currentAttempt
        let (uid, cid, v) = (peerId, callId, video)
        Task { [weak self] in
            guard let self else { return }
            do {
                pc.restartIce()
                let offer = try await pc.offer(for: Self.restartConstraints)
                guard self.isCurrent(attempt) else { return }
                try await pc.setLocalDescription(offer)
                guard self.isCurrent(attempt) else { return }
                try await self.core.sendCallSignal(to: uid, CallSignal(
                    callId: cid, action: .offer, video: v, sdp: offer.sdp, candidates: []))
            } catch {
                if self.isCurrent(attempt) { self.restarting = false }
            }
        }
    }

    private static let restartConstraints = RTCMediaConstraints(mandatoryConstraints: ["IceRestart": "true"],
                                                               optionalConstraints: ["DtlsSrtpKeyAgreement": "true"])

    private func configureAudioSession() {
        let s = RTCAudioSession.sharedInstance()
        s.lockForConfiguration()
        defer { s.unlockForConfiguration() }
        try? s.setCategory(.playAndRecord, mode: .voiceChat, options: [.allowBluetoothHFP])
        try? s.setActive(true)
        applyAudioRoute()
    }

    private func applyAudioRoute() {
        try? AVAudioSession.sharedInstance().overrideOutputAudioPort(speaker ? .speaker : .none)
    }

    /// 创建 PeerConnection 并开始采集。等待 TURN 凭据期间通话若已结束则返回 nil，不创建任何媒体资源。
    private func setupPeerConnection(attempt: Int) async throws -> RTCPeerConnection? {
        let creds = await core.turnCredentials()
        guard isCurrent(attempt) else { return nil }
        releaseMedia()   // 防御：不留下上一路未关闭的连接
        let config = RTCConfiguration()
        var servers: [RTCIceServer] = []
        if let creds, !creds.urls.isEmpty {
            servers.append(RTCIceServer(urlStrings: creds.urls, username: creds.username, credential: creds.credential))
        }
        config.iceServers = servers
        config.sdpSemantics = .unifiedPlan
        config.continualGatheringPolicy = .gatherContinually
        let delegate = PeerDelegate(owner: self)
        guard let pc = Self.factory.peerConnection(with: config, constraints: Self.constraints, delegate: delegate) else {
            throw HushError.Internal(message: "无法创建 PeerConnection")
        }
        self.pc = pc
        pcDelegate = delegate
        configureAudioSession()
        let audioSource = Self.factory.audioSource(with: RTCMediaConstraints(mandatoryConstraints: nil, optionalConstraints: nil))
        let audio = Self.factory.audioTrack(with: audioSource, trackId: "audio0")
        pc.add(audio, streamIds: ["hush"])
        localAudio = audio
        if video {
            let source = Self.factory.videoSource()
            let track = Self.factory.videoTrack(with: source, trackId: "video0")
            videoSender = pc.add(track, streamIds: ["hush"])
            localVideo = track
            if RTCCameraVideoCapturer.captureDevices().isEmpty {
                // 没有摄像头（模拟器）：发送合成画面，对方能看到明确的“模拟摄像头”提示
                syntheticCamera = true
                let s = SyntheticCameraCapturer(delegate: source, label: core.account?.profile.name ?? "Hush")
                synthetic = s
                s.start()
            } else {
                capturer = RTCCameraVideoCapturer(delegate: source)
                startCapture()
            }
        }
        return pc
    }

    private func startCapture() {
        guard let capturer else { return }
        let pos: AVCaptureDevice.Position = frontCamera ? .front : .back
        guard let device = RTCCameraVideoCapturer.captureDevices().first(where: { $0.position == pos })
                ?? RTCCameraVideoCapturer.captureDevices().first else { return }
        let formats = RTCCameraVideoCapturer.supportedFormats(for: device)
        let format = formats.min(by: { abs(CMVideoFormatDescriptionGetDimensions($0.formatDescription).width - 1280) <
                                       abs(CMVideoFormatDescriptionGetDimensions($1.formatDescription).width - 1280) })
        guard let format else { return }
        let fps = min(30, Int(format.videoSupportedFrameRateRanges.map(\.maxFrameRate).max() ?? 30))
        capturer.startCapture(with: device, format: format, fps: fps)
    }

    private func flushRemoteCandidates() {
        guard let pc else { return }
        for c in pendingRemoteCandidates { pc.add(c) { _ in } }
        pendingRemoteCandidates = []
    }

    fileprivate func onLocalCandidate(_ c: RTCIceCandidate) {
        outgoingCandidates.append(IceCandidate(sdp: c.sdp, sdpMid: c.sdpMid, sdpMlineIndex: c.sdpMLineIndex))
        guard !flushScheduled else { return }
        flushScheduled = true
        // 批量发送 ICE，减少信令条数
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { [weak self] in
            MainActor.assumeIsolated {
                guard let self else { return }
                self.flushScheduled = false
                let batch = self.outgoingCandidates
                self.outgoingCandidates = []
                guard !batch.isEmpty, self.isLive else { return }
                let (uid, cid, v, core) = (self.peerId, self.callId, self.video, self.core)
                Task { try? await core.sendCallSignal(to: uid, CallSignal(callId: cid, action: .ice, video: v, sdp: nil, candidates: batch)) }
            }
        }
    }

    fileprivate func onIceState(_ s: RTCIceConnectionState) {
        switch s {
        case .connected, .completed:
            reconnectWork?.cancel()
            reconnectWork = nil
            restarting = false
            if reconnecting { reconnecting = false }
            if phase == .connecting || phase == .outgoing {
                timeoutWork?.cancel()
                phase = .active
                if connectedAt == nil { connectedAt = Date() }
            }
        case .disconnected, .failed:
            // B7：先提示并尝试恢复（主叫做 ICE restart），期限内恢复不了再结束
            guard isLive, phase == .active || phase == .connecting else { return }
            if Self.shouldShowReconnecting(phase: phase) && !reconnecting { reconnecting = true }
            restartIce()
            if reconnectWork == nil {
                let w = DispatchWorkItem { [weak self] in
                    MainActor.assumeIsolated {
                        guard let self, self.isLive, self.reconnectWork != nil else { return }
                        self.reconnectWork = nil
                        self.send(.hangup)
                        self.end("网络连接失败")
                    }
                }
                reconnectWork = w
                DispatchQueue.main.asyncAfter(deadline: .now() + reconnectTimeout, execute: w)
            }
        default: break
        }
    }

    fileprivate func onRemoteTrack(_ t: RTCMediaStreamTrack) {
        // didAdd stream 与 didAdd rtpReceiver 可能对同一轨道各回调一次
        guard let v = t as? RTCVideoTrack, v !== remoteVideo else { return }
        if let old = remoteWatcher { remoteVideo?.remove(old) }
        remoteVideo = v
        let watcher = VideoFrameWatcher()
        v.add(watcher)
        remoteWatcher = watcher
        watchTimer?.invalidate()
        watchTimer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self, let w = self.remoteWatcher else { return }
                let has = w.hasRecentFrames()
                if has != self.remoteHasVideo { self.remoteHasVideo = has }
            }
        }
    }

    /// 只有已经接通过的通话才提示「正在重连」；还在建立中的显示「正在连接…」
    nonisolated static func shouldShowReconnecting(phase: CallPhase) -> Bool { phase == .active }

    private func armTimeout(_ seconds: TimeInterval, _ run: @escaping () -> Void) {
        timeoutWork?.cancel()
        let w = DispatchWorkItem { MainActor.assumeIsolated { run() } }
        timeoutWork = w
        DispatchQueue.main.asyncAfter(deadline: .now() + seconds, execute: w)
    }

    /// B7：接听（或对方接听）后限时建立媒体连接，超时结束，不会一直停在「正在连接…」
    private func armConnectTimeout() {
        armTimeout(connectTimeout) { [weak self] in
            guard let self, self.phase == .connecting else { return }
            self.send(.hangup)
            self.end("连接超时")
        }
    }

    private func end(_ reason: String) {
        _ = beginAttempt()   // 让仍在 await 中的建立流程失效
        timeoutWork?.cancel()
        reconnectWork?.cancel()
        reconnectWork = nil
        restarting = false
        offerSent = false
        NotificationService.shared.withdrawIncomingCall(callId: callId)
        releaseMedia()
        remoteOfferSdp = nil
        interrupted = false
        let s = RTCAudioSession.sharedInstance()
        s.lockForConfiguration()
        try? s.setActive(false)
        s.unlockForConfiguration()
        // 通知其他 App（音乐等）可以恢复播放
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        phase = .ended(reason)
        let cid = callId
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { [weak self] in
            MainActor.assumeIsolated {
                guard let self, self.callId == cid, case .ended = self.phase else { return }
                self.phase = .idle
            }
        }
    }

    /// 关闭 PeerConnection、停止采集并释放所有本地/远端轨道
    private func releaseMedia() {
        capturer?.stopCapture()
        capturer = nil
        synthetic?.stop()
        synthetic = nil
        watchTimer?.invalidate()
        watchTimer = nil
        if let w = remoteWatcher { remoteVideo?.remove(w) }
        remoteWatcher = nil
        videoSender = nil
        remoteHasVideo = false
        pc?.close()
        pc = nil
        pcDelegate = nil
        localAudio = nil
        localVideo = nil
        remoteVideo = nil
    }

    // MARK: - 设备状态（防锁屏 / 距离传感器）与音频打断

    struct DeviceState: Equatable {
        var idleTimerDisabled: Bool
        var proximity: Bool
    }

    /// 通话期间禁止自动锁屏；语音通话且未开扬声器时（贴耳）打开距离传感器，防止脸误触
    static func deviceState(phase: CallPhase, video: Bool, speaker: Bool) -> DeviceState {
        switch phase {
        case .idle, .ended: return DeviceState(idleTimerDisabled: false, proximity: false)
        case .incoming: return DeviceState(idleTimerDisabled: true, proximity: false)
        case .outgoing, .connecting, .active: return DeviceState(idleTimerDisabled: true, proximity: !video && !speaker)
        }
    }

    private func applyDeviceState() {
        let st = Self.deviceState(phase: phase, video: video, speaker: speaker)
        if UIApplication.shared.isIdleTimerDisabled != st.idleTimerDisabled {
            UIApplication.shared.isIdleTimerDisabled = st.idleTimerDisabled
        }
        if UIDevice.current.isProximityMonitoringEnabled != st.proximity {
            UIDevice.current.isProximityMonitoringEnabled = st.proximity
        }
    }

    private func onAudioInterruption(_ type: AVAudioSession.InterruptionType?) {
        guard isLive, let type else { return }
        switch type {
        case .began:
            interrupted = true
            localAudio?.isEnabled = false
        case .ended:
            interrupted = false
            let s = RTCAudioSession.sharedInstance()
            s.lockForConfiguration()
            try? s.setActive(true)
            s.unlockForConfiguration()
            applyAudioRoute()
            localAudio?.isEnabled = !muted
        @unknown default: break
        }
    }
}

/// PeerConnection 回调在 WebRTC 线程，转发到主线程
private final class PeerDelegate: NSObject, RTCPeerConnectionDelegate, @unchecked Sendable {
    weak var owner: CallManager?
    init(owner: CallManager) { self.owner = owner }

    /// WebRTC 对象不是 Sendable，但回调后只在主线程使用，用 Unchecked 包装跨线程传递
    private func main<T>(_ value: T, _ f: @escaping @MainActor @Sendable (CallManager, T) -> Void) {
        let box = Unchecked(value)
        DispatchQueue.main.async { [weak self] in
            MainActor.assumeIsolated { if let o = self?.owner { f(o, box.value) } }
        }
    }

    func peerConnection(_ peerConnection: RTCPeerConnection, didChange stateChanged: RTCSignalingState) {}
    func peerConnection(_ peerConnection: RTCPeerConnection, didAdd stream: RTCMediaStream) {
        if let v = stream.videoTracks.first { main(v) { $0.onRemoteTrack($1) } }
    }
    func peerConnection(_ peerConnection: RTCPeerConnection, didRemove stream: RTCMediaStream) {}
    func peerConnectionShouldNegotiate(_ peerConnection: RTCPeerConnection) {}
    func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCIceConnectionState) {
        main(newState) { $0.onIceState($1) }
    }
    func peerConnection(_ peerConnection: RTCPeerConnection, didChange newState: RTCIceGatheringState) {}
    func peerConnection(_ peerConnection: RTCPeerConnection, didGenerate candidate: RTCIceCandidate) {
        main(candidate) { $0.onLocalCandidate($1) }
    }
    func peerConnection(_ peerConnection: RTCPeerConnection, didRemove candidates: [RTCIceCandidate]) {}
    func peerConnection(_ peerConnection: RTCPeerConnection, didOpen dataChannel: RTCDataChannel) {}
    func peerConnection(_ peerConnection: RTCPeerConnection, didAdd rtpReceiver: RTCRtpReceiver, streams mediaStreams: [RTCMediaStream]) {
        if let t = rtpReceiver.track { main(t) { $0.onRemoteTrack($1) } }
    }
}

private struct Unchecked<T>: @unchecked Sendable {
    let value: T
    init(_ value: T) { self.value = value }
}
