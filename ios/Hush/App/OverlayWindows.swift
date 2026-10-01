import SwiftUI
import UIKit
import Observation

/// 顶层窗口里显示什么（优先级从高到低）
enum OverlayMode: Equatable {
    case none
    /// 屏幕锁
    case lock
    /// 后台 / 应用切换器里的模糊遮罩
    case privacy
    /// 全屏通话页
    case call
    /// 通话最小化后的悬浮条
    case pill
}

/// A1 / A7：通话页、屏幕锁、后台遮罩、toast 放在独立的高层级 UIWindow 里。
/// 主窗口里的 sheet / fullScreenCover / 系统选择器都在主窗口内呈现，盖不住这里的内容。
///
/// 不使用「全屏透明 + 命中测试穿透」的窗口（SwiftUI 宿主视图的命中测试在不同系统版本上行为不一致，
/// 还会让 UI 测试认为下层控件不可点）：没有内容时窗口隐藏；悬浮条时窗口缩到悬浮条大小；
/// toast 窗口不接收触摸。两个窗口都不会成为 key window，不影响主窗口的键盘焦点。
@MainActor @Observable
final class OverlayWindows {
    static let shared = OverlayWindows()

    private(set) var mode: OverlayMode = .none
    /// toast 窗口里显示的文字（淡出动画期间保留上一条）
    private(set) var toastText: String?

    @ObservationIgnored private var window: UIWindow?
    @ObservationIgnored private var toastWindow: UIWindow?
    @ObservationIgnored private var toastHide: DispatchWorkItem?
    @ObservationIgnored private var tracking = false

    static let pillSize = CGSize(width: 148, height: 40)
    static let toastDuration: TimeInterval = 2.2

    /// 纯逻辑：由各项状态决定顶层窗口显示什么。
    /// 屏幕锁与遮罩必须盖住通话页：否则应用切换器快照会露出对方画面和昵称（来电时需先解锁再接听）。
    nonisolated static func mode(ready: Bool, locked: Bool, cover: Bool, callPresented: Bool, callMinimized: Bool) -> OverlayMode {
        if ready && locked { return .lock }
        if cover { return .privacy }
        if ready && callPresented { return callMinimized ? .pill : .call }
        return .none
    }

    /// 在当前窗口场景里创建两个顶层窗口（幂等；场景还没准备好时下次 refresh 再试）
    func install() {
        if window == nil, let scene = UIApplication.shared.connectedScenes.compactMap({ $0 as? UIWindowScene }).first {
            let w = UIWindow(windowScene: scene)
            w.windowLevel = .alert + 1
            w.backgroundColor = .clear
            let host = UIHostingController(rootView: OverlayRootView())
            host.view.backgroundColor = .clear
            w.rootViewController = host
            w.isHidden = true
            w.accessibilityIdentifier = "overlayWindow"
            window = w

            let t = UIWindow(windowScene: scene)
            t.windowLevel = .alert + 2
            t.backgroundColor = .clear
            t.isUserInteractionEnabled = false
            let toastHost = UIHostingController(rootView: ToastOverlayView())
            toastHost.view.backgroundColor = .clear
            t.rootViewController = toastHost
            t.isHidden = true
            toastWindow = t
        }
        if !tracking {
            tracking = true
            track()
        }
        apply()
    }

    /// 立即按当前状态更新窗口（scenePhase 变化时同步调用，保证遮罩进入应用切换器快照）
    func refresh() {
        install()
    }

    private func currentMode() -> OverlayMode {
        let core = HushCore.shared, lock = ScreenLock.shared, calls = CallManager.shared
        return Self.mode(ready: core.phase == .ready, locked: lock.locked, cover: lock.coverVisible,
                         callPresented: calls.isPresented, callMinimized: calls.minimized)
    }

