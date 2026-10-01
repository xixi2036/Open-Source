import AVFoundation
import Observation
import UIKit

/// 录音用到的系统音频能力。正式运行用 SystemVoiceCapture；单元测试注入假实现，
/// 避免测试真的激活音频会话与麦克风（模拟器上激活音频会话偶尔会卡住十几秒以上）。
@MainActor
protocol VoiceCaptureBackend: AnyObject {
    /// 激活音频会话并开始录到 url，返回是否真的在录音
    func begin(url: URL) -> Bool
    /// 停止录音并释放音频会话
    func end()
}

@MainActor
final class SystemVoiceCapture: VoiceCaptureBackend {
    private var recorder: AVAudioRecorder?

    func begin(url: URL) -> Bool {
        let session = AVAudioSession.sharedInstance()
        do {
            try session.setCategory(.playAndRecord, mode: .default, options: [.defaultToSpeaker, .allowBluetoothHFP])
            try session.setActive(true)
        } catch {
            return false
        }
        let settings: [String: Any] = [
            AVFormatIDKey: kAudioFormatMPEG4AAC,
            AVSampleRateKey: 44100,
            AVNumberOfChannelsKey: 1,
            AVEncoderAudioQualityKey: AVAudioQuality.medium.rawValue,
        ]
        let r = try? AVAudioRecorder(url: url, settings: settings)
        guard let r, r.record() else { return false }
        recorder = r
        return true
    }

    func end() {
        recorder?.stop()
        recorder = nil
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
    }
}

/// 语音录制（m4a AAC）。按住说话的完整状态机：按下 → 检查麦克风权限 → 录音；
/// 松手发送 / 上滑取消；达到上限自动结束并通过 onAutoFinish 交出结果，松手前不会重新开始。
@MainActor @Observable
final class VoiceRecorder {
    var isRecording = false
    var elapsed: TimeInterval = 0
    var willCancel = false
    @ObservationIgnored var maxDuration: TimeInterval = 60
    /// 达到时长上限自动结束时回调（手指仍按着）
    @ObservationIgnored var onAutoFinish: ((URL, UInt64) -> Void)?
    /// 麦克风权限（可在测试中替换）
    @ObservationIgnored var permission: () -> AVAudioApplication.recordPermission = { AVAudioApplication.shared.recordPermission }
    /// 系统录音能力（可在测试中替换）
    @ObservationIgnored var backend: VoiceCaptureBackend = SystemVoiceCapture()
    /// 本次录音是否真的在录（模拟器没有麦克风时为 false，结束时改用静音文件）
    @ObservationIgnored private var capturing = false
    @ObservationIgnored private var timer: Timer?
    @ObservationIgnored private var url: URL?
    @ObservationIgnored private var started: Date?
    /// 当前这一次按压是否已处理过（开始录音 / 请求权限 / 自动结束），防止同一次按压里重复开始
    @ObservationIgnored private(set) var pressActive = false
    /// 正在录音的实例（来电 / 呼出时由 CallManager 取消）
    private static weak var active: VoiceRecorder?

    /// 通话开始时丢弃进行中的录音
    static func cancelActive() { active?.cancel() }

    // MARK: - 手势入口

    func pressChanged(translationY: CGFloat) {
        if !pressActive {
            pressActive = true
            begin()
        }
        if isRecording { willCancel = translationY < -60 }
    }

    /// 松手：返回要发送的录音；取消、太短或已自动结束时返回 nil
    func pressEnded() -> (URL, UInt64)? {
        defer { pressActive = false }
        guard isRecording else { return nil }
        return stop(cancel: willCancel)
    }

    /// 手势被系统打断（权限弹窗、来电）或页面消失：丢弃本次录音
    func cancel() {
        if isRecording { _ = stop(cancel: true) }
        pressActive = false
    }

    /// 手势状态已复位但没有收到 onEnded（手势被取消）
    func pressReset() {
        if pressActive { cancel() }
    }

    private func begin() {
        switch permission() {
        case .granted:
            start()
        case .undetermined:
            // 系统弹窗会打断本次按压；授权后下一次按住即可录音
            AVAudioApplication.requestRecordPermission { _ in }
        default:
            HushCore.shared.toast = "请在「设置」中允许 Hush 使用麦克风"
        }
    }

