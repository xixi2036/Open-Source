import SwiftUI

/// 根视图：按登录阶段切换。通话页、屏幕锁、后台遮罩、toast 不在这里，而在独立的高层级窗口里
/// （OverlayWindows），这样它们能盖住任何 sheet / fullScreenCover。
struct RootView: View {
    var core = HushCore.shared

    var body: some View {
        ZStack {
            switch core.phase {
            case .launching:
                LaunchingView()
            case .loggedOut:
                AuthFlowView()
            case .needsProfile:
                ProfileSetupView()
            case .ready:
                MainTabView()
            case let .failed(msg, keyMismatch):
                StartupFailedView(message: msg, keyMismatch: keyMismatch)
            }
        }
        .tint(Theme.accent)
    }
}

/// 启动页：数据库在后台线程打开 / 迁移（B1），期间显示进度而不是白屏
struct LaunchingView: View {
    /// 很快完成时不闪一下文字：0.4 秒后才显示
    @State private var showProgress = false
    var body: some View {
        ZStack {
            Theme.background.ignoresSafeArea()
            VStack(spacing: 18) {
                Image("Logo").resizable().frame(width: 84, height: 84)
                    .clipShape(RoundedRectangle(cornerRadius: 20, style: .continuous))
                if showProgress {
                    ProgressView()
                    Text("正在准备数据…").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                        .accessibilityIdentifier("launchingText")
                }
            }
        }
        .task {
            try? await Task.sleep(for: .milliseconds(400))
            showProgress = true
        }
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
                Button("解锁") { lock.unlockManually() }
                    .buttonStyle(PrimaryButtonStyle())
                    .frame(width: 200)
                    .accessibilityIdentifier("unlockButton")
            }
        }
        .ignoresSafeArea()
        .onAppear { hideKeyboard(); lock.autoUnlock() }
    }
}
