import SwiftUI

/// 聊天页
struct ChatView: View {
    let convId: String
    /// 从消息搜索进入：首次加载后定位并高亮这条消息
    let focusMessageId: String?
    @State private var model: ChatModel
    @State private var recorder = VoiceRecorder()
    @State private var actionTarget: MessageItem?
    @State private var forwardTarget: MessageItem?
    @State private var pendingFocus: String?
    @State private var viewer: ImageViewerItem?
    @State private var shareURL: IdentifiableURL?
    @State private var highlightId: String?
    @State private var didInitialScroll = false
    /// 正在进行的定位（搜索结果 / 引用跳转）；再次定位或离开页面时取消
    @State private var locateTask: Task<Void, Never>?
    var core = HushCore.shared

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
                                model.loadOlder()
                                if let anchor { proxy.scrollTo(anchor, anchor: .top) }
                            }
                    }
                    if !model.isGroup {
                        EncryptionNotice()
                    }
                    // 同一份快照里同时取消息与分隔标记，避免在删除动画中读到错位的 messages[i-1]
                    let items = model.messages
                    let breaks = ChatModel.dayBreaks(items)
                    ForEach(Array(zip(items, breaks)), id: \.0.id) { m, showDay in
                        if showDay {
                            DaySeparator(ts: m.sortTs)
                        }
                        MessageRow(
                            message: m, isGroup: model.isGroup,
                            onLongPress: { actionTarget = m },
                            onQuoteTap: { id in scrollToQuoted(id, proxy) },
                            onOpenImage: { p in openImage(p, in: m) },
                            onShare: { url in shareURL = IdentifiableURL(url: url) },
                            onResend: { model.resend(m) }
                        )
                        .background(highlightId == m.id ? Theme.accent.opacity(0.15) : Color.clear)
                        .id(m.id)
                        .transition(.opacity)
                    }
                    Color.clear.frame(height: 4).id("bottom")
                        .onAppear { model.setAtBottom(true) }
                        .onDisappear { model.setAtBottom(false) }
                }
                .padding(.vertical, 8)
            }
            .defaultScrollAnchor(.bottom)
            .scrollDismissesKeyboard(.interactively)
            .background(Theme.background)
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
            .overlay(alignment: .bottomTrailing) {
                if model.unseenCount > 0 {
                    Button {
                        withAnimation(.easeOut(duration: 0.2)) { proxy.scrollTo("bottom", anchor: .bottom) }
                        model.setAtBottom(true)
                    } label: {
                        Label("\(model.unseenCount) 条新消息", systemImage: "arrow.down")
                            .font(.system(size: 13, weight: .medium))
                            .foregroundStyle(Theme.onAccent)
                            .padding(.horizontal, 12).padding(.vertical, 7)
                            .background(Capsule().fill(Theme.accent))
                            .shadow(color: .black.opacity(0.15), radius: 4, y: 1)
                    }
                    .padding(.trailing, 14).padding(.bottom, 10)
                    .accessibilityIdentifier("newMessagesPill")
                }
            }
            // 停在底部时弹出键盘：可视区域变矮，跟着滚到底部，否则最新的消息会被键盘挡住
            .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillShowNotification)) { _ in
                guard model.atBottom, didInitialScroll else { return }
                for delay in [0.05, 0.35] {
                    DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
                        withAnimation(.easeOut(duration: 0.2)) { proxy.scrollTo("bottom", anchor: .bottom) }
                    }
                }
            }
            .onChange(of: model.bottomTick) { _, _ in
                withAnimation(didInitialScroll ? .easeOut(duration: 0.2) : nil) { proxy.scrollTo("bottom", anchor: .bottom) }
                didInitialScroll = true
            }
            .onAppear {
                // 只在首次进入时滚到底部；从设置页等返回时保持原来的阅读位置
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
                    proxy.scrollTo("bottom", anchor: .bottom)
                    didInitialScroll = true
                }
            }
        }
        .safeAreaInset(edge: .bottom, spacing: 0) {
            if canSend {
                ChatInputBar(model: model, recorder: recorder, members: group?.members.map(\.userId) ?? [])
            } else {
                Text(model.isGroup ? "你已不在此群聊中" : "无法发送消息")
                    .font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                    .frame(maxWidth: .infinity).padding(14).background(Theme.surface)
            }
        }
        .overlay {
            if recorder.isRecording { RecordingOverlay(recorder: recorder) }
        }
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .principal) { header }
            ToolbarItemGroup(placement: .topBarTrailing) {
                if let peer = model.peerId {
                    Button { CallManager.shared.startCall(to: peer, video: true) } label: { Image(systemName: "video") }
                        .accessibilityIdentifier("videoCallButton")
                    Button { CallManager.shared.startCall(to: peer, video: false) } label: { Image(systemName: "phone") }
                        .accessibilityIdentifier("voiceCallButton")
                }
                NavigationLink(value: Route.convSettings(convId)) { Image(systemName: "ellipsis.circle") }
                    .accessibilityIdentifier("chatMoreButton")
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
        .sheet(item: $actionTarget, onDismiss: {
            // 菜单完全收起后再弹出转发选择，避免两个 sheet 叠在一起
            if let m = model.pendingForward {
                model.pendingForward = nil
                forwardTarget = m
            }
        }) { m in MessageActionSheet(message: m, model: model) }
        .sheet(item: $forwardTarget) { m in ForwardSheet(message: m) }
        .fullScreenCover(item: $viewer) { v in ImageViewer(paths: v.paths, index: v.index) }
        .sheet(item: $shareURL, onDismiss: { FileAttachmentView.clearShareCopies() }) { u in
            ShareSheet(items: [u.url]).presentationDetents([.medium, .large])
        }
    }

    private var header: some View {
        HStack(spacing: 8) {
            ConversationAvatar(convId: convId, size: 32)
            VStack(alignment: .leading, spacing: 0) {
                HStack(spacing: 4) {
                    Text(title).font(.system(size: 16, weight: .semibold)).lineLimit(1)
                        .accessibilityIdentifier("chatTitle")
                    if let peer = model.peerId, core.contacts.verifiedUsers.contains(peer) {
                        Image(systemName: "checkmark.shield.fill").font(.system(size: 12)).foregroundStyle(Theme.success)
                            .accessibilityIdentifier("verifiedShield")
                    }
                    if summary?.muted == true {
                        Image(systemName: "bell.slash.fill").font(.system(size: 11)).foregroundStyle(Theme.textSecondary)
                    }
                }
                if let sub = subtitle {
                    Text(sub).font(.system(size: 12)).foregroundStyle(isTyping ? Theme.accent : Theme.textSecondary)
                        .accessibilityIdentifier("chatSubtitle")
                }
            }
        }
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
            let found = await model.locate(id)
            guard !Task.isCancelled else { return }
            locateTask = nil
            guard found else {
                core.toast = notFound
                return
            }
            // 刚插入的更早消息要等 LazyVStack 布局后才能准确滚动：分两次滚动
            DispatchQueue.main.async { proxy.scrollTo(id, anchor: .center) }
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { withAnimation { proxy.scrollTo(id, anchor: .center) } }
            highlightId = id
            DispatchQueue.main.asyncAfter(deadline: .now() + 2.5) {
                if highlightId == id { withAnimation { highlightId = nil } }
            }
        }
    }

    private func openImage(_ path: String, in m: MessageItem) {
        // 收集会话里所有已下载图片，支持左右滑动
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

struct EncryptionNotice: View {
    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: "lock.fill").font(.system(size: 11))
            Text("消息已端到端加密，只有你们双方可以读取").font(.system(size: 12))
        }
        .foregroundStyle(Theme.textSecondary)
        .padding(.horizontal, 12).padding(.vertical, 6)
        .background(RoundedRectangle(cornerRadius: 10).fill(Theme.surface))
        .padding(.vertical, 8)
    }
}

extension MessageItem: Identifiable {}
