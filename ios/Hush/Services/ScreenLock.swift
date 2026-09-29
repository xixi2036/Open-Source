import Foundation
import LocalAuthentication
import Observation

/// 屏幕锁（LocalAuthentication）与后台模糊遮罩的开关与状态
@MainActor @Observable
final class ScreenLock {
    static let shared = ScreenLock()

    var lockEnabled: Bool = UserDefaults.standard.bool(forKey: "screenLock") {
        didSet { UserDefaults.standard.set(lockEnabled, forKey: "screenLock") }
    }
    var privacyScreen: Bool = UserDefaults.standard.object(forKey: "privacyScreen") as? Bool ?? true {
        didSet { UserDefaults.standard.set(privacyScreen, forKey: "privacyScreen") }
    }
    var locked = false
    private var authenticating = false

    init() { locked = lockEnabled }

    func onBackground() { if lockEnabled { locked = true } }

    func unlock() {
        guard locked, !authenticating else { return }
        authenticating = true
        let ctx = LAContext()
        var err: NSError?
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else {
            authenticating = false
            switch Self.actionWhenPolicyUnavailable(err.map { LAError.Code(rawValue: $0.code) } ?? nil, simulator: Self.isSimulator) {
            case .unlock:
                locked = false
            case .disableLock:
                // 开启屏幕锁后设备密码被关闭：无法再验证，关闭屏幕锁并说明原因（界面开关同步变为关）
                lockEnabled = false
                locked = false
                HushCore.shared.toast = "设备已关闭密码，屏幕锁已自动关闭"
            case .stayLocked:
                break   // 其他错误保持锁定，可点「解锁」重试
            }
            return
        }
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: "解锁 Hush") { ok, _ in
            DispatchQueue.main.async {
                MainActor.assumeIsolated {
                    if ok { self.locked = false }
                    self.authenticating = false
                }
            }
        }
    }

    enum UnavailableAction: Equatable { case unlock, disableLock, stayLocked }

    static var isSimulator: Bool {
        #if targetEnvironment(simulator)
        return true
        #else
        return false
        #endif
    }

    /// 无法进行身份验证时怎么处理：模拟器直接放行；真机仅在「未设置密码」时关闭屏幕锁，其余情况保持锁定
    nonisolated static func actionWhenPolicyUnavailable(_ code: LAError.Code?, simulator: Bool) -> UnavailableAction {
        if simulator { return .unlock }
        return code == .passcodeNotSet ? .disableLock : .stayLocked
    }

    /// 开启屏幕锁前先验证一次
    func enable(_ on: Bool) {
        guard on else { lockEnabled = false; return }
        let ctx = LAContext()
        var err: NSError?
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else {
            HushCore.shared.toast = "设备未设置密码，无法开启屏幕锁"
            return
        }
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: "开启屏幕锁") { ok, _ in
            DispatchQueue.main.async { MainActor.assumeIsolated { if ok { self.lockEnabled = true } } }
        }
    }
}
