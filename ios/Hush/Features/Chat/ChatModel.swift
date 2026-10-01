import Foundation
import Observation
import SwiftUI
import UniformTypeIdentifiers

/// 单个会话页的状态：消息分页、事件合并、输入状态、引用回复
@MainActor @Observable
final class ChatModel {
    let convId: String
    var messages: [MessageItem] = [] {
        didSet { dayStarts = DayBreaker.dayStartIds(messages) }
    }
    /// C2：每天第一条消息的 id（显示日期分隔）。只在消息列表变化时重算一次，视图刷新时直接查表
    private(set) var dayStarts: Set<String> = []
    /// 进入会话时的第一条未读消息：在它上方显示「以下为新消息」，并定位到这里
    var firstUnreadId: String?
    /// 进入会话时未读范围内第一条 @我 的消息（显示「有人@我」，点击跳转）
    var mentionJumpId: String?
    /// 多选模式与已选中的消息
    var selecting = false
    var selected: Set<String> = []
    /// 恢复草稿后，输入框第一次获得焦点时把光标放到末尾（C1）
    var cursorToEndOnFocus = false
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
    /// 长按菜单里点了「选择文字」：菜单收起后弹出可选择文字的面板
    @ObservationIgnored var pendingSelectText: MessageItem?
    /// 正在后台重新读取已加载范围（B9）；期间到达的事件在读取结果应用后重放
    @ObservationIgnored private var refreshing = false
    @ObservationIgnored private var eventsDuringRefresh: [CoreEvent] = []
    @ObservationIgnored private var didInitialLoad = false
    @ObservationIgnored private var resyncScheduled = false
    /// 已经为它调用过 mark_read 的最新一条消息（核心不再为已读逐条发 MessageUpdated，不能靠消息状态判断）
    @ObservationIgnored private var lastMarkedReadId: String?
    /// 用户删除了还在发送中的附件消息：对应的 sendAttachment 会以 InvalidInput 结束，不提示
    @ObservationIgnored private var cancelledSends = 0
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
            // 从设置页等返回：保留已加载的历史与滚动位置，并把整个已加载范围与数据库对齐
            //（B9：离开期间更早的消息可能被撤回、到期删除、状态变化，只合并最新一页会漏掉）
            if messages.isEmpty { initialLoad() } else { refreshLoadedRange() }
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
        // C1：光标放在草稿末尾（否则第一次点输入框时光标落在点击处，位置不稳定）
        pendingCursor = d.utf16.count
        cursorToEndOnFocus = true
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
        core.chats.noteFailed(list)
        bottomTick += 1
    }

    /// 首次进入会话：加载最新一页；有未读时再向上补页直到包含全部未读，
    /// 记下第一条未读（显示「以下为新消息」并定位到它）和未读里第一条 @我 的消息。
    /// 必须在第一次 markRead 之前调用（之后未读数就清零了）。
    func initialLoad() {
        let summary = core.chats.summary(convId)
        let unread = Int(summary?.unreadCount ?? 0)
        let mentions = Int(summary?.unreadMentions ?? 0)
        var list = loadPage(nil, pageSize) ?? []
        var more = list.count >= Int(pageSize)
        // 未读多于已加载的：继续向上读（最多 maxUnreadPages 页，再多就定位到已加载的最早一条）
        var pages = 1
        while unread > 0, more, pages < Self.maxUnreadPages,
              Self.firstUnreadIndex(list, unread: unread) == nil, let first = list.first {
            let older = loadPage(first.sortTs, pageSize) ?? []
            let existing = Set(list.map(\.id))
            list.insert(contentsOf: older.filter { !existing.contains($0.id) }, at: 0)
            more = older.count >= Int(pageSize)
            pages += 1
        }
        messages = list
        hasMore = more
        core.chats.noteFailed(list)
        didInitialLoad = true
        if unread > 0, !list.isEmpty {
            let idx = Self.firstUnreadIndex(list, unread: unread) ?? 0
            firstUnreadId = list[idx].id
            // 先按「不在底部」处理：聊天页定位完成后按实际位置判定，看到最新一条才标记已读
            atBottom = false
            if mentions > 0 {
                mentionJumpId = list[idx...].first { MentionText.mentions($0, user: core.myId) }?.id
            }
            // 未读都在一屏之内时定位到分隔条与停在底部是同一个位置，由聊天页根据实际布局决定
        }
        bottomTick += 1
    }

    static let maxUnreadPages = 8

    /// 会被核心计入未读数的消息：别人发来的正文、附件、无法解密的消息和通话记录
    nonisolated static func countsAsUnread(_ m: MessageItem) -> Bool {
        guard !m.isOutgoing else { return false }
        switch m.body {
        case .text, .attachments, .undecryptable: return true
        case let .callLog(_, _, _, outgoing, _): return !outgoing
        case .system: return false
        }
    }

    /// 第一条未读消息的下标：从最新往回数第 unread 条计入未读的消息；已加载的不够数时返回 nil
    nonisolated static func firstUnreadIndex(_ list: [MessageItem], unread: Int) -> Int? {
        guard unread > 0 else { return nil }
        var left = unread
        for i in list.indices.reversed() where countsAsUnread(list[i]) {
            left -= 1
            if left == 0 { return i }
        }
        return nil
    }

    /// B9：把已加载的整个范围（最早一条到最新）重新从数据库读一遍并替换。
    /// 逐页读取在后台线程进行；期间收到的事件先照常应用，读取结果替换后再重放一遍，避免用旧快照覆盖新状态。
    func refreshLoadedRange() {
        guard !refreshing, let first = messages.first else { mergeLatest(); return }
        // 只有一页：同步合并即可
        if messages.count <= Int(pageSize) { mergeLatest(); return }
        refreshing = true
        eventsDuringRefresh = []
        let pager = backgroundPager()
        let size = pageSize
        let oldest = first.sortTs
        let maxPages = messages.count / Int(size) + 3
        Task { [weak self] in
            let loaded = await Task.detached(priority: .userInitiated) {
                Self.loadRange(downTo: oldest, pager: pager, pageSize: size, maxPages: maxPages)
            }.value
            guard let self else { return }
            self.refreshing = false
            let replay = self.eventsDuringRefresh
            self.eventsDuringRefresh = []
            guard let loaded, self.token != nil else { return }
            let beforeLast = self.messages.last?.id
            if loaded != self.messages { self.messages = loaded }
            self.core.chats.noteFailed(loaded)
            for ev in replay { self.handle(ev) }
            if let last = self.messages.last?.id, last != beforeLast, self.atBottom { self.bottomTick += 1 }
        }
    }

    /// 从最新一页向上读到 sortTs <= oldest 为止，返回 sortTs >= oldest 的全部消息（升序）。读取失败返回 nil。
    nonisolated static func loadRange(downTo oldest: Int64, pager: (Int64?, UInt32) -> [MessageItem]?,
                                     pageSize: UInt32, maxPages: Int) -> [MessageItem]? {
        var out: [MessageItem] = []
        var seen = Set<String>()
        var cursor: Int64?
        for _ in 0..<max(maxPages, 1) {
            guard let page = pager(cursor, pageSize) else { return nil }
            let fresh = page.filter { !seen.contains($0.id) }
            for m in fresh { seen.insert(m.id) }
            out.insert(contentsOf: fresh, at: 0)
            guard let first = page.first, page.count >= Int(pageSize), first.sortTs > oldest,
                  cursor.map({ first.sortTs < $0 }) ?? true else { break }
            cursor = first.sortTs
        }
        return out.filter { $0.sortTs >= oldest }
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

    /// 已确认出现在屏幕内的最新一条消息（由聊天页按实际滚动位置确认）。
    /// 「停在底部」只是跟随的意图；已读、未读计数以这里为准。
    @ObservationIgnored private(set) var lastSeenId: String?

    func setAtBottom(_ v: Bool) {
        let changed = atBottom != v
        if changed { atBottom = v }
        if v {
            confirmLatestVisible()
        } else if changed {
            // 离开底部：在确认看到之后才到的消息都算没看到（用户正好在消息连续到达时滚走的情况）
            let n = Self.unseenCount(messages, after: lastSeenId)
            if unseenCount != n { unseenCount = n }
        }
    }

    /// 最新一条消息确实在屏幕内：清零「n 条新消息」，现在才标记已读（A3：停在历史位置时新消息不算已读）
    func confirmLatestVisible() {
        if unseenCount != 0 { unseenCount = 0 }
        let last = messages.last?.id
        guard lastSeenId != last else { return }
        lastSeenId = last
        markRead()
    }

    /// lastSeen 之后别人发来的消息数（lastSeen 不在列表里时为 0）
    nonisolated static func unseenCount(_ list: [MessageItem], after lastSeen: String?) -> Int {
        guard let lastSeen, let i = list.lastIndex(where: { $0.id == lastSeen }) else { return 0 }
        return list[(i + 1)...].filter { !$0.isOutgoing }.count
    }

    private func noteNewMessages(_ n: Int, outgoing: Bool) {
        if outgoing || atBottom { bottomTick += 1 } else { unseenCount += n }
    }

    /// 相邻两条消息是否跨天（按排序时间），与 messages 一一对应
    var dayBreaks: [Bool] { messages.map { dayStarts.contains($0.id) } }

    static func dayBreaks(_ list: [MessageItem]) -> [Bool] {
        let starts = DayBreaker.dayStartIds(list)
        return list.map { starts.contains($0.id) }
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
        core.chats.noteFailed(fresh)
        loadingOlder = false
    }

    func handle(_ ev: CoreEvent) {
        if refreshing { eventsDuringRefresh.append(ev) }
        switch ev {
        case let .messageAdded(cid, m, _, _) where cid == convId:
            // 离线补拉的旧消息插在中间：不滚动、不计入新消息
            if upsert(m, appendIfMissing: true) { noteNewMessages(1, outgoing: m.isOutgoing) }
            // 只有停在底部（新消息确实可见）时才标记已读并发回执
            if !m.isOutgoing { markRead() }
        case let .messageUpdated(cid, m) where cid == convId:
            upsert(m, appendIfMissing: false)
        case let .messageRemoved(cid, id) where cid == convId:
            withAnimation(.easeOut(duration: 0.35)) { messages.removeAll { $0.id == id } }
            selected.remove(id)
        case .conversationsChanged:
            // 清空聊天记录等批量变化：立即处理
            if messages.isEmpty == false,
               (loadPage(nil, 1) ?? []).isEmpty {
                messages = []
                unseenCount = 0
            } else {
                // 核心在事件积压过多时会丢弃逐条的消息事件、只补一个 ConversationsChanged；
                // 撤回 / 到期 / 已读等批量变化也可能只有这一个事件：稍后把当前页与数据库对齐一次
                scheduleResync()
            }
        default: break
        }
    }

    /// 收到 ConversationsChanged 后对齐当前页（合并 1 秒内的多次触发）。
    /// 已加载不多时对齐整个已加载范围；加载了很长的历史时只对齐最新一页（整段重读太重，离开再回来时会做）。
    private func scheduleResync() {
        guard token != nil, !resyncScheduled else { return }
        resyncScheduled = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.0) { [weak self] in
            MainActor.assumeIsolated {
                guard let self else { return }
                self.resyncScheduled = false
                guard self.token != nil, !self.messages.isEmpty, !self.locating else { return }
                if self.messages.count <= Self.fullResyncLimit { self.refreshLoadedRange() } else { self.mergeLatest() }
            }
        }
    }

    static let fullResyncLimit = 240

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
        guard visible, core.appActive, atBottom else { return }
        markReadWork?.cancel()
        let work = DispatchWorkItem { [weak self] in
            MainActor.assumeIsolated { self?.markReadNow() }
        }
        markReadWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.3, execute: work)
    }

    private func markReadNow() {
        // 只有最新一条已确认可见时才标记（核心的 mark_read 会把整个会话标为已读）
        guard visible, core.appActive, atBottom, lastSeenId == messages.last?.id else { return }
        let last = messages.last?.id
        let hasUnread = (core.chats.summary(convId)?.unreadCount ?? 0) > 0
            || (last != lastMarkedReadId && messages.contains { !$0.isOutgoing && $0.status != .read && isVisibleContent($0) })
        guard hasUnread else { return }
        lastMarkedReadId = last
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

    /// thumbnail 由调用方在后台线程生成（C4）
    func sendImage(url: URL, width: Int, height: Int, thumbnail: Data?) {
        let meta = AttachmentMeta(fileName: url.lastPathComponent, width: UInt32(width), height: UInt32(height),
                                  durationMs: 0, thumbnail: thumbnail)
        send(path: url.path, mime: "image/jpeg", kind: .image, meta: meta)
    }

    func sendVideo(_ v: PreparedVideo) {
        let meta = AttachmentMeta(fileName: v.url.lastPathComponent, width: UInt32(v.width), height: UInt32(v.height),
                                  durationMs: v.durationMs, thumbnail: v.thumbnail)
        send(path: v.url.path, mime: v.mime, kind: .video, meta: meta)
    }

    /// 依次发送相册里选中的多项（图片 / 视频）：逐条等待上一条发完再发下一条，保持选择顺序
    func sendMedia(_ items: [PreparedMedia]) {
        let cid = convId
        let chats = core.chats
        Task {
            for item in items {
                let path: String, mime: String, kind: AttachmentKind, meta: AttachmentMeta
                switch item {
                case let .image(url, w, h, thumb):
                    path = url.path; mime = "image/jpeg"; kind = .image
                    meta = AttachmentMeta(fileName: url.lastPathComponent, width: UInt32(w), height: UInt32(h), durationMs: 0, thumbnail: thumb)
                case let .video(v):
                    path = v.url.path; mime = v.mime; kind = .video
                    meta = AttachmentMeta(fileName: v.url.lastPathComponent, width: UInt32(v.width), height: UInt32(v.height),
                                          durationMs: v.durationMs, thumbnail: v.thumbnail)
                }
                do {
                    try await chats.sendAttachment(cid, path: path, mime: mime, kind: kind, meta: meta)
                } catch {
                    if !consumeCancelledSend(error) { core.toast = describe(error) }
                }
                AppPaths.discardOutbox(path)
            }
        }
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
        Task {
            // 核心读入后会在自己的目录保存副本（失败重发也用那份），outbox 里的明文临时文件随即删除
            defer { AppPaths.discardOutbox(path) }
            do {
                try await chats.sendAttachment(cid, path: path, mime: mime, kind: kind, meta: meta)
            } catch {
                if !consumeCancelledSend(error) { core.toast = describe(error) }
            }
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

    /// 发送中的附件被用户删除（取消发送）后，核心让对应的 sendAttachment 以 InvalidInput 结束：这不是错误，不提示
    private func consumeCancelledSend(_ error: Error) -> Bool {
        guard cancelledSends > 0, case HushError.InvalidInput = error else { return false }
        cancelledSends -= 1
        return true
    }

    private func noteCancelIfSending(_ m: MessageItem) {
        if m.isOutgoing, m.status == .sending, case .attachments = m.body { cancelledSends += 1 }
    }

    func deleteLocal(_ m: MessageItem) {
        do {
            try core.chats.deleteLocal(m.id)
            noteCancelIfSending(m)
            withAnimation { messages.removeAll { $0.id == m.id } }
        } catch { core.toast = describe(error) }
    }

    // MARK: - 多选

    /// 可以被选中的消息：气泡消息（文字、附件、无法解密）
    nonisolated static func selectable(_ m: MessageItem) -> Bool {
        switch m.body {
        case .text, .attachments, .undecryptable: return true
        default: return false
        }
    }

    /// 可以转发：未撤回的文字，或已上传的附件（发送中 / 失败的附件还没有服务端密文）
    nonisolated static func forwardable(_ m: MessageItem) -> Bool {
        guard !m.deleted else { return false }
        switch m.body {
        case .text: return true
        case .attachments: return !(m.isOutgoing && (m.status == .sending || m.status == .failed))
        default: return false
        }
    }

    func beginSelecting(_ m: MessageItem) {
        selecting = true
        selected = Self.selectable(m) ? [m.id] : []
    }

    func toggleSelected(_ m: MessageItem) {
        guard Self.selectable(m) else { return }
        if selected.contains(m.id) { selected.remove(m.id) } else { selected.insert(m.id) }
    }

    func endSelecting() {
        selecting = false
        selected = []
    }

    /// 已选中的消息，按时间顺序
    var selectedMessages: [MessageItem] { messages.filter { selected.contains($0.id) } }

    /// 批量删除（仅本机）
    func deleteSelected() {
        let targets = selectedMessages
        var failed = 0
        for m in targets {
            do { try core.chats.deleteLocal(m.id); noteCancelIfSending(m) } catch { failed += 1 }
        }
        let ids = Set(targets.map(\.id))
        withAnimation { messages.removeAll { ids.contains($0.id) } }
        endSelecting()
        core.toast = failed == 0 ? "已删除 \(targets.count) 条消息" : "有 \(failed) 条消息删除失败"
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
