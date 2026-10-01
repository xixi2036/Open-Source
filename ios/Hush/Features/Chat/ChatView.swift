import SwiftUI

/// 聊天页滚动位置的跟踪（A3）。引用类型、不参与观察：滚动时每帧都会更新，不能因此重绘整个列表。
final class ChatScrollTracker {
    /// 底部锚点顶边的全局 Y（未渲染时为 nil，说明离底部很远）
    var sentinelY: CGFloat?
    /// 输入栏顶边的全局 Y（可视区域的下边界；键盘弹出时上移）
    var barTop: CGFloat = .greatestFiniteMagnitude
    var contentHeight: CGFloat = 0
    /// 自上次判定以来，内容高度或可视区域发生过变化（新消息、键盘、图片占位变化）
    var layoutChanged = false
    /// 程序发起的滚动（滚到底部）预计结束的时间
    var programmaticUntil = Date.distantPast
    /// 最近一次检测到用户手指拖动列表的时间（拖动手势被滚动视图接管后不一定有结束回调，所以只记时间）
    var lastDragAt = Date.distantPast
    var evaluateScheduled = false
    var scrollScheduled = false
    var lastScrollAt = Date.distantPast

    static let threshold: CGFloat = 60

    /// 底部锚点到可视区域下边界的距离（<= 0 表示锚点已在屏幕内）
    var distance: CGFloat? { sentinelY.map { $0 - barTop } }

    /// 用户正在拖动，或刚松手仍在惯性滚动
    func userScrolling(now: Date = Date()) -> Bool { now.timeIntervalSince(lastDragAt) < 1.0 }

    /// 纯逻辑：是否「停在底部」。
    /// - 锚点在可视区域附近 → 是；
    /// - 用户正在滚动且锚点不在附近 → 否（用户主动离开了底部）；
    /// - 距离变大是因为内容变高 / 可视区域变矮 / 程序滚动还没结束 → 保持原状态（A3 的根因：
    ///   连续到达的消息把锚点顶出屏幕，曾被当成「用户离开了底部」，之后就不再跟随）；
    /// - 其余情况（位置变了但布局没变）→ 否。
    static func pinned(current: Bool, distance: CGFloat?, userScrolling: Bool, layoutChanged: Bool, programmatic: Bool) -> Bool {
        if let d = distance, d <= threshold { return true }
        if userScrolling { return false }
        if layoutChanged || programmatic { return current }
        return false
    }
}

/// 聊天页
struct ChatView: View {
    let convId: String
    /// 从消息搜索进入：首次加载后定位并高亮这条消息
    let focusMessageId: String?
    @State private var model: ChatModel
    @State private var recorder = VoiceRecorder()
    @State private var actionTarget: MessageItem?
    @State private var forwardTargets: ForwardItems?
    @State private var selectTextTarget: MessageItem?
    @State private var pendingFocus: String?
    @State private var viewer: ImageViewerItem?
    @State private var video: VideoItem?
    @State private var shareURL: IdentifiableURL?
    @State private var highlightId: String?
    @State private var didInitialScroll = false
    @State private var tracker = ChatScrollTracker()
    @State private var pendingLink: URL?
    @State private var confirmDeleteSelected = false
    /// 正在进行的定位（搜索结果 / 引用跳转）；再次定位或离开页面时取消
    @State private var locateTask: Task<Void, Never>?
    var core = HushCore.shared

    static let unreadSeparatorId = "unreadSeparator"

    init(convId: String, focusMessageId: String? = nil) {
        self.convId = convId
        self.focusMessageId = focusMessageId
        _pendingFocus = State(initialValue: focusMessageId)
        _model = State(initialValue: ChatModel(convId: convId))
    }

