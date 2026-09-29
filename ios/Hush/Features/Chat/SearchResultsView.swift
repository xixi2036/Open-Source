import SwiftUI

/// 搜索：会话名（内存过滤）+ 消息内容（核心 search_messages，逐条解密，较慢）。
/// 消息搜索防抖 300ms，在后台线程执行，输入变化时取消上一次。
struct SearchResultsView: View {
    let query: String
    var core = HushCore.shared
    @State private var results: (query: String, messages: [MessageItem])?

    private var q: String { query.trimmingCharacters(in: .whitespaces) }

    var body: some View {
        let q = q
        let convs = (core.chats.conversations + core.chats.archivedConversations).filter {
            core.chats.title($0.id).localizedCaseInsensitiveContains(q)
        }
        let done = results?.query == q
        let msgs = done ? results?.messages ?? [] : []
        List {
            if !convs.isEmpty {
                Section("会话") {
                    ForEach(convs, id: \.id) { c in
                        NavigationLink(value: Route.chat(c.id)) { ConversationRow(summary: c) }
                    }
                }
            }
            if !msgs.isEmpty {
                Section("消息") {
                    ForEach(msgs, id: \.id) { m in
                        NavigationLink(value: Route.chatFocus(m.convId, m.id)) {
                            HStack(spacing: 12) {
                                ConversationAvatar(convId: m.convId, size: 40)
                                VStack(alignment: .leading, spacing: 2) {
                                    HStack {
                                        Text(core.chats.title(m.convId)).font(.system(size: 15, weight: .medium))
                                        Spacer()
                                        Text(Fmt.listTime(m.sentAt)).font(Fonts.timestamp).foregroundStyle(Theme.textSecondary)
                                    }
                                    Text(MessagePreview.text(m)).font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(2)
                                }
                            }
                        }
                    }
                }
            } else if !done {
                HStack(spacing: 8) {
                    ProgressView()
                    Text("正在搜索消息…").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                }
                .frame(maxWidth: .infinity)
                .listRowSeparator(.hidden)
            }
        }
        .listStyle(.plain)
        .overlay {
            if done && convs.isEmpty && msgs.isEmpty { EmptyStateView(systemImage: "magnifyingglass", text: "没有找到相关内容") }
        }
        .task(id: q) {
            do { try await Task.sleep(for: .milliseconds(300)) } catch { return }
            let found = await MessageSearch.run(q)
            guard !Task.isCancelled else { return }
            results = (q, found)
        }
    }
}

enum MessageSearch {
    static let limit = 100

    /// 在后台线程调用核心的消息搜索（同步且会逐条解密）
    @MainActor
    static func run(_ query: String) async -> [MessageItem] {
        await HushCore.shared.chats.searchMessages(query, limit: limit)
    }
}
