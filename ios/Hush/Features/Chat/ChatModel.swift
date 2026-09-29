import Foundation
import Observation
import SwiftUI
import UniformTypeIdentifiers

/// 单个会话页的状态：消息分页、事件合并、输入状态、引用回复
@MainActor @Observable
final class ChatModel {
    let convId: String
    var messages: [MessageItem] = []
    var hasMore = true
    var loadingOlder = false
    var quoting: MessageItem?
    var draft = ""
    /// 需要滚动到底部时递增（自己发出的消息、或用户本来就停在底部时收到新消息）
    var bottomTick = 0
    var visible = false
    /// 底部锚点是否在屏幕内（由 ChatView 更新）
    var atBottom = true
    /// 用户在看历史时新到的消息数（显示「↓ n 条新消息」）
    var unseenCount = 0
    /// @提及：输入框文本与提及区间（UTF-16）
    var composer = MentionComposer()
    /// 正在选择 @ 的成员：刚输入的「@」的 UTF-16 位置（nil = 未弹出）
    var mentionTrigger: Int?
    /// 输入框文本被程序改写（插入 @成员、整段删除提及）后应放置的光标（UTF-16 偏移）；输入栏应用后置 nil。
    /// 普通按键不设置，避免打断拼音输入法的候选（marked text）。
    var pendingCursor: Int?
    /// 长按菜单里点了「转发」：菜单收起后由聊天页弹出会话选择
    @ObservationIgnored var pendingForward: MessageItem?
    @ObservationIgnored private var draftRestored = false
    /// 刚恢复的草稿文本：它引起的文本变化不发送「正在输入」
    @ObservationIgnored private var restoredText: String?

    @ObservationIgnored private var token: UUID?
    @ObservationIgnored private var typingSent = false
    @ObservationIgnored private var typingStopWork: DispatchWorkItem?
    @ObservationIgnored private var markReadWork: DispatchWorkItem?
    @ObservationIgnored private let core = HushCore.shared
    /// 按 sort_ts 游标取一页消息（before 为 nil 取最新一页）；nil 表示当前无法读取（未登录）。测试中替换。
    @ObservationIgnored var loadPage: (_ before: Int64?, _ limit: UInt32) -> [MessageItem]?
    /// 定位消息时在后台线程逐页读取用的分页函数（每次定位时取一次）。测试中替换。
    @ObservationIgnored var backgroundPager: () -> @Sendable (_ before: Int64?, _ limit: UInt32) -> [MessageItem]?
    let pageSize: UInt32 = 40
    /// 正在后台向上翻页定位某条消息（聊天页显示加载指示）
    var locating = false

    init(convId: String) {
        self.convId = convId
        let chats = HushCore.shared.chats
        loadPage = { before, limit in chats.messages(convId, before: before, limit: limit) }
        backgroundPager = { chats.backgroundPager(convId) }
    }

    var isGroup: Bool { convId.hasPrefix("g:") }
    var peerId: String? { convId.hasPrefix("u:") ? String(convId.dropFirst(2)) : nil }
    var groupId: String? { isGroup ? String(convId.dropFirst(2)) : nil }

    func onAppear() {
        visible = true
        core.activeConvId = convId
        if token == nil {
            token = core.subscribe { [weak self] ev in self?.handle(ev) }
            // 从设置页等返回：保留已加载的历史与滚动位置，只合并最新一页
            if messages.isEmpty { reload() } else { mergeLatest() }
            restoreDraft()
        }
        markRead()
    }

    func onDisappear() {
        visible = false
        if core.activeConvId == convId { core.activeConvId = nil }
        stopTyping()
        saveDraft()
        teardown()
    }

    // MARK: - 草稿（核心 set_draft；只保存文字，@提及区间不保存，恢复后按普通文字显示）

    /// 首次进入时恢复草稿（从设置页返回不覆盖输入框）
    func restoreDraft() {
        guard !draftRestored else { return }
        draftRestored = true
        guard draft.isEmpty, let d = core.chats.summary(convId)?.draft, !d.isEmpty else { return }
        composer.reset(d)
        restoredText = d
        draft = d
    }

    /// 离开聊天页 / App 进入后台时保存；空白则清除
    func saveDraft() {
        guard draftRestored else { return }
        core.chats.setDraft(convId, draft)
    }

    // MARK: - @提及

    var mentionQuery: String? {
        guard let t = mentionTrigger else { return nil }
        return composer.query(after: t)
    }

