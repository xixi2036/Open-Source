import SwiftUI

/// 朋友圈 Tab
struct MomentsView: View {
    @State private var model = MomentsModel()
    @State private var showCompose = false
    @State private var viewer: ImageViewerItem?
    var core = HushCore.shared

    var body: some View {
        MomentsFeed(model: model, header: AnyView(header), onOpenImages: { viewer = ImageViewerItem(paths: $0, index: $1) })
            .navigationTitle("朋友圈")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { showCompose = true } label: { Image(systemName: "camera.fill") }
                        .accessibilityIdentifier("composeMoment")
                }
            }
            .task { if !model.loaded { await model.refresh() } }
            .sheet(isPresented: $showCompose) {
                ComposeMomentView { m in model.inserted(m) }
            }
            .fullScreenCover(item: $viewer) { v in ImageViewer(paths: v.paths, index: v.index) }
    }

    private var header: some View {
        VStack(spacing: 0) {
            MomentsCover(userId: core.myId, name: core.myProfile?.name ?? "", avatarId: core.myProfile?.avatarId)
            if core.moments.newNoteCount > 0 {
                NavigationLink(value: Route.momentNotifications) {
                    HStack(spacing: 8) {
                        if let n = core.moments.notes.first {
                            AvatarView(seed: n.from.id, name: n.from.name, avatarId: n.from.avatarId, size: 26)
                        }
                        Text("\(core.moments.newNoteCount) 条新消息").font(.system(size: 14, weight: .medium)).foregroundStyle(.white)
                    }
                    .padding(.horizontal, 12).padding(.vertical, 8)
                    .background(RoundedRectangle(cornerRadius: 8).fill(Color(white: 0.25)))
                }
                .accessibilityIdentifier("newMomentNotes")
                .padding(.top, 36)
            } else if !core.moments.notes.isEmpty {
                NavigationLink(value: Route.momentNotifications) {
                    Text("查看消息记录").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                }
                .accessibilityIdentifier("momentNotesHistory")
                .padding(.top, 36)
            }
        }
    }
}

/// 封面区：accent 渐变 + 右下角头像昵称
struct MomentsCover: View {
    let userId: String
    let name: String
    let avatarId: String?

    var body: some View {
        ZStack(alignment: .bottomTrailing) {
            LinearGradient(colors: [Theme.accent, Theme.accent.opacity(0.55), Color(rgb: 0x7FA7E8)],
                           startPoint: .topLeading, endPoint: .bottomTrailing)
                .frame(height: 280)
                .overlay(alignment: .topLeading) {
                    Image(systemName: "sparkles").font(.system(size: 80)).foregroundStyle(.white.opacity(0.15)).padding(30)
                }
            HStack(alignment: .bottom, spacing: 12) {
                Text(name).font(.system(size: 18, weight: .semibold)).foregroundStyle(.white)
                    .shadow(radius: 2).padding(.bottom, 26)
                AvatarView(seed: userId, name: name, avatarId: avatarId, size: 72)
                    .overlay(Circle().stroke(.white, lineWidth: 2))
            }
            .padding(.trailing, 16)
            .offset(y: 24)
        }
        .onTapGesture { if !userId.isEmpty { Router.shared.push(.userMoments(userId)) } }
    }
}

/// 动态流列表（首页与个人相册共用）：下拉刷新、上滑加载、评论输入栏
struct MomentsFeed: View {
    @Bindable var model: MomentsModel
    let header: AnyView
    /// 有封面时内容延伸到导航栏下方
    var extendsUnderTop = true
    var onOpenImages: ([String], Int) -> Void
    @State private var commentText = ""
    @FocusState private var commentFocused: Bool
    /// 显式的滚动位置（最上方可见的动态）。LazyVStack 在键盘收起、视口高度变化时会重新估算未渲染行的高度，
    /// 没有锚点时 ScrollView 的偏移被重置到顶部；绑定 scrollPosition 后按这条动态保持位置。
    @State private var anchorId: String?
    static let topId = "momentsFeedTop"
    /// 刚发表评论、等待滚到可视区域内的动态
    @State private var pendingReveal: String?