    private var summary: ConversationSummary? { core.chats.summary(convId) }
    private var group: GroupInfo? { model.groupId.flatMap { core.contacts.group($0) } }
    private var canSend: Bool {
        if let g = group { return g.active && g.members.contains { $0.userId == core.myId } }
        if let s = summary, model.isGroup { return s.active }
        return true
    }

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(spacing: 2) {
                    if model.hasMore {
                        ProgressView()
                            .frame(maxWidth: .infinity).padding(8)
                            .onAppear {
                                guard didInitialScroll else { return }
                                let anchor = model.messages.first?.id
                                tracker.layoutChanged = true
                                model.loadOlder()
                                if let anchor { proxy.scrollTo(anchor, anchor: .top) }
                            }
                    }
                    if !model.isGroup {
                        EncryptionNotice()
                    }
                    // 日期分隔与「以下为新消息」的位置都来自模型里缓存的结果（C2），这里只查表
                    let dayStarts = model.dayStarts
                    let firstUnread = model.firstUnreadId
                    let selecting = model.selecting
                    ForEach(model.messages) { m in
                        if dayStarts.contains(m.id) {
                            DaySeparator(ts: m.sortTs)
                        }
                        if m.id == firstUnread {
                            UnreadSeparator().id(Self.unreadSeparatorId)
                        }
                        MessageRow(
                            message: m, isGroup: model.isGroup,
                            selecting: selecting, isSelected: selecting && model.selected.contains(m.id),
                            onLongPress: { actionTarget = m },
                            onQuoteTap: { id in scrollToQuoted(id, proxy) },
                            onOpenImage: { p in openImage(p, in: m) },
                            onOpenVideo: { p in video = VideoItem(path: p) },
                            onShare: { url in shareURL = IdentifiableURL(url: url) },
                            onResend: { model.resend(m) },
                            onToggleSelect: { model.toggleSelected(m) }
                        )
                        .equatable()
                        .background(highlightId == m.id ? Theme.accent.opacity(0.15) : Color.clear)
                        .id(m.id)
                        .transition(.opacity)
                    }
                    Color.clear.frame(height: 4).id("bottom")
                        .background(GeometryReader { g in
                            let y = g.frame(in: .global).minY
                            Color.clear
                                .onChange(of: y, initial: true) { _, v in tracker.sentinelY = v; scheduleEvaluate(proxy) }
                        })
                        .onDisappear { tracker.sentinelY = nil; scheduleEvaluate(proxy) }
                }
                .padding(.vertical, 8)
                .background(GeometryReader { g in
                    Color.clear.onChange(of: g.size.height, initial: true) { _, h in
                        tracker.contentHeight = h
                        tracker.layoutChanged = true
                        scheduleEvaluate(proxy)
                    }
                })
            }
            .defaultScrollAnchor(.bottom)
            .scrollDismissesKeyboard(.interactively)
            .background(Theme.background)
            // 记录用户是否在拖动列表：只有用户自己滚走才算「离开底部」
            .simultaneousGesture(
                DragGesture(minimumDistance: 6)
                    .onChanged { _ in
                        tracker.lastDragAt = Date()
                        scheduleEvaluate(proxy)
                    }
                    .onEnded { _ in
                        tracker.lastDragAt = Date()
                        // 惯性滚动结束后再判定一次
                        DispatchQueue.main.asyncAfter(deadline: .now() + 1.05) { scheduleEvaluate(proxy) }
                    }
            )
            .overlay(alignment: .top) {
                if model.locating {
                    HStack(spacing: 8) {
                        ProgressView()
                        Text("正在定位消息…").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                        Button("取消") { locateTask?.cancel() }
                            .font(Fonts.secondary)
                            .accessibilityIdentifier("cancelLocate")
                    }
                    .padding(.horizontal, 14).padding(.vertical, 8)
                    .background(Capsule().fill(Theme.surface))
                    .shadow(color: .black.opacity(0.1), radius: 4, y: 1)
                    .padding(.top, 8)
                    .transition(.opacity)
                    .accessibilityIdentifier("locatingIndicator")
                }
            }
            .overlay(alignment: .topTrailing) {
                if let mid = model.mentionJumpId, !model.locating {
                    Button {
                        model.mentionJumpId = nil
                        jump(to: mid, proxy, notFound: "该消息已不存在")
                    } label: {
                        Label("有人@我", systemImage: "at")
                            .font(Fonts.sized(13, weight: .medium))
                            .foregroundStyle(Theme.accent)
                            .padding(.horizontal, 12).padding(.vertical, 7)
                            .background(Capsule().fill(Theme.surface))
                            .shadow(color: .black.opacity(0.15), radius: 4, y: 1)
                    }
                    .padding(.trailing, 14).padding(.top, 10)
                    .accessibilityIdentifier("mentionJumpPill")
                }
            }
            .overlay(alignment: .bottomTrailing) {
                if model.unseenCount > 0 {
                    Button {
                        model.setAtBottom(true)
                        scrollToBottom(proxy, animated: true)
                    } label: {
                        Label("\(model.unseenCount) 条新消息", systemImage: "arrow.down")
                            .font(Fonts.sized(13, weight: .medium))
                            .foregroundStyle(Theme.onAccent)
                            .padding(.horizontal, 12).padding(.vertical, 7)
                            .background(Capsule().fill(Theme.accent))
                            .shadow(color: .black.opacity(0.15), radius: 4, y: 1)
                    }
                    .padding(.trailing, 14).padding(.bottom, 10)
                    .accessibilityIdentifier("newMessagesPill")
                }
            }
            .onChange(of: model.bottomTick) { _, _ in
                if !didInitialScroll {
                    // 首次加载：有未读时由 onAppear 定位到第一条未读，这里不抢着滚到底
                    if model.firstUnreadId != nil || pendingFocus != nil || focusMessageId != nil { return }
                    didInitialScroll = true
                    scrollToBottom(proxy, animated: false)
                    return
                }
                scrollToBottom(proxy, animated: true)
            }
            .onAppear {
                // 只在首次进入时定位；从设置页等返回时保持原来的阅读位置
                if let focus = pendingFocus {
                    // 从消息搜索进入：首屏加载完成后定位到该消息（只做一次）
                    pendingFocus = nil
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.35) {
                        didInitialScroll = true
                        jump(to: focus, proxy, notFound: "该消息已不存在")
                    }
                    return
                }
                guard !didInitialScroll else { return }
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) {
                    if model.firstUnreadId != nil {
                        // 定位到第一条未读：「以下为新消息」贴着顶部；未读不满一屏时自然停在底部
                        proxy.scrollTo(Self.unreadSeparatorId, anchor: .top)
                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.1) { proxy.scrollTo(Self.unreadSeparatorId, anchor: .top) }
                    } else {
                        tracker.programmaticUntil = Date().addingTimeInterval(0.4)
                        proxy.scrollTo("bottom", anchor: .bottom)
                    }
                    didInitialScroll = true
                    // 定位完成后按实际位置判定是否在底部（在底部才标记已读）
                    DispatchQueue.main.asyncAfter(deadline: .now() + 0.45) { evaluate(proxy, force: true) }
                }
            }
            .safeAreaInset(edge: .bottom, spacing: 0) {
                bottomBar
                    .background(GeometryReader { g in
                        let y = g.frame(in: .global).minY
                        Color.clear.onChange(of: y, initial: true) { _, v in
                            tracker.barTop = v
                            tracker.layoutChanged = true
                            scheduleEvaluate(proxy)
                        }
                    })
            }
        }
        .overlay {
            if recorder.isRecording { RecordingOverlay(recorder: recorder) }
        }
        .navigationBarTitleDisplayMode(.inline)
        .navigationBarBackButtonHidden(model.selecting)
        .toolbar {
            if model.selecting {
                ToolbarItem(placement: .topBarLeading) {
                    Button("取消") { model.endSelecting() }.accessibilityIdentifier("cancelSelecting")
                }
                ToolbarItem(placement: .principal) {
                    Text("已选择 \(model.selected.count) 条").font(Fonts.sized(16, weight: .semibold))
                        .accessibilityIdentifier("selectionCount")
                }
            } else {
                ToolbarItem(placement: .principal) { header }
                ToolbarItemGroup(placement: .topBarTrailing) {
                    if let peer = model.peerId {
                        Button { CallManager.shared.startCall(to: peer, video: true) } label: { Image(systemName: "video") }
                            .accessibilityLabel("视频通话")
                            .accessibilityIdentifier("videoCallButton")
                        Button { CallManager.shared.startCall(to: peer, video: false) } label: { Image(systemName: "phone") }
                            .accessibilityLabel("语音通话")
                            .accessibilityIdentifier("voiceCallButton")
                    }
                    NavigationLink(value: Route.convSettings(convId)) { Image(systemName: "ellipsis.circle") }
                        .accessibilityLabel("聊天信息")
                        .accessibilityIdentifier("chatMoreButton")
                }
            }
        }
        .toolbar(.hidden, for: .tabBar)
        .onAppear { model.onAppear() }
        .onDisappear {
            recorder.cancel()
            // 离开页面（返回、进入设置页）即取消后台定位
            locateTask?.cancel()
            locateTask = nil
            model.onDisappear()
        }
        .onChange(of: core.appActive) { _, active in
            if active { model.markRead() } else { recorder.cancel(); model.saveDraft() }
        }
        .task {
            if let peer = model.peerId { await core.contacts.refreshVerified(peer) }
        }
        // 消息里的链接：https 直接交给系统打开；http 先确认；其他协议不打开
        .environment(\.openURL, OpenURLAction { url in
            switch LinkPolicy.decision(url) {
            case .open: return .systemAction
            case .confirm:
                pendingLink = url
                return .handled
            case .block: return .discarded
            }
        })
        .alert("打开未加密的链接？", isPresented: Binding(get: { pendingLink != nil }, set: { if !$0 { pendingLink = nil } }),
               presenting: pendingLink) { url in
            Button("取消", role: .cancel) {}
            Button("打开") { UIApplication.shared.open(url) }.accessibilityIdentifier("confirmOpenLink")
        } message: { url in
            Text("\(url.absoluteString)\n\n该链接使用 http，访问内容可能被窃听或篡改。")
        }
        .sheet(item: $actionTarget, onDismiss: {
            // 菜单完全收起后再弹出下一个面板，避免两个 sheet 叠在一起
            if let m = model.pendingForward {
                model.pendingForward = nil
                forwardTargets = ForwardItems(messages: [m], fromSelection: false)
            } else if let m = model.pendingSelectText {
                model.pendingSelectText = nil
                selectTextTarget = m
            }
        }) { m in MessageActionSheet(message: m, model: model) }
        .sheet(item: $forwardTargets) { f in
            ForwardSheet(messages: f.messages, onDone: { if f.fromSelection { model.endSelecting() } })
        }
        .sheet(item: $selectTextTarget) { m in
            if case let .text(t) = m.body { TextSelectSheet(text: t) }
        }
        .fullScreenCover(item: $viewer) { v in ImageViewer(paths: v.paths, index: v.index) }
        .fullScreenCover(item: $video) { v in VideoPlayerScreen(path: v.path) }
        .sheet(item: $shareURL, onDismiss: { FileAttachmentView.clearShareCopies() }) { u in
            ShareSheet(items: [u.url]).presentationDetents([.medium, .large])
        }
        .confirmationDialog("删除选中的 \(model.selected.count) 条消息？只会从本机删除。",
                            isPresented: $confirmDeleteSelected, titleVisibility: .visible) {
            Button("删除", role: .destructive) { model.deleteSelected() }
                .accessibilityIdentifier("confirmDeleteSelected")
        }
    }

    // MARK: - 底部栏

    @ViewBuilder private var bottomBar: some View {
        if model.selecting {
            selectionBar
        } else if canSend {
            ChatInputBar(model: model, recorder: recorder, members: group?.members.map(\.userId) ?? [])
        } else {
            Text(model.isGroup ? "你已不在此群聊中" : "无法发送消息")
                .font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                .frame(maxWidth: .infinity).padding(14).background(Theme.surface)
        }
    }

    /// 多选模式的底部操作栏：批量转发 / 批量删除
    private var selectionBar: some View {
        let picked = model.selectedMessages
        let forwardable = picked.filter(ChatModel.forwardable)
        return HStack {
            Button {
                if forwardable.count < picked.count {
                    core.toast = "已跳过 \(picked.count - forwardable.count) 条不能转发的消息"
                }
                forwardTargets = ForwardItems(messages: forwardable, fromSelection: true)
            } label: {
                Label("转发", systemImage: "arrowshape.turn.up.right").frame(maxWidth: .infinity, minHeight: 44)
            }
            .disabled(forwardable.isEmpty)
            .accessibilityIdentifier("forwardSelected")
            Button(role: .destructive) { confirmDeleteSelected = true } label: {
                Label("删除", systemImage: "trash").frame(maxWidth: .infinity, minHeight: 44)
            }
            .tint(Theme.danger)
            .disabled(picked.isEmpty)
            .accessibilityIdentifier("deleteSelected")
        }
        .font(Fonts.body)
        .padding(.horizontal, 12).padding(.vertical, 6)
        .background(Theme.background)
        .overlay(alignment: .top) { Rectangle().fill(Theme.divider).frame(height: 0.5) }
    }

    // MARK: - 滚动位置

    /// 合并同一轮里的多次测量（内容高度、锚点位置、输入栏位置各自回调），统一判定一次
    private func scheduleEvaluate(_ proxy: ScrollViewProxy) {
        guard !tracker.evaluateScheduled else { return }
        tracker.evaluateScheduled = true
        DispatchQueue.main.async {
            tracker.evaluateScheduled = false
            evaluate(proxy)
        }
    }

    private func evaluate(_ proxy: ScrollViewProxy, force: Bool = false) {
        guard didInitialScroll else { return }
        let now = Date()
        let layoutChanged = tracker.layoutChanged
        tracker.layoutChanged = false
        let pinned = ChatScrollTracker.pinned(
            current: force ? false : model.atBottom, distance: tracker.distance,
            userScrolling: tracker.userScrolling(now: now),
            layoutChanged: force ? false : layoutChanged, programmatic: force ? false : now < tracker.programmaticUntil)
        if pinned != model.atBottom { model.setAtBottom(pinned) }
        // 最新一条确实在屏幕内：记为已看到（此时才标记已读）
        if let d = tracker.distance, d <= ChatScrollTracker.threshold { model.confirmLatestVisible() }
        // 仍停在底部但锚点被顶出了可视区域（新消息、键盘弹出）：跟过去
        if pinned, !tracker.userScrolling(now: now), (tracker.distance ?? .greatestFiniteMagnitude) > 4 {
            scrollToBottom(proxy, animated: false)
        }
    }

    /// 滚到底部。同一轮里的多次请求只滚一次；消息连续到达时不做动画（动画没结束下一条又来了）
    private func scrollToBottom(_ proxy: ScrollViewProxy, animated: Bool) {
        tracker.programmaticUntil = Date().addingTimeInterval(0.45)
        guard !tracker.scrollScheduled else { return }
        tracker.scrollScheduled = true
        DispatchQueue.main.async {
            tracker.scrollScheduled = false
            let now = Date()
            let burst = now.timeIntervalSince(tracker.lastScrollAt) < 0.3
            tracker.lastScrollAt = now
            tracker.programmaticUntil = now.addingTimeInterval(0.45)
            withAnimation(animated && !burst ? .easeOut(duration: 0.2) : nil) { proxy.scrollTo("bottom", anchor: .bottom) }
            // 刚追加的行要等 LazyVStack 布局后才有准确位置：稍后按实际位置复查一次
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { scheduleEvaluate(proxy) }
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            ConversationAvatar(convId: convId, size: 32)
            VStack(alignment: .leading, spacing: 0) {
                HStack(spacing: 4) {
                    Text(title).font(Fonts.sized(16, weight: .semibold)).lineLimit(1)
                        .accessibilityIdentifier("chatTitle")
                    if let peer = model.peerId, core.contacts.verifiedUsers.contains(peer) {
                        Image(systemName: "checkmark.shield.fill").font(.system(size: 12)).foregroundStyle(Theme.success)
                            .accessibilityLabel("已核验")
                            .accessibilityIdentifier("verifiedShield")
                    }
                    if summary?.muted == true {
                        Image(systemName: "bell.slash.fill").font(.system(size: 11)).foregroundStyle(Theme.textSecondary)
                            .accessibilityLabel("已免打扰")
                    }
                }
                if let sub = subtitle {
                    Text(sub).font(Fonts.sized(12)).foregroundStyle(isTyping ? Theme.accent : Theme.textSecondary)
                        .accessibilityIdentifier("chatSubtitle")
                }
            }
        }
        .dynamicTypeSize(...DynamicTypeSize.accessibility1)   // 导航栏高度固定，标题最多放大到 AX1
        .onTapGesture { Router.shared.push(.convSettings(convId)) }
    }

    private var title: String {
        if let g = group { return "\(g.name) (\(g.members.count))" }
        return core.chats.title(convId)
    }

    private var isTyping: Bool { !core.chats.typingUsers(convId).filter { $0 != core.myId }.isEmpty }

    private var subtitle: String? {
        let typers = core.chats.typingUsers(convId).filter { $0 != core.myId }
        if !typers.isEmpty {
            return model.isGroup ? "\(core.contacts.displayName(typers[0])) 正在输入…" : "正在输入…"
        }
        if let s = summary, s.disappearingSecs > 0 { return "⏱ \(Fmt.disappearingLabel(s.disappearingSecs))" }
        return nil
    }

    private func scrollToQuoted(_ id: String, _ proxy: ScrollViewProxy) {
        jump(to: id, proxy, notFound: "原消息已不存在")
    }

    /// 定位到某条消息：不在已加载范围内时在后台向上翻页直到找到（页面顶部显示加载指示，可取消），
    /// 找到后回到主线程滚到屏幕中间并短暂高亮；找不到给出提示，被取消则什么也不做
    private func jump(to id: String, _ proxy: ScrollViewProxy, notFound: String) {
        let previous = locateTask
        previous?.cancel()
        locateTask = Task { @MainActor in
            await previous?.value
            tracker.layoutChanged = true
            let found = await model.locate(id)
            guard !Task.isCancelled else { return }
            locateTask = nil
            guard found else {
                core.toast = notFound
                return
            }
            // 跳到历史位置：不再视为停在底部
            model.setAtBottom(false)
            // 刚插入的更早消息要等 LazyVStack 布局后才能准确滚动：分两次滚动
            DispatchQueue.main.async { proxy.scrollTo(id, anchor: .center) }
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { withAnimation { proxy.scrollTo(id, anchor: .center) } }
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.9) { evaluate(proxy, force: true) }
            highlightId = id
            DispatchQueue.main.asyncAfter(deadline: .now() + 2.5) {
                if highlightId == id { withAnimation { highlightId = nil } }
            }
        }
    }

    private func openImage(_ path: String, in m: MessageItem) {
        // 收集会话里所有已下载图片，支持左右滑动（查看器只解码当前页和前后各一页，C5）
        var paths: [String] = []
        for msg in model.messages {
            if case let .attachments(items, _) = msg.body {
                for it in items where it.kind == .image { if let p = it.existingPath { paths.append(p) } }
            }
        }
        if !paths.contains(path) { paths = [path] }
        viewer = ImageViewerItem(paths: paths, index: paths.firstIndex(of: path) ?? 0)
    }
}

/// 转发面板要转发的消息
struct ForwardItems: Identifiable {
    let id = UUID()
    let messages: [MessageItem]
    /// 来自多选：转发完成后退出多选模式
    let fromSelection: Bool
}

/// 「以下为新消息」分隔条：进入会话时第一条未读消息的上方
struct UnreadSeparator: View {
    var body: some View {
        HStack(spacing: 8) {
            Rectangle().fill(Theme.accent.opacity(0.35)).frame(height: 1)
            Text("以下为新消息").font(Fonts.timestamp).foregroundStyle(Theme.accent)
                .fixedSize()
            Rectangle().fill(Theme.accent.opacity(0.35)).frame(height: 1)
        }
        .padding(.horizontal, 16).padding(.vertical, 8)
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("unreadSeparator")
    }
}

struct EncryptionNotice: View {
    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: "lock.fill").font(.system(size: 11))
            Text("消息已端到端加密，只有你们双方可以读取").font(Fonts.sized(12))
        }
        .foregroundStyle(Theme.textSecondary)
        .padding(.horizontal, 12).padding(.vertical, 6)
        .background(RoundedRectangle(cornerRadius: 10).fill(Theme.surface))
        .padding(.vertical, 8)
    }
}

extension MessageItem: Identifiable {}
