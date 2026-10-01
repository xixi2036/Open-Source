import Foundation
import Observation
import UIKit

/// 会话与消息：会话列表、正在输入、消息分页 / 发送 / 操作、附件下载、消息搜索。
/// 视图（及 ChatModel）只通过这里访问消息相关接口，不直接调用 HushClient。
@MainActor @Observable
final class ConversationStore {
    var conversations: [ConversationSummary] = []
    var archivedConversations: [ConversationSummary] = []
    /// convId -> (userId -> 最近一次“正在输入”的时间)
    var typing: [String: [String: Date]] = [:]

    @ObservationIgnored private var client: HushClient?
    @ObservationIgnored private unowned let core: HushCore
    @ObservationIgnored private var reloadScheduled = false
    @ObservationIgnored private var downloads: Set<String> = []
    /// 附件下载失败记录（"消息id#下标"）：有记录的附件不再自动下载，界面显示失败 / 已过期，点击重试
    var downloadFailures: [String: DownloadFailure] = [:]
    /// C3：列表在后台线程读取；同一时间只有一次读取在跑，期间的刷新请求合并成一次
    @ObservationIgnored private var reloading = false
    @ObservationIgnored private var reloadPending = false
    /// reset() 时递增：退出登录前发出的读取结果不再写回
    @ObservationIgnored private var generation = 0
    /// 发送失败、等待连接恢复后自动重发的消息
    @ObservationIgnored private var autoResend = AutoResendQueue()
    @ObservationIgnored private var autoResending = false
    /// B6：消息搜索同一时间只跑一次（核心搜索持有数据库锁且无法中断），期间的新查询只保留最后一个
    @ObservationIgnored private var searchRunning = false
    @ObservationIgnored private var searchWaiters: [CheckedContinuation<Void, Never>] = []

    init(core: HushCore) { self.core = core }

    func attach(_ client: HushClient?) { self.client = client }

    private func api() throws -> HushClient {
        guard let client else { throw HushError.NotLoggedIn }
        return client
    }

    var totalUnread: Int {
        conversations.filter { !$0.muted }.reduce(0) { $0 + Int($1.unreadCount) }
    }

    func reset() {
        conversations = []; archivedConversations = []
        typing = [:]
        downloads = []
        downloadFailures = [:]
        generation += 1
        reloadPending = false
        autoResend = AutoResendQueue()
    }

    // MARK: - 列表

    /// 刷新会话列表。C3：两次查询（含逐条解密最后一条消息）在后台线程执行，结果回主线程应用。
    func reload() {
        guard let c = client, core.account != nil else { return }
        if reloading { reloadPending = true; return }
        reloading = true
        let gen = generation
        Task {
            let lists = await Task.detached(priority: .userInitiated) {
                ((try? c.conversations(archived: false)) ?? [], (try? c.conversations(archived: true)) ?? [])
            }.value
            reloading = false
            if gen == generation, core.account != nil {
                if conversations != lists.0 { conversations = lists.0 }
                if archivedConversations != lists.1 { archivedConversations = lists.1 }
                UNBadge.set(totalUnread)
            }
            if reloadPending {
                reloadPending = false
                reload()
            }
        }
    }

