import SwiftUI

/// 通讯录 Tab
struct ContactsView: View {
    var core = HushCore.shared

    var body: some View {
        List {
            Section {
                NavigationLink(value: Route.friendRequests) {
                    HStack(spacing: 12) {
                        iconTile("person.badge.plus", .orange)
                        Text("新的朋友")
                        Spacer()
                        BadgeView(count: core.contacts.pendingRequestCount)
                    }
                }
                .accessibilityIdentifier("friendRequestsEntry")
                NavigationLink(value: Route.groupList) {
                    HStack(spacing: 12) {
                        iconTile("person.3.fill", .green)
                        Text("群聊")
                        Spacer()
                        Text("\(core.contacts.groups.filter(\.active).count)").foregroundStyle(Theme.textSecondary)
                    }
                }
                .accessibilityIdentifier("groupsEntry")
            }
            Section("好友（\(core.contacts.friends.count)）") {
                if core.contacts.friends.isEmpty {
                    Text("还没有好友，点右上角添加").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                }
                ForEach(core.contacts.friends, id: \.id) { p in
                    NavigationLink(value: Route.profile(p.id)) {
                        HStack(spacing: 12) {
                            UserAvatar(userId: p.id, size: 40)
                            Text(p.name).foregroundStyle(Theme.textPrimary)
                        }
                    }
                    .accessibilityIdentifier("friend_\(p.name)")
                }
            }
        }
        .listStyle(.insetGrouped)
        .navigationTitle("通讯录")
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                NavigationLink(value: Route.addFriend) { Image(systemName: "person.crop.circle.badge.plus") }
                    .accessibilityLabel("添加好友")
                    .accessibilityIdentifier("addFriendButton")
            }
        }
        .refreshable {
            await core.contacts.refreshFriends()
            await core.contacts.refreshFriendRequests()
            await core.contacts.refreshGroups()
        }
    }

    private func iconTile(_ name: String, _ color: Color) -> some View {
        Image(systemName: name)
            .font(.system(size: 16)).foregroundStyle(.white)
            .frame(width: 40, height: 40)
            .background(RoundedRectangle(cornerRadius: 10).fill(color))
    }
}

/// 新的朋友：申请列表
struct FriendRequestsView: View {
    var core = HushCore.shared
    @State private var busy: Set<String> = []

    var body: some View {
        let incoming = core.contacts.friendRequests.incoming
        let outgoing = core.contacts.friendRequests.outgoing
        List {
            if !incoming.isEmpty {
                Section("收到的申请") {
                    ForEach(incoming, id: \.id) { r in row(r, profile: r.from, incoming: true) }
                }
            }
            if !outgoing.isEmpty {
                Section("我发出的申请") {
                    ForEach(outgoing, id: \.id) { r in row(r, profile: r.to, incoming: false) }
                }
            }
        }
        .overlay {
            if incoming.isEmpty && outgoing.isEmpty { EmptyStateView(systemImage: "person.crop.circle.badge.questionmark", text: "暂无好友申请") }
        }
        .navigationTitle("新的朋友")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await core.contacts.refreshFriendRequests() }
        .task { await core.contacts.refreshFriendRequests() }
    }

    private func row(_ r: FriendRequest, profile: Profile, incoming: Bool) -> some View {
        HStack(spacing: 12) {
            AvatarView(seed: profile.id, name: profile.name, avatarId: profile.avatarId, size: 44)
            VStack(alignment: .leading, spacing: 2) {
                Text(profile.name).font(Fonts.sized(16, weight: .medium))
                if !r.message.isEmpty {
                    Text(r.message).font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(2)
                }
            }
            Spacer()
            if incoming && r.status == "pending" {
                if busy.contains(r.id) {
                    ProgressView()
                } else {
                    Button("拒绝") { act(r, accept: false) }
                        .buttonStyle(.bordered).tint(Theme.danger)
                        .accessibilityIdentifier("reject_\(profile.name)")
                    Button("接受") { act(r, accept: true) }
                        .buttonStyle(.borderedProminent).tint(Theme.accent)
                        .accessibilityIdentifier("accept_\(profile.name)")
                }
            } else {
                Text(statusText(r.status, incoming: incoming)).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
            }
        }
        .contentShape(Rectangle())
        .onTapGesture { Router.shared.push(.profile(profile.id)) }
    }

    private func statusText(_ s: String, incoming: Bool) -> String {
        switch s {
        case "accepted": return "已添加"
        case "rejected": return "已拒绝"
        case "pending": return incoming ? "待处理" : "等待验证"
        default: return s
        }
    }

    private func act(_ r: FriendRequest, accept: Bool) {
        busy.insert(r.id)
        Task {
            do {
                try await core.contacts.respond(to: r, accept: accept)
            } catch { core.toast = describe(error) }
            busy.remove(r.id)
        }
    }
}

/// 群聊列表
struct GroupListView: View {
    var core = HushCore.shared
    var body: some View {
        let groups = core.contacts.groups.filter(\.active)
        List(groups, id: \.id) { g in
            NavigationLink(value: Route.chat("g:\(g.id)")) {
                HStack(spacing: 12) {
                    ConversationAvatar(convId: "g:\(g.id)", size: 40)
                    Text("\(g.name) (\(g.members.count))")
                }
            }
            .accessibilityIdentifier("group_\(g.name)")
        }
        .overlay { if groups.isEmpty { EmptyStateView(systemImage: "person.3", text: "还没有群聊") } }
        .navigationTitle("群聊")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await core.contacts.refreshGroups() }
    }
}
