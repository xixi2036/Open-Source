import SwiftUI

/// 转发目标：最近会话（含已归档）在前，然后是还没有会话的好友
struct ForwardTarget: Equatable, Identifiable {
    var id: String          // convId
    var title: String
    var isGroup: Bool
}

enum ForwardTargets {
    /// conversations 已按列表顺序排列；已退出的群不能转发；好友去掉已有单聊会话的
    static func build(conversations: [ConversationSummary], friends: [Profile],
                      title: (String) -> String, directId: (String) -> String) -> [ForwardTarget] {
        var seen = Set<String>()
        var out: [ForwardTarget] = []
        for c in conversations where c.active && !seen.contains(c.id) {
            seen.insert(c.id)
            out.append(ForwardTarget(id: c.id, title: title(c.id), isGroup: c.kind == .group))
        }
        for f in friends {
            let cid = directId(f.id)
            guard !seen.contains(cid) else { continue }
            seen.insert(cid)
            out.append(ForwardTarget(id: cid, title: f.name.isEmpty ? title(cid) : f.name, isGroup: false))
        }
        return out
    }

    static func filter(_ list: [ForwardTarget], query: String) -> [ForwardTarget] {
        let q = query.trimmingCharacters(in: .whitespaces)
        guard !q.isEmpty else { return list }
        return list.filter { $0.title.localizedCaseInsensitiveContains(q) }
    }
}

/// 转发：多选会话 / 好友（可搜索），确认后调用核心 forward_message。
/// 部分目标发送失败时提示，可一键重发失败的那几条（也可以之后在对应会话里点气泡旁的重发）。
struct ForwardSheet: View {
    let message: MessageItem
    var core = HushCore.shared
    @Environment(\.dismiss) private var dismiss
    @State private var query = ""
    @State private var selected: [String] = []
    @State private var sending = false
    @State private var failed: [MessageItem] = []
    @State private var showFailed = false

    private var targets: [ForwardTarget] {
        ForwardTargets.build(conversations: core.chats.conversations + core.chats.archivedConversations,
                             friends: core.contacts.friends,
                             title: { core.chats.title($0) },
                             directId: { core.chats.directConversationId($0) })
    }

    var body: some View {
        NavigationStack {
            let all = ForwardTargets.filter(targets, query: query)
            let recent = all.filter { isConversation($0.id) }
            let friends = all.filter { !isConversation($0.id) }
            List {
                Section {
                    preview
                }
                if !recent.isEmpty {
                    Section("最近会话") { ForEach(recent) { row($0) } }
                }
                if !friends.isEmpty {
                    Section("好友") { ForEach(friends) { row($0) } }
                }
            }
            .listStyle(.insetGrouped)
            .overlay {
                if recent.isEmpty && friends.isEmpty { EmptyStateView(systemImage: "magnifyingglass", text: "没有匹配的会话或好友") }
            }
            .searchable(text: $query, placement: .navigationBarDrawer(displayMode: .always), prompt: "搜索会话或好友")
            .navigationTitle("转发给")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } }
            }
            // 发送按钮放在底部：搜索时导航栏会被搜索框替换，工具栏按钮不可见
            .safeAreaInset(edge: .bottom, spacing: 0) {
                Button(sending ? "发送中…" : selected.isEmpty ? "选择要转发的会话" : "发送给 \(selected.count) 个会话") { send() }
                    .buttonStyle(PrimaryButtonStyle(enabled: !selected.isEmpty && !sending))
                    .disabled(selected.isEmpty || sending)
                    .padding(.horizontal, 16).padding(.vertical, 10)
                    .background(.bar)
                    .accessibilityIdentifier("forwardSend")
            }
            .alert("有 \(failed.count) 个会话转发失败", isPresented: $showFailed) {
                Button("重发") { resendFailed() }.accessibilityIdentifier("forwardRetry")
                Button("稍后", role: .cancel) { dismiss() }
            } message: {
                Text("失败的消息保留在对应会话中，也可以稍后点气泡旁的红色感叹号重发")
            }
        }
    }

    private func isConversation(_ id: String) -> Bool {
        core.chats.conversations.contains { $0.id == id } || core.chats.archivedConversations.contains { $0.id == id }
    }

    private var preview: some View {
        HStack(spacing: 8) {
            Image(systemName: "arrowshape.turn.up.right").foregroundStyle(Theme.accent)
            Text(MessagePreview.text(message)).font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(2)
        }
    }

    private func row(_ t: ForwardTarget) -> some View {
        Button {
            if let i = selected.firstIndex(of: t.id) { selected.remove(at: i) } else { selected.append(t.id) }
        } label: {
            HStack(spacing: 12) {
                Image(systemName: selected.contains(t.id) ? "checkmark.circle.fill" : "circle")
                    .font(.system(size: 22))
                    .foregroundStyle(selected.contains(t.id) ? Theme.accent : Theme.textSecondary)
                ConversationAvatar(convId: t.id, size: 40)
                Text(t.title).foregroundStyle(Theme.textPrimary).lineLimit(1)
                Spacer()
            }
            .contentShape(Rectangle())
        }
        .accessibilityIdentifier("forward_\(t.title)")
    }

    private func send() {
        sending = true
        let ids = selected, mid = message.id
        Task {
            defer { sending = false }
            do {
                let result = try await core.chats.forward(mid, to: ids)
                let bad = result.filter { $0.status == .failed }
                if bad.isEmpty {
                    core.toast = "已转发"
                    dismiss()
                } else {
                    failed = bad
                    showFailed = true
                }
            } catch {
                core.toast = describe(error)
            }
        }
    }

    private func resendFailed() {
        let ids = failed.map(\.id)
        let chats = core.chats
        dismiss()
        core.run {
            var stillFailed = 0
            for id in ids {
                do {
                    if try await chats.resend(id).status == .failed { stillFailed += 1 }
                } catch { stillFailed += 1 }
            }
            HushCore.shared.toast = stillFailed == 0 ? "已重发" : "仍有 \(stillFailed) 个会话发送失败，可在对应会话中重发"
        }
    }
}