    private func scheduleReload() {
        guard !reloadScheduled else { return }
        reloadScheduled = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.08) { [weak self] in
            MainActor.assumeIsolated {
                self?.reloadScheduled = false
                self?.reload()
            }
        }
    }

    func handle(_ event: CoreEvent) {
        switch event {
        case .conversationsChanged, .messageRemoved, .friendsChanged, .groupsChanged:
            scheduleReload()
        case let .messageUpdated(_, message):
            scheduleReload()
            noteFailed([message])
        case let .messageAdded(convId, message, backlog, mentionsMe):
            scheduleReload()
            noteFailed([message])
            if !message.isOutgoing {
                clearTyping(convId: convId, userId: message.senderId)
                NotificationService.shared.notifyIfNeeded(convId: convId, message: message, backlog: backlog, mentionsMe: mentionsMe)
            }
        case let .typing(convId, userId, isTyping):
            if isTyping {
                typing[convId, default: [:]][userId] = Date()
                DispatchQueue.main.asyncAfter(deadline: .now() + 6) { [weak self] in
                    MainActor.assumeIsolated { self?.expireTyping(convId: convId, userId: userId) }
                }
            } else {
                clearTyping(convId: convId, userId: userId)
            }
        default:
            break
        }
    }

    private func clearTyping(convId: String, userId: String) {
        guard typing[convId]?[userId] != nil else { return }
        typing[convId]?[userId] = nil
    }

    private func expireTyping(convId: String, userId: String) {
        if let t = typing[convId]?[userId], Date().timeIntervalSince(t) >= 5.5 {
            typing[convId]?[userId] = nil
        }
    }

    func typingUsers(_ convId: String) -> [String] {
        Array((typing[convId] ?? [:]).keys)
    }

    // MARK: - 会话

    func summary(_ convId: String) -> ConversationSummary? {
        conversations.first { $0.id == convId }
            ?? archivedConversations.first { $0.id == convId }
            ?? (try? client?.conversation(convId: convId)) ?? nil
    }

    func title(_ convId: String) -> String {
        if convId.hasPrefix("g:") {
            return core.contacts.group(String(convId.dropFirst(2)))?.name ?? summary(convId)?.title ?? "群聊"
        }
        if convId.hasPrefix("u:") { return core.contacts.displayName(String(convId.dropFirst(2))) }
        return summary(convId)?.title ?? ""
    }

    func directConversationId(_ userId: String) -> String {
        client?.directConversationId(userId: userId) ?? "u:\(userId)"
    }

    enum Op {
        /// clear：清空聊天记录（保留会话）；delete：删除会话（本机消息、附件缓存、草稿一并删除，群会话不退群）
        case pin(Bool), mute(Bool), archive(Bool), clear, delete
    }

    /// 会话操作的唯一入口：失败统一 toast，完成后刷新列表
    @discardableResult
    func update(_ convId: String, _ op: Op) -> Bool {
        guard let c = client else { return false }
        defer { reload() }
        do {
            switch op {
            case let .pin(v): try c.setPinned(convId: convId, pinned: v)
            case let .mute(v): try c.setMuted(convId: convId, muted: v)
            case let .archive(v): try c.setArchived(convId: convId, archived: v)
            case .clear: try c.clearConversation(convId: convId)
            case .delete: try c.deleteConversation(convId: convId)
            }
            return true
        } catch {
            core.toast = describe(error)
            return false
        }
    }

    func setDisappearing(_ convId: String, secs: UInt32) async throws {
        try await api().setDisappearing(convId: convId, secs: secs)
        reload()
    }

    // MARK: - 消息

    /// 按核心的 sort_ts 游标分页：返回 sortTs < before 的最近 limit 条
    func messages(_ convId: String, before: Int64?, limit: UInt32) -> [MessageItem]? {
        try? client?.messages(convId: convId, beforeTs: before, limit: limit)
    }

    /// 可在后台线程调用的分页读取（捕获当前 HushClient；核心的 messages 是同步调用，长会话逐页读取时不占主线程）
    func backgroundPager(_ convId: String) -> @Sendable (_ before: Int64?, _ limit: UInt32) -> [MessageItem]? {
        let c = client
        return { before, limit in try? c?.messages(convId: convId, beforeTs: before, limit: limit) }
    }

    /// B4：发送用后台任务保护——发出后立刻切到后台时，系统仍给一段时间把加密、上传、投递做完
    func sendText(_ convId: String, text: String, quoteId: String?, mentions: [Mention]) async throws {
        let c = try api()
        try await BackgroundWork.protect("send-text") {
            if mentions.isEmpty {
                _ = try await c.sendText(convId: convId, text: text, quoteId: quoteId)
            } else {
                _ = try await c.sendTextWithMentions(convId: convId, text: text, quoteId: quoteId, mentions: mentions)
            }
        }
    }

    /// 转发到多个会话；返回每个目标的新消息（发送失败的那条状态为 failed，可在对应会话重发）
    func forward(_ messageId: String, to convIds: [String]) async throws -> [MessageItem] {
        let c = try api()
        return try await BackgroundWork.protect("forward") {
            try await c.forwardMessage(messageId: messageId, convIds: convIds)
        }
    }

    /// 保存草稿（nil / 空白清除）；内容没变时核心不发事件
    func setDraft(_ convId: String, _ text: String?) {
        guard let c = client, core.account != nil else { return }
        let t = text?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false ? text : nil
        if summary(convId)?.draft == t { return }
        do { try c.setDraft(convId: convId, text: t) } catch { core.toast = describe(error) }
    }

    func sendAttachment(_ convId: String, path: String, mime: String, kind: AttachmentKind, meta: AttachmentMeta) async throws {
        let c = try api()
        try await BackgroundWork.protect("send-attachment") {
            _ = try await c.sendAttachment(convId: convId, filePath: path, mime: mime, kind: kind, caption: nil, meta: meta)
        }
    }

    @discardableResult
    func resend(_ messageId: String) async throws -> MessageItem {
        let c = try api()
        autoResend.markAttempted(messageId)   // 手动重发过的不再自动重发
        return try await BackgroundWork.protect("resend") {
            try await c.resend(messageId: messageId)
        }
    }

    // MARK: - 失败消息自动重发

    /// 记录发送失败的消息（事件里看到的，或聊天页从库里读到的）。核心没有「列出失败消息」的接口，
    /// 所以只覆盖本次运行中出现过的失败消息。
    func noteFailed(_ messages: [MessageItem]) {
        for m in messages where m.isOutgoing && m.status == .failed && !m.deleted {
            autoResend.note(id: m.id, sortTs: m.sortTs)
        }
    }

    /// 连接恢复后调用：把记录的失败消息按原来的先后顺序逐条重发，每条只自动重发一次
    func resendFailedAfterReconnect() {
        guard !autoResending, let c = client else { return }
        let ids = autoResend.takePending()
        guard !ids.isEmpty else { return }
        autoResending = true
        let gen = generation
        Task {
            await BackgroundWork.protect("auto-resend") {
                for id in ids {
                    guard gen == generation else { break }
                    // 逐条等待，保证顺序；仍然失败的留给用户手动重发
                    _ = try? await c.resend(messageId: id)
                }
            }
            autoResending = false
        }
    }

    func react(_ messageId: String, emoji: String, remove: Bool) async throws {
        try await api().react(messageId: messageId, emoji: emoji, remove: remove)
    }

    func recall(_ messageId: String) async throws {
        try await api().deleteForEveryone(messageId: messageId)
    }

    func deleteLocal(_ messageId: String) throws {
        try api().deleteLocal(messageId: messageId)
    }

    func markRead(_ convId: String) {
        guard let c = client else { return }
        Task { try? await c.markRead(convId: convId) }
    }

    func setTyping(_ convId: String, _ typing: Bool) {
        guard let c = client else { return }
        Task { try? await c.setTyping(convId: convId, typing: typing) }
    }

    /// 下载附件（结果也会通过 MessageUpdated 事件回写 localPath）；同一附件同时只下载一次。
    /// 失败会记入 downloadFailures（404 = 服务器上已清理，记为已过期），调用方据此显示失败态而不是反复弹提示。
    func downloadAttachment(_ messageId: String, index: Int) async throws -> String? {
        let key = Self.attachmentKey(messageId, index)
        guard !downloads.contains(key) else { return nil }
        downloads.insert(key)
        defer { downloads.remove(key) }
        if downloadFailures[key] != nil { downloadFailures[key] = nil }
        do {
            let c = try api()
            return try await BackgroundWork.protect("download") {
                try await c.downloadAttachment(messageId: messageId, index: UInt32(index))
            }
        } catch {
            downloadFailures[key] = DownloadFailure.classify(error)
            throw error
        }
    }

    static func attachmentKey(_ messageId: String, _ index: Int) -> String { "\(messageId)#\(index)" }

    func downloadFailure(_ messageId: String, _ index: Int) -> DownloadFailure? {
        downloadFailures[Self.attachmentKey(messageId, index)]
    }

    func isDownloading(_ messageId: String, _ index: Int) -> Bool {
        downloads.contains(Self.attachmentKey(messageId, index))
    }

    /// 消息内容搜索（B6）：使用核心的 search_messages_async；同一时间最多一次搜索在跑，
    /// 排队期间调用方被取消（输入又变了）就不再执行。
    func searchMessages(_ query: String, limit: Int) async -> [MessageItem] {
        guard !query.isEmpty, let c = client else { return [] }
        while searchRunning {
            await withCheckedContinuation { searchWaiters.append($0) }
            if Task.isCancelled { return [] }
        }
        if Task.isCancelled { return [] }
        searchRunning = true
        defer {
            searchRunning = false
            let waiters = searchWaiters
            searchWaiters = []
            for w in waiters { w.resume() }
        }
        // 核心的异步搜索：分批解密、够 200 条即停，在核心的运行时线程上执行
        return Array(((try? await c.searchMessagesAsync(query: query)) ?? []).prefix(limit))
    }
}

