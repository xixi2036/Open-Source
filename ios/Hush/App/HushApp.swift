import SwiftUI

@main
struct HushApp: App {
    @Environment(\.scenePhase) private var scenePhase
    var core = HushCore.shared
    var lock = ScreenLock.shared

    init() {
        // 单元测试以本 App 为宿主运行：不连接服务器、不打开本地数据库，避免测试影响已登录账号
        guard !AppEnvironment.isUnitTesting else { return }
        NotificationService.shared.setup()
        HushCore.shared.bootstrap()
    }

    var body: some Scene {
        WindowGroup {
            RootView()
                .onAppear { OverlayWindows.shared.install() }
                .onChange(of: scenePhase) { _, phase in
                    switch phase {
                    case .active:
                        lock.onForeground()
                        lock.sceneActive = true
                        core.onForeground()
                    case .background:
                        lock.sceneActive = false
                        core.onBackground()
                        lock.onBackground()
                    default:
                        // B3：.inactive（下拉通知中心、权限弹窗、Face ID 验证、应用切换器）不算进入后台：
                        // 不取消录音、不重跑同步、不改变通知抑制；只盖上遮罩，避免切换器快照露出内容
                        lock.sceneActive = false
                    }
                    // 遮罩必须在本次运行循环内就位（系统随后就会截取应用切换器快照）
                    OverlayWindows.shared.refresh()
                }
        }
    }
}

enum AppEnvironment {
    /// 作为 XCTest 单元测试宿主运行（UI 测试不会向 App 进程注入该变量）
    static let isUnitTesting = ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] != nil
}
