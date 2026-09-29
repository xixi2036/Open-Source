import Foundation
import Observation
import UIKit
import UserNotifications

enum AppPhase: Equatable {
    case launching
    case loggedOut
    case needsProfile
    case ready
    /// 启动失败；keyMismatch = 本地库无法用当前密钥打开（重试无用，只能重置本地数据）
    case failed(String, keyMismatch: Bool = false)
}

/// 核心事件桥：回调在核心工作线程，必须立即返回；按到达顺序投递到主线程。
final class CoreEventBridge: EventListener, @unchecked Sendable {
    func onEvent(event: CoreEvent) {
        DispatchQueue.main.async {
            MainActor.assumeIsolated { HushCore.shared.handle(event) }
        }
    }
}

/// 全局可观察模型：持有 HushClient，负责启动、登录、连接、设置与事件分发。
/// 会话 / 通讯录 / 朋友圈的数据与操作分别在 ConversationStore、ContactsStore、MomentsStore 中，
/// 视图不直接访问 HushClient。
@MainActor @Observable
final class HushCore {
    static let shared = HushCore()

    @ObservationIgnored private var client: HushClient!
    @ObservationIgnored private let bridge = CoreEventBridge()

    var phase: AppPhase = .launching
    var account: MyAccount?
    var serverURL: String = UserDefaults.standard.string(forKey: "serverURL") ?? "http://127.0.0.1:8080"
    var connection: ConnectionState = .disconnected
    var settings = Settings(readReceipts: true, typingIndicators: true, defaultDisappearingSecs: 0, notifications: true)

    @ObservationIgnored private(set) lazy var chats = ConversationStore(core: self)
    @ObservationIgnored private(set) lazy var contacts = ContactsStore(core: self)
    @ObservationIgnored private(set) lazy var moments = MomentsStore(core: self)
    /// 头像 / 朋友圈图片本地路径
    let media = MediaCache()

    var toast: String?
    var loggedOutReason: String?
    /// 当前可见的聊天页（用于通知抑制与 mark_read）
    var activeConvId: String?
    var appActive = true

    @ObservationIgnored private var observers: [UUID: (CoreEvent) -> Void] = [:]

    private init() {}

    var myId: String { account?.userId ?? "" }
    var myProfile: Profile? { account?.profile }

    private func attach(_ c: HushClient?) {
        client = c
        chats.attach(c)
        contacts.attach(c)
        moments.attach(c)
    }

    // MARK: - 启动与登录

    func bootstrap() {
        guard client == nil else { return }
        do {
            let key = try KeyStore.loadOrCreateDBKey()
            let c = try HushClient(dataDir: AppPaths.dataDir.path, dbKey: key,
                                   platform: "ios", deviceName: UIDevice.current.name)
            c.setListener(listener: bridge)
            attach(c)
        } catch HushError.KeyMismatch {
            phase = .failed("本机的聊天数据无法用当前密钥打开（例如从备份恢复到了新设备）", keyMismatch: true)
            return
        } catch {
            phase = .failed(describe(error))
            return
        }
        if client.isLoggedIn(), let acc = client.myAccount() {
            account = acc
            serverURL = acc.serverUrl
            loadLocal()
            phase = acc.profile.name.isEmpty ? .needsProfile : .ready
            startConnection()
            Task { await fullSync() }
        } else {
            phase = .loggedOut
        }
    }

    /// 启动失败后用户确认「重置本地数据」：删除本地库与密钥，回到登录页（服务器上的好友关系不受影响）
    func resetLocalDataAndRestart() {
        guard client == nil else { return }
        try? FileManager.default.removeItem(at: AppPaths.dataDir)
        KeyStore.deleteDBKey()
        phase = .launching
        bootstrap()
    }

    /// 启动失败后重试（例如设备解锁后钥匙串可读）
    func retryBootstrap() {
        guard client == nil else { return }
        phase = .launching
        bootstrap()
    }

    func requestCode(server: String, email: String) async throws {
        try await client.requestCode(serverUrl: server, email: email)
    }

    func verify(server: String, email: String, code: String) async throws {
        let acc = try await client.verifyCode(serverUrl: server, email: email, code: code)
        account = acc
        serverURL = server
        UserDefaults.standard.set(server, forKey: "serverURL")
        loggedOutReason = nil
        loadLocal()
        // 登录后全量同步（服务端会清空旧事件队列），然后再启动长连接
        await fullSync()
        startConnection()
        phase = (acc.isNew || acc.profile.name.isEmpty) ? .needsProfile : .ready
    }

    func completeProfile(name: String, avatarPath: String?) async throws {
        try await updateMyProfile(name: name, about: nil, avatarPath: avatarPath)
        phase = .ready
    }

