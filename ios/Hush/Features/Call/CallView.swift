import SwiftUI
import WebRTC

/// 全屏通话页：来电 / 呼叫中 / 通话中 / 结束提示
struct CallView: View {
    var calls = CallManager.shared
    var core = HushCore.shared

    var body: some View {
        if let block = calls.blocked {
            CallBlockedView(block: block)
        } else {
            callBody
        }
    }

    private var callBody: some View {
        ZStack {
            background
            if showRemoteVideo { scrims }
            VStack(spacing: 0) {
                if !showRemoteVideo {
                    Spacer().frame(height: 90)
                    UserAvatar(userId: calls.peerId, size: 110)
                        .shadow(radius: 10)
                    Text(calls.peerName).font(Fonts.sized(28, weight: .semibold)).foregroundStyle(.white)
                        .padding(.top, 20)
                        .accessibilityIdentifier("callPeerName")
                } else {
                    Text(calls.peerName).font(Fonts.sized(20, weight: .semibold)).foregroundStyle(.white)
                        .legibleOnVideo().padding(.top, 60)
                        .accessibilityIdentifier("callPeerName")
                }
                statusText
                    .font(Fonts.sized(17)).foregroundStyle(.white.opacity(0.9))
                    .legibleOnVideo()
                    .padding(.top, 8)
                    .accessibilityIdentifier("callStatus")
                if calls.reconnecting {
                    Label("网络不稳定，正在重连…", systemImage: "wifi.exclamationmark")
                        .font(Fonts.sized(15)).foregroundStyle(.white.opacity(0.9))
                        .legibleOnVideo()
                        .padding(.top, 14)
                        .accessibilityIdentifier("callReconnecting")
                }
                if calls.interrupted {
                    Label("通话被系统打断，麦克风已暂停", systemImage: "pause.circle")
                        .font(Fonts.sized(15)).foregroundStyle(.white.opacity(0.85))
                        .legibleOnVideo()
                        .padding(.top, 14)
                        .accessibilityIdentifier("callInterrupted")
                }
                if calls.video && calls.phase == .active && !calls.remoteHasVideo {
                    Label("对方未开启摄像头", systemImage: "video.slash")
                        .font(Fonts.sized(15)).foregroundStyle(.white.opacity(0.7))
                        .padding(.top, 14)
                        .accessibilityIdentifier("remoteNoVideo")
                }
                Spacer()
                controls.padding(.bottom, 60)
            }
            if let local = calls.localVideo, calls.phase != .incoming {
                VStack {
                    HStack {
                        Spacer()
                        localPreview(local)
                            .frame(width: 110, height: 160)
                            .clipShape(RoundedRectangle(cornerRadius: 12))
                            .overlay(RoundedRectangle(cornerRadius: 12).stroke(.white.opacity(0.4)))
                            .padding(.trailing, 16).padding(.top, 60)
                    }
                    Spacer()
                }
            }
            if calls.isLive && calls.phase != .incoming {
                // 最小化：回到聊天继续操作，通话以悬浮条保留
                VStack {
                    HStack {
                        Button { calls.minimized = true } label: {
                            Image(systemName: "chevron.down").font(.system(size: 18, weight: .semibold))
                                .foregroundStyle(.white).frame(width: 44, height: 44)
                                .background(Circle().fill(Color.white.opacity(0.18)))
                        }
                        .accessibilityLabel("最小化通话")
                        .accessibilityIdentifier("minimizeCall")
                        .padding(.leading, 16).padding(.top, 56)
                        Spacer()
                    }
                    Spacer()
                }
            }
        }
        .ignoresSafeArea()
        .onAppear { hideKeyboard() }
        .onChange(of: calls.phase) { _, _ in hideKeyboard() }
    }

    /// 远端画面上方/下方的渐变遮罩，保证白色文字在明亮画面上仍可读（ui-guide §2.8）
    private var scrims: some View {
        VStack(spacing: 0) {
            LinearGradient(colors: [.black.opacity(0.55), .clear], startPoint: .top, endPoint: .bottom)
                .frame(height: 180)
            Spacer()
            LinearGradient(colors: [.clear, .black.opacity(0.6)], startPoint: .top, endPoint: .bottom)
                .frame(height: 260)
        }
        .ignoresSafeArea()
        .allowsHitTesting(false)
    }

