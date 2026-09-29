import SwiftUI
import PhotosUI

/// 群聊设置：群头像/群名/公告、成员网格（添加/移除/设管理员）、阅后即焚、免打扰、置顶、清空、退群
struct GroupSettingsView: View {
    let groupId: String
    var core = HushCore.shared
    @State private var editName = false
    @State private var editNotice = false
    @State private var nameText = ""
    @State private var noticeText = ""
    @State private var showAdd = false
    @State private var removeMode = false
    @State private var memberAction: GroupMember?
    @State private var confirmLeave = false
    @State private var confirmClear = false
    @State private var avatarPick: PhotosPickerItem?

    private var convId: String { "g:\(groupId)" }
    private var group: GroupInfo? { core.contacts.group(groupId) }
    private var myRole: GroupRole? { group?.members.first { $0.userId == core.myId }?.role }
    private var isAdmin: Bool { myRole == .owner || myRole == .admin }
    private var isOwner: Bool { myRole == .owner }

    var body: some View {
        let g = group
        List {
            Section {
                HStack(spacing: 14) {
                    ZStack(alignment: .bottomTrailing) {
                        ConversationAvatar(convId: convId, size: 64)
                        if isAdmin {
                            PhotosPicker(selection: $avatarPick, matching: .images) {
                                Image(systemName: "camera.fill").font(.system(size: 11)).foregroundStyle(.white)
                                    .frame(width: 22, height: 22).background(Circle().fill(Theme.accent))
                            }
                            // 同一列表行里有两个按钮：必须用 borderless，否则点任意位置会同时触发两者，改群名的弹框出不来
                            .buttonStyle(.borderless)
                        }
                    }
                    VStack(alignment: .leading, spacing: 4) {
                        Text(g?.name ?? "群聊").font(Fonts.title).accessibilityIdentifier("groupName")
                        Text("\(g?.members.count ?? 0) 位成员").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                    }
                    Spacer()
                    if isAdmin {
                        Button("编辑") { nameText = g?.name ?? ""; editName = true }
                            .buttonStyle(.borderless)
                            .accessibilityIdentifier("editGroupName")
                    }
                }
            }
            Section("群成员") {
                LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 8, alignment: .top), count: 5), spacing: 14) {
                    ForEach(g?.members ?? [], id: \.userId) { m in memberCell(m) }
                    if g?.active == true {
                        gridButton("plus") { showAdd = true }.accessibilityIdentifier("addMember")
                        if isAdmin && (g?.members.count ?? 0) > 1 {
                            gridButton(removeMode ? "checkmark" : "minus") { removeMode.toggle() }
                                .accessibilityIdentifier("removeMode")
                        }
                    }
                }
                .padding(.vertical, 8)
            }
            Section("群公告") {
                Button {
                    guard isAdmin else { return }
                    noticeText = g?.announcement ?? ""
                    editNotice = true
                } label: {
                    Text((g?.announcement.isEmpty ?? true) ? (isAdmin ? "点击设置群公告" : "暂无公告") : g!.announcement)
                        .foregroundStyle((g?.announcement.isEmpty ?? true) ? Theme.textSecondary : Theme.textPrimary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .accessibilityIdentifier("groupNotice")
            }
            Section { ConversationToggles(convId: convId) }
            Section {
                Button("清空聊天记录") { confirmClear = true }.foregroundStyle(Theme.textPrimary)
                if g?.active == true {
                    Button("退出群聊", role: .destructive) { confirmLeave = true }
                        .foregroundStyle(Theme.danger)
                        .accessibilityIdentifier("leaveGroup")
                }
            }
        }
        .navigationTitle("群聊信息")
        .navigationBarTitleDisplayMode(.inline)
        .task { await core.contacts.refreshGroups() }
        .alert("修改群名", isPresented: $editName) {
            TextField("群名称", text: $nameText)
            Button("取消", role: .cancel) {}
            Button("保存") { update(name: nameText) }
        }
        .alert("群公告", isPresented: $editNotice) {
            TextField("公告内容", text: $noticeText)
            Button("取消", role: .cancel) {}
            Button("发布") { update(announcement: noticeText) }
        }
        .sheet(isPresented: $showAdd) {
            AddMembersSheet(exclude: Set(g?.members.map(\.userId) ?? [])) { ids in
                core.run {
                    try await core.contacts.addMembers(groupId, ids)
                }
            }
        }
        .confirmationDialog(memberAction.map { core.contacts.displayName($0.userId) } ?? "", isPresented: Binding(
            get: { memberAction != nil }, set: { if !$0 { memberAction = nil } }
        ), titleVisibility: .visible, presenting: memberAction) { m in
            Button("查看资料") { Router.shared.push(.profile(m.userId)) }
            if isOwner && m.userId != core.myId && m.role != .owner {
                Button(m.role == .admin ? "取消管理员" : "设为管理员") {
                    core.run {
                        try await core.contacts.setAdmin(groupId, m.userId, admin: m.role != .admin)
                    }
                }
            }
            if isAdmin && m.userId != core.myId && m.role != .owner {
                Button("移出群聊", role: .destructive) { remove(m.userId) }
            }
        }
        .confirmationDialog("退出后将不再接收此群聊消息", isPresented: $confirmLeave, titleVisibility: .visible) {
            Button("退出群聊", role: .destructive) {
                core.run {
                    try await core.contacts.leaveGroup(groupId)
                    core.chats.reload()
                    Router.shared.chatPath = .init()
                }
            }
        }
        .confirmationDialog("确定清空群聊记录？", isPresented: $confirmClear, titleVisibility: .visible) {
            Button("清空", role: .destructive) {
                if core.chats.update(convId, .clear) { core.toast = "已清空" }
            }
        }
        .onChange(of: avatarPick) { _, item in
            guard let item else { return }
            Task {
                if let r = await PhotoLoader.load(item, maxSide: 640) {
                    core.run {
                        try await core.contacts.updateGroup(groupId, avatarPath: r.url.path)
                    }
                }
            }
        }
    }

    private func memberCell(_ m: GroupMember) -> some View {
        VStack(spacing: 4) {
            ZStack(alignment: .topTrailing) {
                UserAvatar(userId: m.userId, size: 50)
                if removeMode && m.userId != core.myId && m.role != .owner {
                    Image(systemName: "minus.circle.fill").foregroundStyle(Theme.danger).background(Circle().fill(.white))
                        .offset(x: 4, y: -4)
                }
            }
            Text(core.contacts.displayName(m.userId)).font(.system(size: 11)).lineLimit(1)
            if m.role != .member {
                Text(m.role == .owner ? "群主" : "管理员").font(.system(size: 9)).foregroundStyle(Theme.accent)
            }
        }
        .contentShape(Rectangle())
        .onTapGesture {
            if removeMode && m.userId != core.myId && m.role != .owner { remove(m.userId) } else { memberAction = m }
        }
        .accessibilityElement(children: .combine)
        .accessibilityAddTraits(.isButton)
        .accessibilityIdentifier("member_\(core.contacts.displayName(m.userId))")
    }

    private func gridButton(_ icon: String, _ run: @escaping () -> Void) -> some View {
        Button(action: run) {
            VStack(spacing: 4) {
                RoundedRectangle(cornerRadius: 10).stroke(Theme.divider, style: StrokeStyle(lineWidth: 1.5, dash: [4]))
                    .frame(width: 50, height: 50)
                    .overlay(Image(systemName: icon).foregroundStyle(Theme.textSecondary))
                Text(" ").font(.system(size: 11))
            }
        }
        .buttonStyle(.plain)
    }

    private func remove(_ uid: String) {
        core.run {
            try await core.contacts.removeMember(groupId, uid)
        }
    }

    private func update(name: String? = nil, announcement: String? = nil) {
        core.run {
            try await core.contacts.updateGroup(groupId, name: name, announcement: announcement)
        }
    }
}

struct AddMembersSheet: View {
    let exclude: Set<String>
    var onDone: ([String]) -> Void
    @State private var selected: Set<String> = []
    @Environment(\.dismiss) private var dismiss
    var core = HushCore.shared

    var body: some View {
        NavigationStack {
            List { MultiFriendPicker(candidates: core.contacts.friends.filter { !exclude.contains($0.id) }, selected: $selected) }
                .overlay {
                    if core.contacts.friends.filter({ !exclude.contains($0.id) }).isEmpty {
                        EmptyStateView(systemImage: "person.2", text: "没有可添加的好友")
                    }
                }
                .navigationTitle("添加成员")
                .navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("添加(\(selected.count))") { dismiss(); onDone(Array(selected)) }.disabled(selected.isEmpty)
                    }
                }
        }
    }
}
