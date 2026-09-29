import SwiftUI
import PhotosUI

/// 我 Tab：个人卡片 + 入口
struct MeView: View {
    var core = HushCore.shared

    var body: some View {
        let me = core.myProfile
        List {
            Section {
                NavigationLink(value: Route.editProfile) {
                    HStack(spacing: 14) {
                        AvatarView(seed: core.myId, name: me?.name ?? "", avatarId: me?.avatarId, size: 64)
                        VStack(alignment: .leading, spacing: 4) {
                            Text(me?.name ?? "").font(Fonts.title).foregroundStyle(Theme.textPrimary)
                                .accessibilityIdentifier("myName")
                            Text(core.account?.email ?? "").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                            if let about = me?.about, !about.isEmpty {
                                Text(about).font(Fonts.secondary).foregroundStyle(Theme.textSecondary).lineLimit(1)
                            }
                        }
                        Spacer()
                        Button { Router.shared.push(.myQR) } label: {
                            Image(systemName: "qrcode").font(.system(size: 20)).foregroundStyle(Theme.textSecondary)
                        }
                        .buttonStyle(.plain)
                        .accessibilityIdentifier("meQRButton")
                    }
                    .padding(.vertical, 8)
                }
                .accessibilityIdentifier("myCard")
            }
            Section {
                NavigationLink(value: Route.userMoments(core.myId)) { SettingRow(icon: "camera.aperture", tint: .teal, title: "我的朋友圈") }
                NavigationLink(value: Route.myQR) { SettingRow(icon: "qrcode", tint: .green, title: "我的二维码") }
            }
            Section {
                NavigationLink(value: Route.settings) { SettingRow(icon: "gearshape.fill", tint: .gray, title: "设置") }
                    .accessibilityIdentifier("settingsEntry")
            }
        }
        .navigationTitle("我")
    }
}

/// 编辑资料：头像 / 昵称 / 签名
struct EditProfileView: View {
    var core = HushCore.shared
    @State private var name = HushCore.shared.myProfile?.name ?? ""
    @State private var about = HushCore.shared.myProfile?.about ?? ""
    @State private var avatarURL: URL?
    @State private var saving = false
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        Form {
            Section {
                HStack {
                    Spacer()
                    let (myId, avatarId) = (core.myId, core.myProfile?.avatarId)
                    AvatarPickerField(size: 96, avatarURL: $avatarURL) {
                        AvatarView(seed: myId, name: name, avatarId: avatarId, size: 96)
                    }
                    Spacer()
                }
                .listRowBackground(Color.clear)
            }
            Section("昵称") {
                TextField("昵称", text: $name).accessibilityIdentifier("editNameField")
            }
            Section("签名") {
                TextField("介绍一下自己", text: $about, axis: .vertical).lineLimit(1...4)
                    .accessibilityIdentifier("editAboutField")
            }
        }
        .navigationTitle("编辑资料")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .confirmationAction) {
                Button(saving ? "保存中…" : "保存") { save() }
                    .disabled(saving || name.trimmingCharacters(in: .whitespaces).isEmpty)
                    .accessibilityIdentifier("saveProfile")
            }
        }
    }

    private func save() {
        saving = true
        Task {
            do {
                try await core.updateMyProfile(name: name.trimmingCharacters(in: .whitespaces), about: about, avatarPath: avatarURL?.path)
                core.toast = "已保存"
                dismiss()
            } catch { core.toast = describe(error) }
            saving = false
        }
    }
}