    /// 观察相关状态；任何一项变化后在主线程重新计算
    private func track() {
        withObservationTracking {
            _ = currentMode()
            _ = HushCore.shared.toast
        } onChange: {
            DispatchQueue.main.async {
                MainActor.assumeIsolated {
                    OverlayWindows.shared.apply()
                    OverlayWindows.shared.track()
                }
            }
        }
    }

    private func apply() {
        applyMode(currentMode())
        applyToast(HushCore.shared.toast)
    }

    private func applyMode(_ new: OverlayMode) {
        guard let window, let scene = window.windowScene else { mode = new; return }
        let old = mode
        if old != new { mode = new }
        let screen = scene.screen.bounds
        switch new {
        case .none:
            if !window.isHidden { window.isHidden = true }
        case .pill:
            let top = (scene.windows.first { $0 !== window && $0 !== toastWindow }?.safeAreaInsets.top ?? 47)
            let size = Self.pillSize
            let frame = CGRect(x: screen.width - size.width - 10, y: top + 50, width: size.width, height: size.height)
            if window.frame != frame { window.frame = frame }
            if window.isHidden { window.isHidden = false }
        case .lock, .privacy, .call:
            if window.frame != screen { window.frame = screen }
            if window.isHidden {
                window.isHidden = false
                if new == .call {
                    // 通话页从底部滑入；锁与遮罩必须立刻盖住，不做动画
                    window.transform = CGAffineTransform(translationX: 0, y: screen.height)
                    UIView.animate(withDuration: 0.28, delay: 0, options: [.curveEaseOut, .allowUserInteraction]) {
                        window.transform = .identity
                    }
                }
            }
            if new != .call { window.layer.removeAllAnimations(); window.transform = .identity }
        }
        if old != new {
            window.rootViewController?.view.setNeedsLayout()
            window.layoutIfNeeded()
        }
    }

    private func applyToast(_ text: String?) {
        guard let toastWindow, let scene = toastWindow.windowScene else { return }
        guard let text else {
            if toastText != nil {
                toastText = nil
                toastHide?.cancel()
                toastWindow.isHidden = true
            }
            return
        }
        guard text != toastText || toastWindow.isHidden else { return }
        toastText = text
        let screen = scene.screen.bounds
        toastWindow.frame = CGRect(x: 0, y: 0, width: screen.width, height: 190)
        toastWindow.isHidden = false
        toastHide?.cancel()
        let work = DispatchWorkItem {
            MainActor.assumeIsolated {
                // 到时清除；期间如果换成了别的文字，由那一条自己的计时器负责
                if HushCore.shared.toast == text { HushCore.shared.toast = nil }
            }
        }
        toastHide = work
        DispatchQueue.main.asyncAfter(deadline: .now() + Self.toastDuration, execute: work)
    }
}

/// 顶层窗口的内容
struct OverlayRootView: View {
    var overlay = OverlayWindows.shared

    var body: some View {
        ZStack {
            switch overlay.mode {
            case .none: Color.clear
            case .lock: LockScreenView()
            case .privacy: PrivacyCoverView()
            case .call: CallView()
            case .pill: CallPill()
            }
        }
        .tint(Theme.accent)
    }
}

/// toast 窗口的内容（不接收触摸）
struct ToastOverlayView: View {
    var overlay = OverlayWindows.shared

    var body: some View {
        VStack {
            if let t = overlay.toastText {
                Text(t)
                    .font(Fonts.secondary)
                    .foregroundStyle(.white)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal, 16).padding(.vertical, 10)
                    .background(Capsule().fill(Color.black.opacity(0.8)))
                    .overlay(Capsule().stroke(Color.white.opacity(0.25), lineWidth: 0.5))   // 深色背景（图片查看器）上也看得出轮廓
                    .padding(.horizontal, 24)
                    .padding(.top, 60)
                    .transition(.opacity)
                    .accessibilityIdentifier("toast")
                    .id(t)
            }
            Spacer(minLength: 0)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .ignoresSafeArea()
        .animation(.easeInOut(duration: 0.2), value: overlay.toastText)
    }
}