    var body: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(spacing: 0) {
                    header
                        .padding(.bottom, 12)
                        .id(Self.topId)   // 停在顶部时以封面为锚点：下拉刷新、新动态插入到最前面时仍显示在封面下方
                    if model.unavailable {
                        EmptyStateView(systemImage: "eye.slash", text: "该动态已不可见")
                            .accessibilityIdentifier("momentUnavailable")
                    } else if model.loaded && model.moments.isEmpty && !model.loading {
                        EmptyStateView(systemImage: "camera.aperture", text: "还没有动态")
                    }
                    ForEach(model.moments, id: \.id) { m in
                        VStack(spacing: 0) {
                            MomentCard(moment: m, model: model, onOpenImages: onOpenImages)
                            Divider().padding(.leading, 70)
                        }
                        .id(m.id)
                        .onAppear {
                            if m.id == model.moments.last?.id { Task { await model.loadMore() } }
                        }
                    }
                    if model.loading && model.loaded {
                        ProgressView().padding()
                    } else if !model.hasMore && !model.moments.isEmpty && !model.isSingle {
                        Text("— 没有更多了 —").font(Fonts.timestamp).foregroundStyle(Theme.textSecondary).padding()
                    }
                }
                .scrollTargetLayout()
            }
            .scrollPosition(id: $anchorId)
            .ignoresSafeArea(edges: extendsUnderTop ? .top : [])
            .background(Theme.background)
            .refreshable { await model.refresh() }
            // 收到新的朋友圈通知（别人点赞 / 评论 / 回复）时合并最新一页，并更新通知对应的那条动态
            .onChange(of: HushCore.shared.moments.notes.first?.id) { _, id in
                guard id != nil, let note = HushCore.shared.moments.notes.first else { return }
                Task { await model.mergeLatest(touched: note.momentId) }
            }
            .safeAreaInset(edge: .bottom) {
                if let t = model.commentTarget {
                    HStack(spacing: 8) {
                        TextField(t.replyTo.map { "回复 \($0.name)" } ?? "评论", text: $commentText)
                            .padding(.horizontal, 12).frame(height: 38)
                            .background(RoundedRectangle(cornerRadius: 19).fill(Theme.surface))
                            .focused($commentFocused)
                            .submitLabel(.send)
                            .onSubmit(send)
                            .accessibilityIdentifier("commentField")
                        Button("发送", action: send)
                            .disabled(commentText.trimmingCharacters(in: .whitespaces).isEmpty)
                            .accessibilityIdentifier("sendComment")
                    }
                    .padding(10)
                    .background(Theme.background)
                    .overlay(alignment: .top) { Divider() }
                    .onAppear { commentFocused = true }
                }
            }
            .onChange(of: commentFocused) { _, f in
                if !f && commentText.isEmpty { model.commentTarget = nil }
            }
            // 键盘弹出后把目标卡片的底部对齐到评论框上方（被键盘挡住的卡片也能看到）
            .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardDidShowNotification)) { _ in
                guard let id = model.commentTarget?.momentId else { return }
                withAnimation(.easeOut(duration: 0.2)) { proxy.scrollTo(id, anchor: .bottom) }
            }
            // 发送后：键盘收起、评论写回时让目标卡片底部（新评论所在处）留在可视区域内。
            // 只在「刚发送」这一次生效：键盘通知是全局的，别处收起键盘不能把列表拉回这条动态。
            .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardDidHideNotification)) { _ in
                if let id = pendingReveal { reveal(id, proxy) }
            }
            .onChange(of: model.commentAdded?.tick) { _, _ in
                guard let id = pendingReveal, model.commentAdded?.momentId == id else { return }
                pendingReveal = nil
                reveal(id, proxy)
            }
            .onChange(of: model.commentTarget) { _, t in if t != nil { pendingReveal = nil } }
        }
    }

    private func reveal(_ id: String, _ proxy: ScrollViewProxy) {
        withAnimation(.easeOut(duration: 0.2)) { proxy.scrollTo(id, anchor: .bottom) }
    }

    private func send() {
        let id = model.commentTarget?.momentId
        pendingReveal = id
        // 评论请求失败时不会写回：稍后自动作废，避免之后的键盘收起把列表拉回来
        DispatchQueue.main.asyncAfter(deadline: .now() + 8) { if pendingReveal == id { pendingReveal = nil } }
        model.sendComment(commentText)
        commentText = ""
        commentFocused = false
    }
}

