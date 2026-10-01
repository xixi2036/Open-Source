import SwiftUI
import PhotosUI

/// 资料设置页（新用户或昵称为空）：头像 + 昵称（必填）+ 完成
struct ProfileSetupView: View {
    @State private var name = HushCore.shared.account?.profile.name ?? ""
    @State private var avatarURL: URL?
    @State private var loading = false
    @State private var error: String?
    var core = HushCore.shared

    var body: some View {
        NavigationStack {
            VStack(spacing: 24) {
                Text("设置你的资料").font(Fonts.sized(26, weight: .bold)).foregroundStyle(Theme.textPrimary)
                Text("好友会看到你的头像和昵称").font(Fonts.body).foregroundStyle(Theme.textSecondary)
                AvatarPickerField(size: 110, avatarURL: $avatarURL) {
                    Circle().fill(Theme.surface)
                        .overlay(Image(systemName: "person.fill").font(.system(size: 44)).foregroundStyle(Theme.textSecondary))
                }
                .accessibilityIdentifier("avatarPicker")
                TextField("昵称（必填）", text: $name)
                    .authField()
                    .accessibilityIdentifier("nameField")
                if let error { Text(error).font(Fonts.secondary).foregroundStyle(Theme.danger) }
                Spacer()
                Button {
                    save()
                } label: {
                    if loading { ProgressView().tint(.white) } else { Text("完成") }
                }
                .buttonStyle(PrimaryButtonStyle(enabled: valid && !loading))
                .disabled(!valid || loading)
                .accessibilityIdentifier("profileDone")
            }
            .padding(24)
            .background(Theme.background)
        }
    }

    private var valid: Bool { !name.trimmingCharacters(in: .whitespaces).isEmpty }

    private func save() {
        loading = true; error = nil
        Task {
            do {
                try await core.completeProfile(name: name.trimmingCharacters(in: .whitespaces), avatarPath: avatarURL?.path)
            } catch { self.error = describe(error) }
            loading = false
        }
    }
}