    /// 可 @ 的群成员（不含自己；不提供 @所有人），按筛选文字过滤
    func mentionCandidates(_ members: [String], query: String) -> [String] {
        let others = members.filter { $0 != core.myId }
        guard !query.isEmpty else { return others }
        return others.filter { core.contacts.displayName($0).localizedCaseInsensitiveContains(query) }
    }

    func pickMention(_ userId: String) {
        guard let t = mentionTrigger else { return }
        let end = t + 1 + (mentionQuery?.utf16.count ?? 0)
        let name = core.contacts.displayName(userId)
        mentionTrigger = nil
        draft = composer.insertMention(userId: userId, name: name, replacing: t, end)
        pendingCursor = composer.cursor
    }

    func cancelMention() { mentionTrigger = nil }

    // MARK: - 定位到某条消息（搜索结果、引用）

    /// 定位到某条消息：已加载直接返回 true；否则在后台线程从当前最早一条向上逐页读取（最多 maxPages 页 × pageSize 条），
    /// 读到目标后回到主线程把这些页一次性并入列表（按 id 去重）并返回 true。
    /// 调用方的任务被取消（离开聊天页、又点了别的定位）时尽快停止并返回 false；找不到或被取消时列表保持不变。
    /// 定位期间 loadingOlder 为 true，顶部的上滑加载不会与它并发翻页。
    func locate(_ id: String, maxPages: Int = 200) async -> Bool {
        if messages.contains(where: { $0.id == id }) { return true }
        guard hasMore, !locating, let first = messages.first else { return false }
        locating = true
        loadingOlder = true
        defer { locating = false; loadingOlder = false }
        let pager = backgroundPager()
        let size = pageSize
        let work = Task.detached(priority: .userInitiated) { () -> LocateResult? in
            Self.scan(for: id, from: first.sortTs, pager: pager, pageSize: size, maxPages: maxPages)
        }
        // 分离任务不继承取消：调用方取消时转发给它
        let result = await withTaskCancellationHandler { await work.value } onCancel: { work.cancel() }
        guard !Task.isCancelled, let result, result.found, !messages.isEmpty else { return false }
        let existing = Set(messages.map(\.id))
        let older = result.pages.reversed().flatMap { $0 }.filter { !existing.contains($0.id) }
        messages.insert(contentsOf: older, at: 0)
        hasMore = !result.reachedOldest
        return messages.contains { $0.id == id }
    }

    struct LocateResult: Sendable {
        /// 读到的页，按读取顺序（越往后越早），每页内部按时间升序
        var pages: [[MessageItem]]
        var found: Bool
        /// 已读到最早的消息（最后一页不足一页）
        var reachedOldest: Bool
    }

    /// 从游标 before 向上逐页读取直到找到 id；在调用线程同步执行（定位时在后台线程调用）。任务被取消时返回 nil。
    nonisolated static func scan(for id: String, from before: Int64,
                                 pager: (Int64?, UInt32) -> [MessageItem]?,
                                 pageSize: UInt32, maxPages: Int) -> LocateResult? {
        var cursor = before
        var pages: [[MessageItem]] = []
        for _ in 0..<maxPages {
            if Task.isCancelled { return nil }
            guard let page = pager(cursor, pageSize), let oldest = page.first else {
                return LocateResult(pages: pages, found: false, reachedOldest: true)
            }
            pages.append(page)
            let full = page.count >= Int(pageSize)
            if page.contains(where: { $0.id == id }) { return LocateResult(pages: pages, found: true, reachedOldest: !full) }
            // 不足一页 = 已到最早；游标没有前进（异常分页）也停止
            guard full, oldest.sortTs < cursor else { return LocateResult(pages: pages, found: false, reachedOldest: !full) }
            cursor = oldest.sortTs
        }
        return LocateResult(pages: pages, found: false, reachedOldest: false)
    }

    func teardown() {
        if let token { core.unsubscribe(token) }
        token = nil
    }

    func reload() {
        let list = loadPage(nil, pageSize) ?? []
        messages = list
        hasMore = list.count >= Int(pageSize)
        bottomTick += 1
    }

    /// 返回聊天页时合并最新一页：更新 / 插入新消息，删除窗口内已不存在的消息，不改动更早的已加载页
    func mergeLatest() {
        guard let latest = loadPage(nil, pageSize) else { return }
        merge(latest: latest)
    }