    func updateMyProfile(name: String?, about: String?, avatarPath: String?) async throws {
        let p = try await client.updateProfile(name: name, about: about, avatarPath: avatarPath)
        if let avatarPath { AppPaths.discardOutbox(avatarPath) }
        account?.profile = p
        contacts.remember(p)
    }

    func logout() async {
        CallManager.shared.hangup()
        try? await client.logout()
        resetState()
        phase = .loggedOut
    }

    /// 退出登录 / 被登出：清空上一个账号在内存、通知中心、角标和临时目录里的所有痕迹。
    /// 核心 logout 会删除 files/，缓存的头像 / 媒体路径必须一起作废。
    func resetState() {
        account = nil
        chats.reset()
        contacts.reset()
        moments.reset()
        media.reset()
        activeConvId = nil
        VoicePlayer.shared.stop()
        UNBadge.set(0)
        let center = UNUserNotificationCenter.current()
        center.removeAllDeliveredNotifications()
        center.removeAllPendingNotificationRequests()
        NotificationService.shared.reset()
        AppPaths.clearTemporaryPlaintext()
        Router.shared.reset()
    }

    func startConnection() {
        guard let c = client, c.isLoggedIn() else { return }
        do { try c.start() } catch HushError.NotLoggedIn { } catch { toast = describe(error) }
        connection = c.connectionState()
    }

    /// 回到前台：确保长连接在跑，并用 HTTP 补拉一次积压消息
    func onForeground() {
        appActive = true
        guard phase == .ready || phase == .needsProfile else { return }
        startConnection()
        Task {
            _ = try? await client.syncNow()
            await contacts.refreshFriendRequests()
            await moments.refreshNotes()
            chats.reload()
        }
    }

    func fullSync() async {
        guard client != nil else { return }
        await contacts.fullSync()
        await moments.refreshNotes()
        chats.reload()
    }

    private func loadLocal() {
        moments.loadLocal(userId: myId)
        settings = client.settings()
        contacts.loadLocal(me: account?.profile)
        chats.reload()
    }

    func saveSettings(_ s: Settings) {
        do {
            try client.setSettings(settings: s)
            settings = s
        } catch { toast = describe(error) }
    }

    // MARK: - 事件

    /// 聊天页等订阅原始事件；返回的 token 用于取消订阅
    func subscribe(_ handler: @escaping (CoreEvent) -> Void) -> UUID {
        let id = UUID()
        observers[id] = handler
        return id
    }

    func unsubscribe(_ id: UUID) { observers[id] = nil }

    /// 事件分发：连接 / 通话 / 登出在这里处理，其余交给各 Store，最后通知订阅者
    func handle(_ event: CoreEvent) {
        switch event {
        case let .connectionChanged(state):
            connection = state
            // 连接被同账号新连接替换后核心会保持登录但停止重连：前台时稍后再 start() 一次（幂等）
            if state == .disconnected && appActive {
                DispatchQueue.main.asyncAfter(deadline: .now() + 5) { [weak self] in
                    MainActor.assumeIsolated {
                        guard let self, self.appActive, self.connection == .disconnected, self.phase == .ready else { return }
                        self.startConnection()
                    }
                }
            }
        case let .callSignalReceived(fromUserId, signal):
            CallManager.shared.handleSignal(from: fromUserId, signal: signal)
        case let .loggedOut(reason):
            CallManager.shared.hangup()
            loggedOutReason = Self.loggedOutText(reason)
            resetState()
            phase = .loggedOut
        default:
            contacts.handle(event)
            chats.handle(event)
            moments.handle(event)
        }
        for h in observers.values { h(event) }
    }

    static func loggedOutText(_ reason: String) -> String {
        switch reason {
        case "logged_in_elsewhere": return "你的账号已在其他设备登录"
        case "session_expired": return "登录已过期，请重新登录"
        default: return "登录已失效（\(reason)）"
        }
    }

    // MARK: - 媒体缓存

    func loadAvatar(_ avatarId: String) async {
        guard let c = client else { return }
        await media.loadAvatar(avatarId) { try await c.fetchAvatar(avatarId: $0) }
    }

    func loadMedia(_ attachmentId: String) async {
        guard let c = client else { return }
        await media.loadMedia(attachmentId) { try await c.fetchMedia(attachmentId: $0) }
    }

    // MARK: - 通话（CallManager 使用）

    func sendCallSignal(to userId: String, _ signal: CallSignal) async throws {
        guard let c = client else { throw HushError.NotLoggedIn }
        try await c.sendCallSignal(userId: userId, signal: signal)
    }

    func turnCredentials() async -> TurnCredentials? {
        try? await client?.turnCredentials()
    }

    /// 执行一个可能失败的操作，失败时弹出提示
    func run(_ op: @escaping () async throws -> Void) {
        Task {
            do { try await op() } catch { toast = describe(error) }
        }
    }
}
