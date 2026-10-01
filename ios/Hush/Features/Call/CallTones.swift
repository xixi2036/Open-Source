import AVFoundation
import AudioToolbox

/// 通话提示音：前台来电铃声（带振动）与去电回铃音。
/// 音频在运行时合成（正弦波写成内存中的 WAV），不携带任何第三方铃声素材。
@MainActor
final class CallTones {
    enum Tone: Equatable {
        /// 来电铃声（响 1 秒停 2 秒）+ 振动
        case ring
        /// 去电回铃音（响 1 秒停 4 秒）
        case ringback
    }

    /// 纯逻辑：当前应该播放什么。
    /// 来电响铃阶段播铃声；去电在 offer 已发出（音频会话已按通话配置好）后播回铃音；接通或结束即停。
    nonisolated static func tone(phase: CallPhase, isCaller: Bool, offerSent: Bool) -> Tone? {
        switch phase {
        case .incoming: return .ring
        case .outgoing: return isCaller && offerSent ? .ringback : nil
        default: return nil
        }
    }

    private(set) var current: Tone?
    private var player: AVAudioPlayer?
    private var vibration: Timer?

    func apply(_ tone: Tone?) {
        guard tone != current else { return }
        stop()
        guard let tone else { return }
        current = tone
        // 单元测试宿主里只记录状态，不真的出声
        guard !AppEnvironment.isUnitTesting else { return }
        switch tone {
        case .ring:
            // soloAmbient：遵循静音开关（静音时只振动）；此时还没有接听，不占用通话音频会话
            let s = AVAudioSession.sharedInstance()
            try? s.setCategory(.soloAmbient, mode: .default)
            try? s.setActive(true)
            play(Self.ringWAV)
            AudioServicesPlaySystemSound(kSystemSoundID_Vibrate)
            let t = Timer(timeInterval: 3, repeats: true) { _ in AudioServicesPlaySystemSound(kSystemSoundID_Vibrate) }
            RunLoop.main.add(t, forMode: .common)
            vibration = t
        case .ringback:
            // 音频会话已由 CallManager 配成 playAndRecord / voiceChat，回铃音从当前通话输出（听筒或扬声器）播放
            play(Self.ringbackWAV)
        }
    }

    func stop() {
        vibration?.invalidate(); vibration = nil
        player?.stop(); player = nil
        if current == .ring, !AppEnvironment.isUnitTesting {
            // 铃声占用过音频会话：未接听就结束时让其他 App 的音乐恢复（接听时 CallManager 会重新配置）
            try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        }
        current = nil
    }

    private func play(_ data: Data) {
        guard let p = try? AVAudioPlayer(data: data) else { return }
        p.numberOfLoops = -1
        p.volume = 0.9
        p.prepareToPlay()
        p.play()
        player = p
    }

    // MARK: - 合成

    /// 来电铃声：两个音交替的 1 秒铃音 + 2 秒静音
    static let ringWAV: Data = wav(segments: [
        (freqs: [880, 1108.7], secs: 0.18), (freqs: [], secs: 0.07),
        (freqs: [880, 1108.7], secs: 0.18), (freqs: [], secs: 0.07),
        (freqs: [1174.7, 1480], secs: 0.18), (freqs: [], secs: 0.07),
        (freqs: [1174.7, 1480], secs: 0.25), (freqs: [], secs: 2.0),
    ])

    /// 回铃音：450 Hz 响 1 秒停 4 秒
    static let ringbackWAV: Data = wav(segments: [(freqs: [450], secs: 1.0), (freqs: [], secs: 4.0)], amplitude: 0.25)

    /// 把若干段（同时发声的频率 + 时长）合成为 16 位单声道 PCM WAV；每段首尾做 10ms 淡入淡出避免爆音
    nonisolated static func wav(segments: [(freqs: [Double], secs: Double)], sampleRate: Int = 22050, amplitude: Double = 0.35) -> Data {
        var samples: [Int16] = []
        let fade = Int(Double(sampleRate) * 0.01)
        for seg in segments {
            let n = Int(Double(sampleRate) * seg.secs)
            for i in 0..<n {
                guard !seg.freqs.isEmpty else { samples.append(0); continue }
                let t = Double(i) / Double(sampleRate)
                var v = seg.freqs.reduce(0.0) { $0 + sin(2 * .pi * $1 * t) } / Double(seg.freqs.count)
                let edge = min(i, n - 1 - i)
                if edge < fade { v *= Double(edge) / Double(fade) }
                samples.append(Int16(max(-1, min(1, v * amplitude)) * Double(Int16.max)))
            }
        }
        var d = Data()
        func le<T: FixedWidthInteger>(_ v: T) { withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) } }
        let dataSize = UInt32(samples.count * 2)
        d.append(contentsOf: Array("RIFF".utf8)); le(36 + dataSize)
        d.append(contentsOf: Array("WAVEfmt ".utf8)); le(UInt32(16)); le(UInt16(1)); le(UInt16(1))
        le(UInt32(sampleRate)); le(UInt32(sampleRate * 2)); le(UInt16(2)); le(UInt16(16))
        d.append(contentsOf: Array("data".utf8)); le(dataSize)
        d.reserveCapacity(d.count + samples.count * 2)
        for s in samples { le(s) }
        return d
    }
}