    private var showRemoteVideo: Bool {
        calls.video && calls.remoteVideo != nil && calls.phase == .active && calls.remoteHasVideo
    }

    /// 本地小窗：摄像头关闭时显示占位；合成画面时标注“模拟摄像头”
    @ViewBuilder private func localPreview(_ track: RTCVideoTrack) -> some View {
        if calls.cameraOff {
            ZStack {
                Color.black.opacity(0.55)
                VStack(spacing: 6) {
                    Image(systemName: "video.slash.fill").font(.system(size: 22))
                    Text("摄像头已关闭").font(Fonts.sized(12))
                }.foregroundStyle(.white.opacity(0.8))
            }
        } else {
            RTCVideoView(track: track, mirror: calls.frontCamera && !calls.syntheticCamera)
                .overlay(alignment: .bottom) {
                    if calls.syntheticCamera {
                        Text("模拟摄像头").font(Fonts.sized(11, weight: .medium)).foregroundStyle(.white)
                            .padding(.horizontal, 6).padding(.vertical, 2)
                            .background(Capsule().fill(.black.opacity(0.45)))
                            .padding(.bottom, 6)
                    }
                }
        }
    }

    @ViewBuilder private var background: some View {
        if showRemoteVideo, let remote = calls.remoteVideo {
            RTCVideoView(track: remote, mirror: false).ignoresSafeArea()
        } else {
            LinearGradient(colors: [Color(rgb: 0x1B2B4B), Color(rgb: 0x0E1422)], startPoint: .top, endPoint: .bottom)
                .ignoresSafeArea()
        }
    }

    @ViewBuilder private var statusText: some View {
        switch calls.phase {
        case .incoming: Text(calls.video ? "视频来电" : "语音来电")
        case .outgoing: Text("正在呼叫…")
        case .connecting: Text("正在连接…")
        case .active:
            TimelineView(.periodic(from: .now, by: 1)) { ctx in
                Text(Fmt.duration(Int(ctx.date.timeIntervalSince(calls.connectedAt ?? ctx.date))))
                    .monospacedDigit()
            }
        case let .ended(reason): Text(reason)
        case .idle: Text("")
        }
    }

    @ViewBuilder private var controls: some View {
        switch calls.phase {
        case .incoming:
            HStack(spacing: 90) {
                CallButton(icon: "phone.down.fill", label: "拒绝", color: Theme.danger, id: "rejectCall") { calls.reject() }
                CallButton(icon: calls.video ? "video.fill" : "phone.fill", label: "接听", color: Theme.success, id: "acceptCall") { calls.accept() }
            }
        case .ended:
            EmptyView()
        default:
            VStack(spacing: 30) {
                HStack(spacing: 34) {
                    CallButton(icon: calls.muted ? "mic.slash.fill" : "mic.fill", label: calls.muted ? "已静音" : "静音",
                               color: calls.muted ? .white : .white.opacity(0.2), fg: calls.muted ? .black : .white, id: "muteCall") { calls.toggleMute() }
                    CallButton(icon: "speaker.wave.3.fill", label: "扬声器",
                               color: calls.speaker ? .white : .white.opacity(0.2), fg: calls.speaker ? .black : .white, id: "speakerCall") { calls.toggleSpeaker() }
                    if calls.video {
                        CallButton(icon: calls.cameraOff ? "video.slash.fill" : "video.fill", label: calls.cameraOff ? "摄像头已关" : "摄像头",
                                   color: calls.cameraOff ? .white : .white.opacity(0.2), fg: calls.cameraOff ? .black : .white, id: "cameraCall") { calls.toggleCamera() }
                        CallButton(icon: "arrow.triangle.2.circlepath.camera.fill", label: "翻转",
                                   color: .white.opacity(0.2), id: "flipCall") { calls.switchCamera() }
                    }
                }
                CallButton(icon: "phone.down.fill", label: "挂断", color: Theme.danger, id: "hangupCall") { calls.hangup() }
            }
        }
    }
}

struct CallButton: View {
    let icon: String
    let label: String
    let color: Color
    var fg: Color = .white
    let id: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            VStack(spacing: 8) {
                Image(systemName: icon)
                    .font(.system(size: 26))
                    .foregroundStyle(fg)
                    .frame(width: 68, height: 68)
                    .background(Circle().fill(color))
                Text(label).font(Fonts.sized(13)).foregroundStyle(.white)
                    .legibleOnVideo()
            }
        }
        .accessibilityIdentifier(id)
    }
}

