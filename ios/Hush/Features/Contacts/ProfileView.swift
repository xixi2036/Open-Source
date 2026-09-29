import SwiftUI

/// 个人资料页：发消息 / 语音 / 视频 / 朋友圈；非好友可添加
struct ProfileView: View {
    let userId: String
    var core = HushCore.shared
    @State private var profile: Profile?
    @State private var showAdd = false
    @State private var greeting = ""

    var body: some View {
        let p = profile ?? core.contacts.profile(userId)
        let isMe = userId == core.myId
        let friend = core.contacts.isFriend(userId)
        List {
            Section {
                HStack(spacing: 16) {
                    AvatarView(seed: userId, name: p?.name ?? "", avatarId: p?.avatarId, size: 72)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(p?.name ?? "用户").font(Fonts.title).accessibilityIdentifier("profileName")
                        if let email = p?.email, !email.isEmpty {
                            Text(email).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                                .accessibilityIdentifier("profileEmail")
                        }
                        if let about = p?.about, !about.isEmpty {
                            Text(about).font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                        }
                        if core.contacts.isBlocked(userId) {
                            Label("已拉黑", systemImage: "hand.raised.fill").font(Fonts.secondary).foregroundStyle(Theme.danger)
                        }
                    }
                }
                .padding(.vertical, 8)
            }
            Section {
                NavigationLink(value: Route.userMoments(userId)) {
                    Label("朋友圈", systemImage: "camera.aperture")
                }
                .accessibilityIdentifier("profileMoments")
            }
            if !isMe {
                if friend {
                    Section {
                        Button { Router.shared.openChat(core.chats.directConversationId(userId)) } label: {
                            Label("发消息", systemImage: "message.fill").frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .accessibilityIdentifier("profileSendMessage")
                        Button { CallManager.shared.startCall(to: userId, video: false) } label: {
                            Label("语音通话", systemImage: "phone.fill").frame(maxWidth: .infinity, alignment: .leading)
                        }
                        Button { CallManager.shared.startCall(to: userId, video: true) } label: {
                            Label("视频通话", systemImage: "video.fill").frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                } else if pendingOutgoing {
                    Section { Text("已发送好友申请，等待对方验证").foregroundStyle(Theme.textSecondary).frame(maxWidth: .infinity) }
                } else {
                    Section {
                        Button {
                            greeting = "我是\(core.myProfile?.name ?? "")"
                            showAdd = true
                        } label: {
                            Label("添加到通讯录", systemImage: "person.badge.plus").frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .accessibilityIdentifier("addToContacts")
                    }
                }
            }
        }
        .navigationTitle("详细资料")
        .navigationBarTitleDisplayMode(.inline)
        .task { profile = await core.contacts.fetchProfile(userId) }
        .alert("填写验证消息", isPresented: $showAdd) {
            TextField("验证消息", text: $greeting).accessibilityIdentifier("greetingField")
            Button("取消", role: .cancel) {}
            Button("发送") { sendRequest() }.accessibilityIdentifier("sendFriendRequest")
        }
    }

    private var pendingOutgoing: Bool {
        core.contacts.friendRequests.outgoing.contains { $0.to.id == userId && $0.status == "pending" }
    }

    private func sendRequest() {
        Task {
            do {
                try await core.contacts.sendFriendRequest(to: userId, message: greeting)
                core.toast = "已发送好友申请"
            } catch { core.toast = describe(error) }
        }
    }
}

/// 添加好友：邮箱搜索 / 扫一扫 / 我的二维码
struct AddFriendView: View {
    var core = HushCore.shared
    @State private var email = ""
    @State private var result: Profile?
    @State private var searched = false
    @State private var searching = false
    @State private var showScanner = false

    var body: some View {
        List {
            Section {
                HStack {
                    Image(systemName: "magnifyingglass").foregroundStyle(Theme.textSecondary)
                    TextField("输入对方邮箱", text: $email)
                        .keyboardType(.emailAddress).textInputAutocapitalization(.never).autocorrectionDisabled()
                        .submitLabel(.search)
                        .onSubmit { search() }
                        .accessibilityIdentifier("searchEmailField")
                    if searching { ProgressView() } else {
                        Button("搜索") { search() }.disabled(!email.contains("@"))
                            .accessibilityIdentifier("searchEmailButton")
                    }
                }
            }
            if let r = result {
                Section("搜索结果") {
                    NavigationLink(value: Route.profile(r.id)) {
                        HStack(spacing: 12) {
                            AvatarView(seed: r.id, name: r.name, avatarId: r.avatarId, size: 48)
                            VStack(alignment: .leading) {
                                Text(r.name).font(.system(size: 17, weight: .medium))
                                Text(core.contacts.isFriend(r.id) ? "已是好友" : (r.about.isEmpty ? "点击查看资料" : r.about))
                                    .font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(1)
                            }
                        }
                    }
                    .accessibilityIdentifier("searchResult")
                }
            } else if searched {
                Section { Text("没有找到该用户").foregroundStyle(Theme.textSecondary) }
            }
            Section {
                Button { showScanner = true } label: { SettingRow(icon: "qrcode.viewfinder", title: "扫一扫") }
                    .accessibilityIdentifier("scanButton")
                NavigationLink(value: Route.myQR) { SettingRow(icon: "qrcode", tint: .green, title: "我的二维码") }
                    .accessibilityIdentifier("myQREntry")
            }
        }
        .navigationTitle("添加好友")
        .navigationBarTitleDisplayMode(.inline)
        .sheet(isPresented: $showScanner) {
            QRScannerSheet { code in
                if let id = QRCode.userId(from: code) { Router.shared.push(.profile(id)) } else { core.toast = "不是 Hush 名片二维码" }
            }
        }
    }

    private func search() {
        let e = email.trimmingCharacters(in: .whitespaces).lowercased()
        guard e.contains("@") else { return }
        searching = true
        Task {
            do { result = try await core.contacts.searchUser(email: e) } catch { result = nil; core.toast = describe(error) }
            searched = true
            searching = false
        }
    }
}

/// 我的二维码
struct MyQRCodeView: View {
    var core = HushCore.shared
    var body: some View {
        let me = core.myProfile
        VStack(spacing: 20) {
            VStack(spacing: 16) {
                HStack(spacing: 12) {
                    AvatarView(seed: core.myId, name: me?.name ?? "", avatarId: me?.avatarId, size: 52)
                    VStack(alignment: .leading) {
                        Text(me?.name ?? "").font(Fonts.title)
                        Text(core.account?.email ?? "").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                    }
                    Spacer()
                }
                QRImageView(payload: core.contacts.myQrPayload)
                    .frame(width: 240, height: 240)
                    .padding(12)
                    .background(Color.white)
                    .clipShape(RoundedRectangle(cornerRadius: 12))
                Text("扫一扫上面的二维码，加我为好友").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
            }
            .padding(24)
            .background(RoundedRectangle(cornerRadius: 16).fill(Theme.surface))
            .padding(24)
            Spacer()
        }
        .background(Theme.background)
        .navigationTitle("我的二维码")
        .navigationBarTitleDisplayMode(.inline)
    }
}
