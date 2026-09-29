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
                .onChange(of: scenePhase) { _, phase in
                    switch phase {
                    case .active:
                        core.onForeground()
                        lock.unlock()
                    case .background:
                        core.appActive = false
                        lock.onBackground()
                    default:
                        core.appActive = false
                    }
                }
        }
    }
}

enum AppEnvironment {
    /// 作为 XCTest 单元测试宿主运行（UI 测试不会向 App 进程注入该变量）
    static let isUnitTesting = ProcessInfo.processInfo.environment["XCTestConfigurationFilePath"] != nil
}
