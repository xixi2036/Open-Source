import SwiftUI

/// 根视图：按登录阶段切换；叠加屏幕锁、后台遮罩、通话页、toast
struct RootView: View {
    @Environment(\.scenePhase) private var scenePhase
    var core = HushCore.shared
    var lock = ScreenLock.shared
    var calls = CallManager.shared

    var body: some View {
        ZStack {
            switch core.phase {
            case .launching:
                Theme.background.ignoresSafeArea()
            case .loggedOut:
                AuthFlowView()
            case .needsProfile:
                ProfileSetupView()
            case .ready:
                MainTabView()
            case let .failed(msg, keyMismatch):
                StartupFailedView(message: msg, keyMismatch: keyMismatch)
            }
            if calls.isPresented && core.phase == .ready {
                CallView()
                    .transition(.move(edge: .bottom))
                    .zIndex(1)
            }
            // 屏幕锁与后台遮罩必须盖住通话页：否则应用切换器快照会露出对方画面和昵称。
            // 设计决定：屏幕锁也覆盖通话页，来电时需先解锁再接听。
            if lock.locked && core.phase == .ready {
                LockScreenView().zIndex(2)
            } else if lock.privacyScreen && scenePhase != .active {
                PrivacyCoverView().zIndex(2)
            }
        }
        .tint(Theme.accent)
        .withToast()
    }
}

/// 启动失败：可重试；数据库密钥丢失（如恢复备份到新设备）时可重置本地数据后重新登录
struct StartupFailedView: View {
    let message: String
    /// 库密钥不匹配：重试没有意义，只提供重置
    var keyMismatch = false
    var core = HushCore.shared
    @State private var confirmReset = false

    var body: some View {
        VStack(spacing: 16) {
            EmptyStateView(systemImage: keyMismatch ? "lock.trianglebadge.exclamationmark" : "exclamationmark.triangle",
                           text: keyMismatch ? "\(message)。需要重置本地数据后重新登录，服务器上的好友关系不受影响。" : "启动失败：\(message)")
            if !keyMismatch {
                Button("重试") { core.retryBootstrap() }
                    .buttonStyle(PrimaryButtonStyle())
                    .frame(width: 220)
                    .accessibilityIdentifier("retryBootstrap")
            }
            Button("重置本地数据并重新登录", role: .destructive) { confirmReset = true }
                .foregroundStyle(Theme.danger)
                .accessibilityIdentifier("resetLocalData")
        }
        .padding(.horizontal, 24)
        .confirmationDialog("将删除本机上的全部聊天记录和密钥，然后重新登录。此操作无法撤销。",
                            isPresented: $confirmReset, titleVisibility: .visible) {
            Button("删除并重新登录", role: .destructive) { core.resetLocalDataAndRestart() }
        }
    }
}

/// 切到后台时的模糊遮罩
struct PrivacyCoverView: View {
    var body: some View {
        ZStack {
            Rectangle().fill(.ultraThinMaterial)
            Image("Logo").resizable().frame(width: 80, height: 80)
                .clipShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
        }
        .ignoresSafeArea()
    }
}

struct LockScreenView: View {
    var lock = ScreenLock.shared
    var body: some View {
        ZStack {
            Rectangle().fill(.ultraThickMaterial)
            VStack(spacing: 20) {
                Image(systemName: "lock.fill").font(.system(size: 44)).foregroundStyle(Theme.accent)
                Text("Hush 已锁定").font(Fonts.title)
                Button("解锁") { lock.unlock() }
                    .buttonStyle(PrimaryButtonStyle())
                    .frame(width: 200)
                    .accessibilityIdentifier("unlockButton")
            }
        }
        .ignoresSafeArea()
        .onAppear { lock.unlock() }
    }
}
