import SwiftUI

/// 一条动态：头像、昵称、正文、九宫格、时间、删除、赞/评论、互动区
struct MomentCard: View {
    let moment: Moment
    let model: MomentsModel
    var onOpenImages: ([String], Int) -> Void
    var core = HushCore.shared
    @State private var showActions = false
    @State private var confirmDelete = false
    @State private var commentToDelete: Comment?

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            AvatarView(seed: moment.author.id, name: moment.author.name, avatarId: moment.author.avatarId, size: 44)
                .onTapGesture { Router.shared.push(.profile(moment.author.id)) }
            VStack(alignment: .leading, spacing: 8) {
                Text(moment.author.name)
                    .font(.system(size: 16, weight: .semibold))
                    .foregroundStyle(Theme.accent)
                    .onTapGesture { Router.shared.push(.profile(moment.author.id)) }
                if !moment.text.isEmpty {
                    Text(moment.text).font(Fonts.body).foregroundStyle(Theme.textPrimary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                if !moment.media.isEmpty {
                    MomentImageGrid(media: moment.media, onOpen: onOpenImages)
                }
                HStack(spacing: 12) {
                    Text(Fmt.momentTime(moment.createdAt)).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                    if moment.author.id == core.myId {
                        if let v = moment.visibility, v.mode != .all {
                            Image(systemName: "person.2.slash").font(.system(size: 11)).foregroundStyle(Theme.textSecondary)
                        }
                        Button("删除") { confirmDelete = true }
                            .font(Fonts.secondary).foregroundStyle(Theme.accent)
                            .accessibilityIdentifier("deleteMoment")
                    }
                    Spacer()
                    if showActions {
                        HStack(spacing: 0) {
                            Button {
                                withAnimation { showActions = false }
                                model.toggleLike(moment)
                            } label: {
                                Label(model.liked(moment) ? "取消" : "赞", systemImage: model.liked(moment) ? "heart.slash" : "heart")
                                    .frame(width: 80, height: 34)
                            }
                            .accessibilityIdentifier("likeButton")
                            Divider().frame(height: 18).background(Color.white.opacity(0.3))
                            Button {
                                withAnimation { showActions = false }
                                model.commentTarget = .init(momentId: moment.id, replyTo: nil)
                            } label: {
                                Label("评论", systemImage: "text.bubble").frame(width: 80, height: 34)
                            }
                            .accessibilityIdentifier("commentButton")
                        }
                        .font(.system(size: 14))
                        .foregroundStyle(.white)
                        .background(RoundedRectangle(cornerRadius: 6).fill(Color(white: 0.25)))
                        .transition(.move(edge: .trailing).combined(with: .opacity))
                    }
                    Button { withAnimation(.easeOut(duration: 0.15)) { showActions.toggle() } } label: {
                        Image(systemName: "ellipsis")
                            .font(.system(size: 14, weight: .bold))
                            .foregroundStyle(Theme.accent)
                            .frame(width: 32, height: 22)
                            .background(RoundedRectangle(cornerRadius: 4).fill(Theme.surface))
                    }
                    .accessibilityIdentifier("momentActions_\(moment.author.name)")
                }
                if !moment.likes.isEmpty || !moment.comments.isEmpty {
                    interactions
                }
            }
        }
        .padding(.horizontal, 16).padding(.vertical, 14)
        .confirmationDialog("删除这条动态？", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("删除", role: .destructive) { model.deleteMoment(moment) }
        }
        .confirmationDialog("删除这条评论？", isPresented: Binding(get: { commentToDelete != nil }, set: { if !$0 { commentToDelete = nil } }),
                            titleVisibility: .visible) {
            Button("删除", role: .destructive) { if let c = commentToDelete { model.deleteComment(moment.id, c) } }
        }
    }

    private var interactions: some View {
        VStack(alignment: .leading, spacing: 4) {
            if !moment.likes.isEmpty {
                HStack(alignment: .top, spacing: 4) {
                    Image(systemName: "heart").font(.system(size: 12)).foregroundStyle(Theme.accent).padding(.top, 3)
                    Text(moment.likes.map(\.name).joined(separator: "，"))
                        .font(.system(size: 14, weight: .medium)).foregroundStyle(Theme.accent)
                }
                .accessibilityIdentifier("likesLine")
            }
            if !moment.likes.isEmpty && !moment.comments.isEmpty { Divider() }
            ForEach(moment.comments, id: \.id) { c in
                commentLine(c)
                    .contentShape(Rectangle())
                    .onTapGesture {
                        if c.author.id != core.myId { model.commentTarget = .init(momentId: moment.id, replyTo: c.author) }
                    }
                    .onLongPressGesture {
                        if c.author.id == core.myId { commentToDelete = c }
                    }
            }
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 6).fill(Theme.surface))
    }

    /// 「A 回复 B：内容」：用 Text 拼接着色（不用 AttributedString 的动态键路径，避免并发检查告警）
    private func commentLine(_ c: Comment) -> some View {
        let nameFont = Font.system(size: 14, weight: .medium)
        var t = Text(c.author.name).font(nameFont).foregroundStyle(Theme.accent)
        if let r = c.replyTo {
            t = t + Text(" 回复 ").foregroundStyle(Theme.textPrimary) + Text(r.name).font(nameFont).foregroundStyle(Theme.accent)
        }
        t = t + Text("：\(c.text)").foregroundStyle(Theme.textPrimary)
        return t.font(.system(size: 14)).fixedSize(horizontal: false, vertical: true)
            .accessibilityIdentifier("comment")
    }
}

/// 九宫格：1 张大图保持比例，2–9 张 3 列方格
struct MomentImageGrid: View {
    let media: [MomentMedia]
    var onOpen: ([String], Int) -> Void
    var core = HushCore.shared

    var body: some View {
        if media.count == 1, let m = media.first {
            let w = CGFloat(max(m.width, 1)), h = CGFloat(max(m.height, 1))
            let scale = min(200 / w, 200 / h)
            MomentImage(attachmentId: m.attachmentId)
                .frame(width: max(w * scale, 90), height: max(h * scale, 90))
                .clipped()
                .onTapGesture { open(0) }
        } else {
            let cols = media.count == 4 ? 2 : 3
            LazyVGrid(columns: Array(repeating: GridItem(.fixed(84), spacing: 4), count: cols), alignment: .leading, spacing: 4) {
                ForEach(Array(media.enumerated()), id: \.offset) { i, m in
                    MomentImage(attachmentId: m.attachmentId)
                        .frame(width: 84, height: 84)
                        .clipped()
                        .onTapGesture { open(i) }
                }
            }
        }
    }

    private func open(_ i: Int) {
        let paths = media.compactMap { core.media.mediaPaths[$0.attachmentId] }
        guard paths.count == media.count else { return }
        onOpen(paths, i)
    }
}

struct MomentImage: View {
    let attachmentId: String
    var core = HushCore.shared
    var body: some View {
        ZStack {
            if let p = core.media.mediaPaths[attachmentId] {
                LocalImage(path: p, maxPixel: 500) { loading }
            } else {
                loading
            }
        }
        .contentShape(Rectangle())
        .task(id: attachmentId) { await core.loadMedia(attachmentId) }
        .accessibilityElement()
        .accessibilityIdentifier("momentImage")
        .accessibilityAddTraits(.isButton)
    }

    private var loading: some View {
        ZStack {
            Theme.surface
            ProgressView()
        }
    }
}
