import Foundation
import LocalAuthentication
import Observation

/// 屏幕锁（LocalAuthentication）与后台模糊遮罩的开关与状态
@MainActor @Observable
final class ScreenLock {
    static let shared = ScreenLock()

    /// 宽限时间选项（秒）：离开 App 不超过这么久再回来不需要重新验证
    static let graceOptions: [(Int, String)] = [(0, "立即"), (60, "1 分钟"), (300, "5 分钟")]

    var lockEnabled: Bool = UserDefaults.standard.bool(forKey: "screenLock") {
        didSet { UserDefaults.standard.set(lockEnabled, forKey: "screenLock") }
    }
    var privacyScreen: Bool = UserDefaults.standard.object(forKey: "privacyScreen") as? Bool ?? true {
        didSet { UserDefaults.standard.set(privacyScreen, forKey: "privacyScreen") }
    }
    /// 屏幕锁宽限时间（秒）
    var graceSecs: Int = UserDefaults.standard.integer(forKey: "screenLockGrace") {
        didSet { UserDefaults.standard.set(graceSecs, forKey: "screenLockGrace") }
    }
    var locked = false
    /// 场景是否处于 active（由 HushApp 同步）；非 active 时显示遮罩，避免应用切换器快照露出内容
    var sceneActive = true
    /// 进入后台的时刻（宽限时间从这里算起）
    @ObservationIgnored private var backgroundedAt: Date?
    @ObservationIgnored private var authenticating = false
    /// B2：系统验证失败 / 被取消后，不再自动弹出验证（真机上取消 Face ID 会让场景重新 active，
    /// 自动重试会形成「取消 → 再弹」的循环）；只有用户点「解锁」或再次从后台回来才重新验证
    @ObservationIgnored private(set) var autoPromptSuppressed = false

    init() { locked = lockEnabled }

    /// 是否需要盖住内容：未解锁，或不在前台且开启了屏幕安全 / 屏幕锁（宽限期内尚未上锁时也不能让快照露出内容）
    var coverVisible: Bool { Self.coverVisible(sceneActive: sceneActive, privacyScreen: privacyScreen, lockEnabled: lockEnabled) }

    nonisolated static func coverVisible(sceneActive: Bool, privacyScreen: Bool, lockEnabled: Bool) -> Bool {
        !sceneActive && (privacyScreen || lockEnabled)
    }

    /// 离开了 elapsed 秒后回到前台是否需要重新验证
    nonisolated static func shouldLock(enabled: Bool, graceSecs: Int, elapsed: TimeInterval?) -> Bool {
        guard enabled else { return false }
        guard let elapsed else { return false }
        return graceSecs <= 0 || elapsed >= TimeInterval(graceSecs)
    }

    func onBackground(now: Date = Date()) {
        guard lockEnabled else { return }
        if backgroundedAt == nil { backgroundedAt = now }
        autoPromptSuppressed = false
        // 「立即」：进入后台时就上锁，应用切换器快照里是锁定页
        if graceSecs <= 0 { locked = true }
    }

    /// 回到前台：超过宽限时间则上锁，并自动发起一次验证
    func onForeground(now: Date = Date()) {
        let elapsed = backgroundedAt.map { now.timeIntervalSince($0) }
        backgroundedAt = nil
        if Self.shouldLock(enabled: lockEnabled, graceSecs: graceSecs, elapsed: elapsed) { locked = true }
        autoUnlock()
    }

    /// 自动发起验证（回到前台、锁定页出现）；上一次验证失败 / 取消后不再自动弹
    func autoUnlock() {
        guard !autoPromptSuppressed else { return }
        unlock()
    }

    /// 用户点「解锁」
    func unlockManually() {
        autoPromptSuppressed = false
        unlock()
    }

    private func unlock() {
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
                autoPromptSuppressed = true   // 其他错误保持锁定，可点「解锁」重试
            }
            return
        }
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: "解锁 Hush") { ok, _ in
            DispatchQueue.main.async {
                MainActor.assumeIsolated { self.finishAuthentication(ok) }
            }
        }
    }

    /// 系统验证结束（测试中直接调用）
    func finishAuthentication(_ ok: Bool) {
        authenticating = false
        if ok { locked = false } else { autoPromptSuppressed = true }
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