    func start(now: Date = Date()) {
        guard !isRecording else { return }
        // 录音与语音播放互斥
        VoicePlayer.shared.stop()
        Self.active = self
        let url = AppPaths.newOutboxFile(ext: "m4a")
        let ok = backend.begin(url: url)
        #if !targetEnvironment(simulator)
        guard ok else {
            HushCore.shared.toast = "无法录音，请检查麦克风"
            backend.end()
            return
        }
        #endif
        // 模拟器可能没有可用麦克风：录音失败时仍保持录音 UI，结束时生成静音文件，保证发送链路可验证（仅模拟器）
        capturing = ok
        self.url = url
        started = now
        elapsed = 0
        willCancel = false
        isRecording = true
        let t = Timer(timeInterval: 0.1, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.tick() }
        }
        // 按住手势期间 RunLoop 处于 tracking 模式，需加入 common 模式
        RunLoop.main.add(t, forMode: .common)
        timer = t
    }

    func tick(now: Date = Date()) {
        guard isRecording, let s = started else { return }
        elapsed = now.timeIntervalSince(s)
        if elapsed >= maxDuration, let result = stop(now: now) {
            onAutoFinish?(result.0, result.1)
        }
    }

    /// 结束录音，返回文件与时长；取消或太短返回 nil
    func stop(cancel: Bool = false, now: Date = Date()) -> (URL, UInt64)? {
        guard isRecording else { return nil }
        timer?.invalidate(); timer = nil
        isRecording = false
        let hadRecorder = capturing
        capturing = false
        backend.end()
        let duration = min(started.map { now.timeIntervalSince($0) } ?? elapsed, maxDuration)
        guard let url else { return nil }
        self.url = nil
        if cancel || duration < 1 {
            try? FileManager.default.removeItem(at: url)
            if !cancel && duration < 1 { HushCore.shared.toast = "说话时间太短" }
            return nil
        }
        #if targetEnvironment(simulator)
        if !hadRecorder || !FileManager.default.fileExists(atPath: url.path) {
            SilentAudio.write(to: url, seconds: duration)
        }
        #else
        _ = hadRecorder
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        #endif
        return (url, UInt64(duration * 1000))
    }
}

/// 生成一段静音 AAC 文件（仅模拟器在麦克风不可用时兜底）
enum SilentAudio {
    static func write(to url: URL, seconds: TimeInterval) {
        guard let format = AVAudioFormat(standardFormatWithSampleRate: 44100, channels: 1),
              let file = try? AVAudioFile(forWriting: url, settings: [
                  AVFormatIDKey: kAudioFormatMPEG4AAC, AVSampleRateKey: 44100, AVNumberOfChannelsKey: 1,
              ]) else { return }
        let frames = AVAudioFrameCount(44100 * seconds)
        guard let buf = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: frames) else { return }
        buf.frameLength = frames
        try? file.write(from: buf)
    }
}

/// 语音播放（同一时间只播放一条）。
/// 默认外放；播放期间打开距离传感器，手机贴近耳朵时改从听筒播放（B7 听筒模式），拿开后恢复外放。
/// 播放结束释放音频会话并通知其他 App（被打断的音乐可以恢复）。
@MainActor @Observable
final class VoicePlayer: NSObject, AVAudioPlayerDelegate {
    static let shared = VoicePlayer()
    var playingId: String?
    var progress: Double = 0
    /// 当前是否从听筒播放
    private(set) var earpiece = false
    @ObservationIgnored private var player: AVAudioPlayer?
    @ObservationIgnored private var timer: Timer?
    @ObservationIgnored private var proximityObserver: NSObjectProtocol?

    func toggle(id: String, path: String) {
        if playingId == id { stop(); return }
        stop()
        guard !CallManager.shared.isLive else {
            HushCore.shared.toast = "通话中无法播放语音"
            return
        }
        do {
            try applyRoute(earpiece: false)
            try AVAudioSession.sharedInstance().setActive(true)
            let p = try AVAudioPlayer(contentsOf: URL(fileURLWithPath: path))
            p.delegate = self
            p.play()
            player = p
            playingId = id
            progress = 0
            let t = Timer(timeInterval: 0.05, repeats: true) { [weak self] _ in
                MainActor.assumeIsolated {
                    guard let self, let p = self.player, p.duration > 0 else { return }
                    self.progress = p.currentTime / p.duration
                }
            }
            RunLoop.main.add(t, forMode: .common)
            timer = t
            startProximity()
        } catch {
            HushCore.shared.toast = "无法播放语音"
        }
    }

    func stop() {
        let wasPlaying = player != nil
        timer?.invalidate(); timer = nil
        player?.stop(); player = nil
        playingId = nil
        progress = 0
        stopProximity()
        if wasPlaying && !CallManager.shared.isLive {
            // 释放音频会话，让被打断的其他 App 恢复播放
            try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        }
    }

    /// 音频会话的类别：外放用 playback；听筒用 playAndRecord（不带 defaultToSpeaker 时输出到听筒）
    nonisolated static func category(earpiece: Bool) -> AVAudioSession.Category { earpiece ? .playAndRecord : .playback }

    private func applyRoute(earpiece: Bool) throws {
        let s = AVAudioSession.sharedInstance()
        try s.setCategory(Self.category(earpiece: earpiece), mode: earpiece ? .voiceChat : .default,
                          options: earpiece ? [.allowBluetoothHFP] : [])
        self.earpiece = earpiece
    }

    private func startProximity() {
        UIDevice.current.isProximityMonitoringEnabled = true
        proximityObserver = NotificationCenter.default.addObserver(
            forName: UIDevice.proximityStateDidChangeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated {
                guard let self, self.player != nil else { return }
                try? self.applyRoute(earpiece: UIDevice.current.proximityState)
            }
        }
    }

    private func stopProximity() {
        if let o = proximityObserver { NotificationCenter.default.removeObserver(o) }
        proximityObserver = nil
        earpiece = false
        // 通话中的距离传感器由 CallManager 管理，这里不去关它
        if !CallManager.shared.isLive, UIDevice.current.isProximityMonitoringEnabled {
            UIDevice.current.isProximityMonitoringEnabled = false
        }
    }

    nonisolated func audioPlayerDidFinishPlaying(_ player: AVAudioPlayer, successfully flag: Bool) {
        DispatchQueue.main.async { MainActor.assumeIsolated { self.stop() } }
    }
}
