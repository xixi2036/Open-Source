import Foundation
import Observation

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
    }

    // MARK: - 列表

    func reload() {
        guard let c = client, core.account != nil else { return }
        conversations = (try? c.conversations(archived: false)) ?? []
        archivedConversations = (try? c.conversations(archived: true)) ?? []
        UNBadge.set(totalUnread)
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
        case .conversationsChanged, .messageUpdated, .messageRemoved, .friendsChanged, .groupsChanged:
            scheduleReload()
        case let .messageAdded(convId, message, backlog, mentionsMe):
            scheduleReload()
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

    func sendText(_ convId: String, text: String, quoteId: String?, mentions: [Mention]) async throws {
        if mentions.isEmpty {
            _ = try await api().sendText(convId: convId, text: text, quoteId: quoteId)
        } else {
            _ = try await api().sendTextWithMentions(convId: convId, text: text, quoteId: quoteId, mentions: mentions)
        }
    }

    /// 转发到多个会话；返回每个目标的新消息（发送失败的那条状态为 failed，可在对应会话重发）
    func forward(_ messageId: String, to convIds: [String]) async throws -> [MessageItem] {
        try await api().forwardMessage(messageId: messageId, convIds: convIds)
    }

    /// 保存草稿（nil / 空白清除）；内容没变时核心不发事件
    func setDraft(_ convId: String, _ text: String?) {
        guard let c = client, core.account != nil else { return }
        let t = text?.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty == false ? text : nil
        if summary(convId)?.draft == t { return }
        do { try c.setDraft(convId: convId, text: t) } catch { core.toast = describe(error) }
    }

    func sendAttachment(_ convId: String, path: String, mime: String, kind: AttachmentKind, meta: AttachmentMeta) async throws {
        _ = try await api().sendAttachment(convId: convId, filePath: path, mime: mime, kind: kind, caption: nil, meta: meta)
    }

    @discardableResult
    func resend(_ messageId: String) async throws -> MessageItem {
        try await api().resend(messageId: messageId)
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

    /// 下载附件（结果也会通过 MessageUpdated 事件回写 localPath）；同一附件同时只下载一次
    func downloadAttachment(_ messageId: String, index: Int) async throws -> String? {
        let key = "\(messageId)#\(index)"
        guard !downloads.contains(key) else { return nil }
        downloads.insert(key)
        defer { downloads.remove(key) }
        return try await api().downloadAttachment(messageId: messageId, index: UInt32(index))
    }

    /// 消息内容搜索：核心同步逐条解密，放到后台线程执行
    func searchMessages(_ query: String, limit: Int) async -> [MessageItem] {
        guard !query.isEmpty, let c = client else { return [] }
        return await Task.detached(priority: .userInitiated) {
            Array(((try? c.searchMessages(query: query)) ?? []).prefix(limit))
        }.value
    }
}