    func merge(latest: [MessageItem]) {
        if latest.isEmpty { messages = []; return }
        let ids = Set(latest.map(\.id))
        if let oldest = latest.map(\.sortTs).min() {
            messages.removeAll { $0.sortTs >= oldest && !ids.contains($0.id) }
        }
        var appended = 0
        for m in latest where upsert(m, appendIfMissing: true) { appended += 1 }
        if appended > 0 { noteNewMessages(appended, outgoing: false) }
    }

    func setAtBottom(_ v: Bool) {
        atBottom = v
        if v { unseenCount = 0 }
    }

    private func noteNewMessages(_ n: Int, outgoing: Bool) {
        if outgoing || atBottom { bottomTick += 1 } else { unseenCount += n }
    }

    /// 相邻两条消息是否跨天（按排序时间），与 messages 一一对应
    var dayBreaks: [Bool] { Self.dayBreaks(messages) }

    static func dayBreaks(_ list: [MessageItem]) -> [Bool] {
        list.indices.map { i in
            i == 0 || !Fmt.cal.isDate(Fmt.date(list[i].sortTs), inSameDayAs: Fmt.date(list[i - 1].sortTs))
        }
    }

    func loadOlder() {
        guard hasMore, !loadingOlder, let first = messages.first else { return }
        loadingOlder = true
        // 游标直接用核心给出的 sortTs（与 messages(before_ts) 同一口径）；同一时间戳的边界消息按 id 去重
        let older = loadPage(first.sortTs, pageSize) ?? []
        let existing = Set(messages.map(\.id))
        let fresh = older.filter { !existing.contains($0.id) }
        messages.insert(contentsOf: fresh, at: 0)
        hasMore = older.count >= Int(pageSize)
        loadingOlder = false
    }

    func handle(_ ev: CoreEvent) {
        switch ev {
        case let .messageAdded(cid, m, _, _) where cid == convId:
            // 离线补拉的旧消息插在中间：不滚动、不计入新消息
            if upsert(m, appendIfMissing: true) { noteNewMessages(1, outgoing: m.isOutgoing) }
            if !m.isOutgoing { markRead() }
        case let .messageUpdated(cid, m) where cid == convId:
            upsert(m, appendIfMissing: false)
        case let .messageRemoved(cid, id) where cid == convId:
            withAnimation(.easeOut(duration: 0.35)) { messages.removeAll { $0.id == id } }
        case .conversationsChanged:
            // 清空聊天记录等批量变化
            if messages.isEmpty == false,
               (loadPage(nil, 1) ?? []).isEmpty {
                messages = []
                unseenCount = 0
            }
        default: break
        }
    }

    /// 更新或插入；返回是否作为最新一条追加到末尾
    @discardableResult
    private func upsert(_ m: MessageItem, appendIfMissing: Bool) -> Bool {
        if let i = messages.firstIndex(where: { $0.id == m.id }) {
            if messages[i].sortTs == m.sortTs {
                messages[i] = m
            } else {
                // K3：发出的消息被服务端接受后 sortTs 改为服务端时间，按新的排序时间挪到正确位置
                messages.remove(at: i)
                let idx = messages.firstIndex(where: { $0.sortTs > m.sortTs }) ?? messages.count
                messages.insert(m, at: idx)
            }
            return false
        }
        guard appendIfMissing else { return false }
        // 按排序时间插入（离线补拉的消息可能早于当前最后一条）
        if let last = messages.last, m.sortTs < last.sortTs,
           let idx = messages.firstIndex(where: { $0.sortTs > m.sortTs }) {
            messages.insert(m, at: idx)
            return false
        }
        messages.append(m)
        return true
    }