/// 附件下载失败的原因
enum DownloadFailure: Equatable {
    /// 可重试（网络、服务器错误等）
    case failed
    /// 服务器上已不存在（过期清理）
    case expired

    static func classify(_ error: Error) -> DownloadFailure {
        if let e = error as? HushError {
            switch e {
            case let .Server(code, _) where code == "not_found" || code == "gone" || code == "http_410": return .expired
            case .NotFound: return .expired
            default: return .failed
            }
        }
        return .failed
    }

    var text: String { self == .expired ? "附件已过期" : "下载失败，点击重试" }
}

/// 失败消息自动重发队列（纯逻辑）：按排序时间保持先后顺序，每条消息只自动重发一次
struct AutoResendQueue: Equatable {
    private var pending: [String: Int64] = [:]
    private var attempted: Set<String> = []

    mutating func note(id: String, sortTs: Int64) {
        guard !attempted.contains(id) else { return }
        pending[id] = sortTs
    }

    /// 手动重发过：不再自动重发
    mutating func markAttempted(_ id: String) {
        attempted.insert(id)
        pending[id] = nil
    }

    /// 取出待重发的消息 id（按排序时间升序，时间相同按 id），并标记为已尝试
    mutating func takePending() -> [String] {
        let ids = pending.sorted { ($0.value, $0.key) < ($1.value, $1.key) }.map(\.key)
        attempted.formUnion(ids)
        pending = [:]
        return ids
    }

    var pendingCount: Int { pending.count }
}

/// 后台任务保护：操作进行期间向系统申请后台执行时间（发送、上传、下载）
@MainActor
enum BackgroundWork {
    static func protect<T>(_ name: String, _ op: () async throws -> T) async rethrows -> T {
        var id = UIBackgroundTaskIdentifier.invalid
        id = UIApplication.shared.beginBackgroundTask(withName: "hush.\(name)") {
            // 时间用完：必须结束任务，否则系统会杀掉 App；操作本身继续，下次回前台后完成或失败
            if id != .invalid { UIApplication.shared.endBackgroundTask(id); id = .invalid }
        }
        defer { if id != .invalid { UIApplication.shared.endBackgroundTask(id); id = .invalid } }
        return try await op()
    }
}
