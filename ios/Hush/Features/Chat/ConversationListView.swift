import SwiftUI

/// 聊天 Tab：会话列表
struct ConversationListView: View {
    var core = HushCore.shared
    @State private var query = ""
    @State private var searching = false
    @State private var showNewDirect = false
    @State private var showNewGroup = false
    @State private var deleteTarget: ConversationSummary?

    var body: some View {
        Group {
            if !query.trimmingCharacters(in: .whitespaces).isEmpty {
                SearchResultsView(query: query)
            } else if core.chats.conversations.isEmpty && core.chats.archivedConversations.isEmpty {
                VStack {
                    ConnectionBanner()
                    Spacer()
                    EmptyStateView(systemImage: "bubble.left.and.bubble.right", text: "还没有会话，去通讯录找朋友聊天吧")
                    Spacer()
                }
            } else {
                list
            }
        }
        .background(Theme.background)
        .navigationTitle("聊天")
        .searchable(text: $query, isPresented: $searching, placement: .navigationBarDrawer(displayMode: .automatic), prompt: "搜索会话与消息")
        .toolbar {
            ToolbarItemGroup(placement: .topBarTrailing) {
                Button { searching = true } label: { Image(systemName: "magnifyingglass") }
                    .accessibilityIdentifier("searchButton")
                Menu {
                    Button { showNewDirect = true } label: { Label("发起单聊", systemImage: "person") }
                        .accessibilityIdentifier("newDirectChat")
                    Button { showNewGroup = true } label: { Label("新建群聊", systemImage: "person.3") }
                        .accessibilityIdentifier("newGroupChat")
                } label: {
                    Image(systemName: "square.and.pencil")
                }
                .accessibilityIdentifier("newChatMenu")
            }
        }
        .sheet(isPresented: $showNewDirect) {
            FriendPickerSheet(title: "发起单聊") { p in
                Router.shared.openChat(core.chats.directConversationId(p.id))
            }
        }
        .sheet(isPresented: $showNewGroup) { NewGroupSheet() }
        .deleteConversationDialog($deleteTarget)
    }

    private var list: some View {
        List {
            ConnectionBanner()
                .listRowInsets(EdgeInsets())
                .listRowSeparator(.hidden)
            ForEach(core.chats.conversations, id: \.id) { c in
                ConversationRowLink(summary: c, onDelete: { deleteTarget = c })
            }
            if !core.chats.archivedConversations.isEmpty {
                NavigationLink(value: Route.archived) {
                    HStack {
                        Image(systemName: "archivebox").foregroundStyle(Theme.textSecondary)
                        Text("已归档的会话 (\(core.chats.archivedConversations.count))").foregroundStyle(Theme.textSecondary)
                    }
                    .font(Fonts.secondary)
                }
                .accessibilityIdentifier("archivedEntry")
            }
        }
        .listStyle(.plain)
        .refreshable { await core.fullSync() }
    }
}

/// 一行会话 + 滑动操作 + 长按菜单
/// 删除会话的二次确认。放在列表层而不是行内：行内的对话框在左滑按钮收起、行被复用时可能弹不出来
extension View {
    func deleteConversationDialog(_ target: Binding<ConversationSummary?>) -> some View {
        confirmationDialog("删除后本机聊天记录将被清除", isPresented: Binding(
            get: { target.wrappedValue != nil }, set: { if !$0 { target.wrappedValue = nil } }
        ), titleVisibility: .visible, presenting: target.wrappedValue) { c in
            Button("删除", role: .destructive) { HushCore.shared.chats.update(c.id, .delete) }
                .accessibilityIdentifier("confirmDeleteConversation")
        }
    }
}

struct ConversationRowLink: View {
    let summary: ConversationSummary
    var onDelete: () -> Void
    var core = HushCore.shared

