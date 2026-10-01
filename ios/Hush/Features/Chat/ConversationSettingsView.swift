import SwiftUI

/// 会话设置入口：单聊 / 群聊两套
struct ConversationSettingsView: View {
    let convId: String
    var body: some View {
        if convId.hasPrefix("g:") {
            GroupSettingsView(groupId: String(convId.dropFirst(2)))
        } else {
            DirectSettingsView(userId: String(convId.dropFirst(2)))
        }
    }
}

/// 阅后即焚、免打扰、置顶三项（单聊群聊共用）
struct ConversationToggles: View {
    let convId: String
    var core = HushCore.shared

    var body: some View {
        let s = core.chats.summary(convId)
        Picker(selection: Binding(
            get: { s?.disappearingSecs ?? 0 },
            set: { secs in core.run { try await core.chats.setDisappearing(convId, secs: secs) } }
        )) {
            ForEach(Fmt.disappearingOptions, id: \.0) { opt in Text(opt.1).tag(opt.0) }
        } label: {
            SettingRow(icon: "timer", tint: .orange, title: "阅后即焚")
        }
        .pickerStyle(.navigationLink)
        .accessibilityIdentifier("disappearingPicker")
        Toggle(isOn: Binding(
            get: { s?.muted ?? false },
            set: { v in core.chats.update(convId, .mute(v)) }
        )) { SettingRow(icon: "bell.slash.fill", tint: .indigo, title: "消息免打扰") }
            .accessibilityIdentifier("muteToggle")
        Toggle(isOn: Binding(
            get: { s?.pinned ?? false },
            set: { v in core.chats.update(convId, .pin(v)) }
        )) { SettingRow(icon: "pin.fill", tint: Theme.accent, title: "置顶聊天") }
            .accessibilityIdentifier("pinToggle")
    }
}

struct DirectSettingsView: View {
    let userId: String
    var core = HushCore.shared
    @State private var confirmClear = false
    @State private var confirmDelete = false
    @State private var confirmBlock = false
    @Environment(\.dismiss) private var dismiss

    private var convId: String { "u:\(userId)" }

    var body: some View {
        let p = core.contacts.profile(userId)
        let muted = core.chats.summary(convId)?.muted ?? false
        List {
            Section {
                VStack(spacing: 10) {
                    AvatarView(seed: userId, name: p?.name ?? "", avatarId: p?.avatarId, size: 84)
                    HStack(spacing: 4) {
                        Text(p?.name ?? "用户").font(Fonts.title)
                        if core.contacts.verifiedUsers.contains(userId) {
                            Image(systemName: "checkmark.shield.fill").foregroundStyle(Theme.success)
                        }
                    }
                    if let email = p?.email, !email.isEmpty {
                        Text(email).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                            .textSelection(.enabled)
                            .accessibilityIdentifier("peerEmail")
                    }
                    if let about = p?.about, !about.isEmpty {
                        Text(about).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                    }
                    HStack(spacing: 12) {
                        actionButton("语音", "phone.fill") { CallManager.shared.startCall(to: userId, video: false) }
                        actionButton("视频", "video.fill") { CallManager.shared.startCall(to: userId, video: true) }
                        actionButton(muted ? "取消静音" : "静音", muted ? "bell.fill" : "bell.slash.fill") {
                            core.chats.update(convId, .mute(!muted))
                        }
                    }
                    .padding(.top, 6)
                }
                .frame(maxWidth: .infinity)
                .padding(.vertical, 8)
                .onTapGesture { Router.shared.push(.profile(userId)) }
            }
            .listRowBackground(Color.clear)
            Section { ConversationToggles(convId: convId) }
            Section {
                NavigationLink(value: Route.safety(userId)) {
                    SettingRow(icon: "checkmark.shield.fill", tint: Theme.success, title: "查看安全码")
                }
                .accessibilityIdentifier("safetyNumberEntry")
                NavigationLink(value: Route.userMoments(userId)) {
                    SettingRow(icon: "camera.aperture", tint: .teal, title: "朋友圈")
                }
            }
            Section {
                Button("清空聊天记录") { confirmClear = true }.foregroundStyle(Theme.textPrimary)
                Button(core.contacts.isBlocked(userId) ? "取消拉黑" : "拉黑") { confirmBlock = true }.foregroundStyle(Theme.textPrimary)
                    .accessibilityIdentifier("blockButton")
                Button("删除好友", role: .destructive) { confirmDelete = true }.foregroundStyle(Theme.danger)
            }
        }
        .navigationTitle("聊天信息")
        .navigationBarTitleDisplayMode(.inline)
        .task {
            // 刷新对方资料（邮箱只在互为好友时由服务端返回）
            _ = await core.contacts.fetchProfile(userId)
            await core.contacts.refreshVerified(userId)
        }
        .confirmationDialog("确定清空与该联系人的聊天记录？", isPresented: $confirmClear, titleVisibility: .visible) {
            Button("清空", role: .destructive) {
                if core.chats.update(convId, .clear) { core.toast = "已清空" }
            }
        }
        .confirmationDialog(core.contacts.isBlocked(userId) ? "取消拉黑后可以互相发消息" : "拉黑后你将不再收到对方的消息", isPresented: $confirmBlock, titleVisibility: .visible) {
            Button(core.contacts.isBlocked(userId) ? "取消拉黑" : "拉黑", role: core.contacts.isBlocked(userId) ? nil : .destructive) {
                let blocked = core.contacts.isBlocked(userId)
                core.run {
                    try await core.contacts.setBlocked(userId, !blocked)
                }
            }
        }
        .confirmationDialog("删除好友后将同时删除与其的聊天", isPresented: $confirmDelete, titleVisibility: .visible) {
            Button("删除好友", role: .destructive) {
                core.run {
                    try await core.contacts.removeFriend(userId)
                    core.chats.reload()
                    Router.shared.chatPath = .init()
                    Router.shared.contactsPath = .init()
                }
            }
            .accessibilityIdentifier("confirmRemoveFriend")
        }
    }

    private func actionButton(_ title: String, _ icon: String, _ run: @escaping () -> Void) -> some View {
        Button(action: run) {
            VStack(spacing: 4) {
                Image(systemName: icon).font(.system(size: 18))
                Text(title).font(Fonts.sized(12))
            }
            .foregroundStyle(Theme.accent)
            .frame(width: 84, height: 58)
            .background(RoundedRectangle(cornerRadius: Theme.cardRadius).fill(Theme.surface))
        }
        .buttonStyle(.plain)
    }
}