/// 通话最小化后的悬浮条（顶层窗口缩到这么大）：点击回到通话页
struct CallPill: View {
    var calls = CallManager.shared

    var body: some View {
        Button { calls.minimized = false } label: {
            HStack(spacing: 6) {
                Image(systemName: calls.video ? "video.fill" : "phone.fill").font(.system(size: 14, weight: .semibold))
                label.font(.system(size: 14, weight: .semibold)).monospacedDigit().lineLimit(1)
            }
            .foregroundStyle(.white)
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Capsule().fill(Theme.success))
        }
        .buttonStyle(.plain)
        .ignoresSafeArea()
        .accessibilityLabel("返回通话")
        .accessibilityIdentifier("callPill")
    }

    @ViewBuilder private var label: some View {
        switch calls.phase {
        case .active:
            if calls.reconnecting { Text("正在重连…") } else {
                TimelineView(.periodic(from: .now, by: 1)) { ctx in
                    Text("通话中 " + Fmt.duration(Int(ctx.date.timeIntervalSince(calls.connectedAt ?? ctx.date))))
                }
            }
        case .outgoing: Text("正在呼叫…")
        case .connecting: Text("正在连接…")
        case let .ended(reason): Text(reason)
        default: Text("通话")
        }
    }
}

/// A4：麦克风 / 摄像头权限被拒绝，通话已中止——说明原因并引导去系统设置
struct CallBlockedView: View {
    let block: CallBlock
    var calls = CallManager.shared

    var body: some View {
        ZStack {
            LinearGradient(colors: [Color(rgb: 0x1B2B4B), Color(rgb: 0x0E1422)], startPoint: .top, endPoint: .bottom)
                .ignoresSafeArea()
            VStack(spacing: 18) {
                Spacer()
                Image(systemName: block == .microphone ? "mic.slash.fill" : "video.slash.fill")
                    .font(.system(size: 52)).foregroundStyle(.white)
                Text(block.title).font(Fonts.sized(24, weight: .semibold)).foregroundStyle(.white)
                    .accessibilityIdentifier("callBlockedTitle")
                Text(block.message).font(Fonts.sized(16)).foregroundStyle(.white.opacity(0.85))
                    .multilineTextAlignment(.center).padding(.horizontal, 36)
                Spacer()
                Button("去设置") {
                    calls.blocked = nil
                    if let url = URL(string: UIApplication.openSettingsURLString) { UIApplication.shared.open(url) }
                }
                .buttonStyle(PrimaryButtonStyle())
                .accessibilityIdentifier("callOpenSettings")
                Button("关闭") { calls.blocked = nil }
                    .foregroundStyle(.white.opacity(0.9))
                    .frame(minHeight: 44)
                    .accessibilityIdentifier("callBlockedClose")
            }
            .padding(.horizontal, 32).padding(.bottom, 48)
        }
    }
}

/// WebRTC 视频渲染
struct RTCVideoView: UIViewRepresentable {
    let track: RTCVideoTrack
    let mirror: Bool

    func makeUIView(context: Context) -> RTCMTLVideoView {
        let v = RTCMTLVideoView()
        v.videoContentMode = .scaleAspectFill
        v.transform = mirror ? CGAffineTransform(scaleX: -1, y: 1) : .identity
        track.add(v)
        context.coordinator.track = track
        return v
    }

    func updateUIView(_ v: RTCMTLVideoView, context: Context) {
        if context.coordinator.track !== track {
            context.coordinator.track?.remove(v)
            track.add(v)
            context.coordinator.track = track
        }
        v.transform = mirror ? CGAffineTransform(scaleX: -1, y: 1) : .identity
    }

    static func dismantleUIView(_ v: RTCMTLVideoView, coordinator: Coordinator) {
        coordinator.track?.remove(v)
    }

    func makeCoordinator() -> Coordinator { Coordinator() }
    final class Coordinator { var track: RTCVideoTrack? }
}

extension View {
    /// 叠加在视频上的白色文字加深色阴影
    func legibleOnVideo() -> some View {
        shadow(color: .black.opacity(0.6), radius: 4, x: 0, y: 1)
    }
}