    var body: some View {
        NavigationLink(value: Route.chat(summary.id)) {
            ConversationRow(summary: summary)
        }
        .listRowBackground(summary.pinned ? Theme.surface : Theme.background)
        .listRowInsets(EdgeInsets(top: 8, leading: 16, bottom: 8, trailing: 16))
        .swipeActions(edge: .trailing, allowsFullSwipe: false) {
            Button(role: .destructive) { onDelete() } label: { Label("删除", systemImage: "trash") }
                .tint(Theme.danger)   // 全局 tint 是 accent，不显式指定会变成蓝色
            Button { archive() } label: { Label(summary.archived ? "取消归档" : "归档", systemImage: "archivebox") }
                .tint(.gray)
        }
        .swipeActions(edge: .leading) {
            Button { pin() } label: { Label(summary.pinned ? "取消置顶" : "置顶", systemImage: summary.pinned ? "pin.slash" : "pin") }
                .tint(Theme.accent)
            Button { mute() } label: { Label(summary.muted ? "取消免打扰" : "免打扰", systemImage: summary.muted ? "bell" : "bell.slash") }
                .tint(.indigo)
        }
        .contextMenu {
            Button { pin() } label: { Label(summary.pinned ? "取消置顶" : "置顶", systemImage: "pin") }
            Button { mute() } label: { Label(summary.muted ? "取消免打扰" : "免打扰", systemImage: "bell.slash") }
            Button { archive() } label: { Label(summary.archived ? "取消归档" : "归档", systemImage: "archivebox") }
            Button(role: .destructive) { onDelete() } label: { Label("删除", systemImage: "trash") }
        }
    }

    private func pin() { core.chats.update(summary.id, .pin(!summary.pinned)) }
    private func mute() { core.chats.update(summary.id, .mute(!summary.muted)) }
    private func archive() { core.chats.update(summary.id, .archive(!summary.archived)) }
}

struct ConversationRow: View {
    let summary: ConversationSummary
    var core = HushCore.shared

    var body: some View {
        HStack(spacing: 12) {
            ConversationAvatar(convId: summary.id, size: 56)
            VStack(alignment: .leading, spacing: 4) {
                HStack {
                    Text(title).font(.system(size: 17, weight: .medium)).foregroundStyle(Theme.textPrimary).lineLimit(1)
                    Spacer()
                    Text(Fmt.listTime(summary.lastMessage?.sentAt ?? summary.lastActivityTs))
                        .font(Fonts.timestamp).foregroundStyle(Theme.textSecondary)
                }
                HStack(spacing: 6) {
                    previewText
                        .font(Fonts.secondary)
                        .lineLimit(1)
                        .accessibilityIdentifier("convPreview")
                    Spacer()
                    if summary.muted {
                        Image(systemName: "bell.slash.fill").font(.system(size: 11)).foregroundStyle(Theme.textSecondary)
                    }
                    if summary.unreadCount > 0 {
                        BadgeView(count: Int(summary.unreadCount), muted: summary.muted)
                            .accessibilityIdentifier("unreadBadge")
                    }
                }
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("conv_\(title)")
    }

    private var title: String {
        if summary.kind == .group, let gid = summary.groupId, let g = core.contacts.group(gid) { return g.name }
        if let peer = summary.peerUserId, let p = core.contacts.profile(peer), !p.name.isEmpty { return p.name }
        return summary.title
    }

    private var typing: Bool { !core.chats.typingUsers(summary.id).isEmpty }

    private var lastMessageText: String {
        guard let m = summary.lastMessage else { return summary.disappearingSecs > 0 ? "⏱ 阅后即焚已开启" : "" }
        return MessagePreview.text(m, inGroup: summary.kind == .group)
    }

    private var previewText: Text {
        let line = MessagePreview.listLine(typing: typing, unreadMentions: summary.unreadMentions,
                                           draft: summary.draft, message: lastMessageText)
        let body = Text(line.body).foregroundStyle(typing ? Theme.accent : Theme.textSecondary)
        guard let prefix = line.prefix else { return body }
        return Text(prefix + " ").foregroundStyle(Theme.danger) + body
    }
}

/// 已归档的会话
struct ArchivedListView: View {
    var core = HushCore.shared
    @State private var deleteTarget: ConversationSummary?
    var body: some View {
        List {
            ForEach(core.chats.archivedConversations, id: \.id) { c in ConversationRowLink(summary: c, onDelete: { deleteTarget = c }) }
        }
        .listStyle(.plain)
        .overlay {
            if core.chats.archivedConversations.isEmpty { EmptyStateView(systemImage: "archivebox", text: "没有已归档的会话") }
        }
        .deleteConversationDialog($deleteTarget)
        .navigationTitle("已归档的会话")
        .navigationBarTitleDisplayMode(.inline)
    }
}