    /// 标记已读（合并 0.3 秒内的多次调用，积压消息同步时只发一次）
    func markRead() {
        guard visible, core.appActive else { return }
        markReadWork?.cancel()
        let work = DispatchWorkItem { [weak self] in
            MainActor.assumeIsolated { self?.markReadNow() }
        }
        markReadWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.3, execute: work)
    }

    private func markReadNow() {
        guard visible, core.appActive else { return }
        let hasUnread = (core.chats.summary(convId)?.unreadCount ?? 0) > 0
            || messages.contains { !$0.isOutgoing && $0.status != .read && isVisibleContent($0) }
        guard hasUnread else { return }
        core.chats.markRead(convId)
    }

    private func isVisibleContent(_ m: MessageItem) -> Bool {
        switch m.body {
        case .text, .attachments: return !m.deleted
        default: return false
        }
    }

    // MARK: - 发送

    func sendText() {
        if composer.text != draft { _ = composer.textChanged(to: draft) }
        let (text, mentions) = composer.payload()
        guard !text.isEmpty else { return }
        let quoteId = quoting?.id
        composer.reset()
        mentionTrigger = nil
        pendingCursor = nil
        draft = ""
        quoting = nil
        stopTyping()
        let cid = convId
        let chats = core.chats
        core.run { try await chats.sendText(cid, text: text, quoteId: quoteId, mentions: mentions) }
    }

    func sendImage(url: URL, width: Int, height: Int) {
        let meta = AttachmentMeta(fileName: url.lastPathComponent, width: UInt32(width), height: UInt32(height),
                                  durationMs: 0, thumbnail: ImageUtil.thumbnail(url))
        send(path: url.path, mime: "image/jpeg", kind: .image, meta: meta)
    }

    func sendVoice(url: URL, durationMs: UInt64) {
        let meta = AttachmentMeta(fileName: "voice.m4a", width: 0, height: 0, durationMs: durationMs, thumbnail: nil)
        send(path: url.path, mime: "audio/mp4", kind: .voice, meta: meta)
    }

    func sendFile(url: URL) {
        let meta = AttachmentMeta(fileName: url.lastPathComponent, width: 0, height: 0, durationMs: 0, thumbnail: nil)
        send(path: url.path, mime: MimeType.of(url), kind: .file, meta: meta)
    }

    private func send(path: String, mime: String, kind: AttachmentKind, meta: AttachmentMeta) {
        let cid = convId
        let chats = core.chats
        core.run {
            // 核心读入后会在自己的目录保存副本（失败重发也用那份），outbox 里的明文临时文件随即删除
            defer { AppPaths.discardOutbox(path) }
            try await chats.sendAttachment(cid, path: path, mime: mime, kind: kind, meta: meta)
        }
    }

    func resend(_ m: MessageItem) {
        let chats = core.chats, id = m.id
        core.run { try await chats.resend(id) }
    }

    func react(_ m: MessageItem, _ emoji: String) {
        let mine = m.reactions.first { $0.userIds.contains(core.myId) }?.emoji
        let remove = mine == emoji
        let chats = core.chats, id = m.id
        core.run { try await chats.react(id, emoji: emoji, remove: remove) }
    }

    func recall(_ m: MessageItem) {
        let chats = core.chats, id = m.id
        core.run { try await chats.recall(id) }
    }

    func deleteLocal(_ m: MessageItem) {
        do {
            try core.chats.deleteLocal(m.id)
            withAnimation { messages.removeAll { $0.id == m.id } }
        } catch { core.toast = describe(error) }
    }

    func canRecall(_ m: MessageItem) -> Bool {
        guard m.isOutgoing, !m.deleted, m.status != .sending, m.status != .failed else { return false }
        switch m.body { case .text, .attachments: break; default: return false }
        return Date().timeIntervalSince(Fmt.date(m.sentAt)) < 24 * 3600
    }

    // MARK: - 正在输入：文本变化时 true，停止 3 秒或发送后 false

    func draftChanged() {
        if draft != composer.text {
            let r = composer.textChanged(to: draft)
            if r.text != draft {
                // 整体删除了提及片段；再次触发 onChange 时两者相等。光标留在删除处
                draft = r.text
                pendingCursor = composer.cursor
            }
            if let t = r.trigger, isGroup { mentionTrigger = t }
        }
        if mentionTrigger != nil && mentionQuery == nil { mentionTrigger = nil }
        if let r = restoredText {
            restoredText = nil
            if r == draft { return }
        }
        guard core.settings.typingIndicators else { return }
        if draft.isEmpty { stopTyping(); return }
        if !typingSent {
            typingSent = true
            core.chats.setTyping(convId, true)
        }
        typingStopWork?.cancel()
        let work = DispatchWorkItem { [weak self] in
            MainActor.assumeIsolated { self?.stopTyping() }
        }
        typingStopWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 3, execute: work)
    }

    func stopTyping() {
        typingStopWork?.cancel()
        guard typingSent else { return }
        typingSent = false
        core.chats.setTyping(convId, false)
    }
}

enum MimeType {
    static func of(_ url: URL) -> String {
        UTType(filenameExtension: url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
    }
}
