import SwiftUI

/// 选择一个好友
struct FriendPickerSheet: View {
    let title: String
    var onPick: (Profile) -> Void
    @Environment(\.dismiss) private var dismiss
    var core = HushCore.shared

    var body: some View {
        NavigationStack {
            List(core.contacts.friends, id: \.id) { p in
                Button {
                    dismiss()
                    onPick(p)
                } label: {
                    HStack(spacing: 12) {
                        UserAvatar(userId: p.id, size: 40)
                        Text(p.name).foregroundStyle(Theme.textPrimary)
                    }
                }
                .accessibilityIdentifier("pick_\(p.name)")
            }
            .listStyle(.plain)
            .overlay { if core.contacts.friends.isEmpty { EmptyStateView(systemImage: "person.2", text: "还没有好友") } }
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } } }
        }
    }
}

/// 多选好友（建群 / 拉人 / 可见范围）
struct MultiFriendPicker: View {
    let candidates: [Profile]
    @Binding var selected: Set<String>

    var body: some View {
        ForEach(candidates, id: \.id) { p in
            Button {
                if selected.contains(p.id) { selected.remove(p.id) } else { selected.insert(p.id) }
            } label: {
                HStack(spacing: 12) {
                    Image(systemName: selected.contains(p.id) ? "checkmark.circle.fill" : "circle")
                        .font(.system(size: 22))
                        .foregroundStyle(selected.contains(p.id) ? Theme.accent : Theme.textSecondary)
                    UserAvatar(userId: p.id, size: 40)
                    Text(p.name).foregroundStyle(Theme.textPrimary)
                    Spacer()
                }
            }
            .accessibilityIdentifier("select_\(p.name)")
        }
    }
}

/// 新建群聊：多选好友 + 群名
struct NewGroupSheet: View {
    @Environment(\.dismiss) private var dismiss
    @State private var name = ""
    @State private var selected: Set<String> = []
    @State private var creating = false
    var core = HushCore.shared

    var body: some View {
        NavigationStack {
            List {
                Section {
                    TextField("群名称", text: $name).accessibilityIdentifier("groupNameField")
                }
                Section("选择成员") {
                    MultiFriendPicker(candidates: core.contacts.friends, selected: $selected)
                }
            }
            .navigationTitle("新建群聊")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("取消") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(creating ? "创建中…" : "创建(\(selected.count))") { create() }
                        .disabled(selected.isEmpty || name.trimmingCharacters(in: .whitespaces).isEmpty || creating)
                        .accessibilityIdentifier("createGroupButton")
                }
            }
        }
    }

    private func create() {
        creating = true
        Task {
            do {
                let g = try await core.contacts.createGroup(name: name.trimmingCharacters(in: .whitespaces), memberIds: Array(selected))
                dismiss()
                Router.shared.openChat("g:\(g.id)")
            } catch { core.toast = describe(error) }
            creating = false
        }
    }
}