/// 个人相册：某个用户的动态
struct UserMomentsView: View {
    let userId: String
    @State private var model: MomentsModel
    @State private var viewer: ImageViewerItem?
    var core = HushCore.shared

    init(userId: String) {
        self.userId = userId
        _model = State(initialValue: MomentsModel(userId: userId))
    }

    var body: some View {
        let p = core.contacts.profile(userId)
        MomentsFeed(model: model,
                    header: AnyView(MomentsCover(userId: "", name: p?.name ?? core.contacts.displayName(userId), avatarId: p?.avatarId)
                        .padding(.bottom, 24)),
                    onOpenImages: { viewer = ImageViewerItem(paths: $0, index: $1) })
            .navigationTitle(userId == core.myId ? "我的相册" : "\(p?.name ?? "")的朋友圈")
            .navigationBarTitleDisplayMode(.inline)
            .task { if !model.loaded { await model.refresh() } }
            .fullScreenCover(item: $viewer) { v in ImageViewer(paths: v.paths, index: v.index) }
    }
}

/// 单条动态详情（从朋友圈通知进入）
struct MomentDetailView: View {
    let momentId: String
    @State private var model: MomentsModel
    @State private var viewer: ImageViewerItem?

    init(momentId: String) {
        self.momentId = momentId
        _model = State(initialValue: MomentsModel(source: .single(momentId)))
    }

    var body: some View {
        MomentsFeed(model: model, header: AnyView(EmptyView()), extendsUnderTop: false,
                    onOpenImages: { viewer = ImageViewerItem(paths: $0, index: $1) })
            .overlay { if !model.loaded { ProgressView() } }
            .navigationTitle("详情")
            .navigationBarTitleDisplayMode(.inline)
            .task { if !model.loaded { await model.refresh() } }
            .fullScreenCover(item: $viewer) { v in ImageViewer(paths: v.paths, index: v.index) }
    }
}

/// 互动通知列表：点击进入对应动态详情
struct MomentNotificationsView: View {
    var core = HushCore.shared
    var body: some View {
        List(core.moments.notes, id: \.id) { n in
            NavigationLink(value: Route.momentDetail(n.momentId)) { row(n) }
                .accessibilityIdentifier("momentNote_\(n.from.name)")
        }
        .listStyle(.plain)
        .overlay { if core.moments.notes.isEmpty { EmptyStateView(systemImage: "bell", text: "暂无消息") } }
        .navigationTitle("消息")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await core.moments.refreshNotes() }
        .onDisappear { core.moments.markNotesSeen() }
        .task {
            await core.moments.refreshNotes()
            try? await Task.sleep(for: .seconds(1.5))
            core.moments.markNotesSeen()
        }
    }

    private func row(_ n: MomentNotification) -> some View {
        HStack(alignment: .top, spacing: 12) {
            AvatarView(seed: n.from.id, name: n.from.name, avatarId: n.from.avatarId, size: 44)
            VStack(alignment: .leading, spacing: 4) {
                Text(n.from.name).font(.system(size: 15, weight: .semibold)).foregroundStyle(Theme.accent)
                if n.kind == .like {
                    Image(systemName: "heart.fill").foregroundStyle(Theme.danger)
                } else {
                    Text(n.text ?? "").font(Fonts.body)
                }
                Text(Fmt.momentTime(n.createdAt)).font(Fonts.timestamp).foregroundStyle(Theme.textSecondary)
            }
            Spacer()
            if n.createdAt > core.moments.notesSeenTs {
                Circle().fill(Theme.danger).frame(width: 8, height: 8)
            }
        }
        .padding(.vertical, 4)
    }
}

