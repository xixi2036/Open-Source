import SwiftUI

/// 设置：隐私 / 通知 / 连接 / 关于 / 退出登录
struct SettingsView: View {
    var core = HushCore.shared
    @Bindable var lock = ScreenLock.shared
    @State private var confirmLogout = false

    var body: some View {
        let s = core.settings
        Form {
            Section("隐私") {
                Toggle(isOn: bind(\.readReceipts)) { SettingRow(icon: "checkmark.message.fill", title: "已读回执") }
                    .accessibilityIdentifier("readReceiptsToggle")
                Toggle(isOn: bind(\.typingIndicators)) { SettingRow(icon: "ellipsis.bubble.fill", tint: .teal, title: "正在输入提示") }
                    .accessibilityIdentifier("typingToggle")
                Picker(selection: Binding(get: { s.defaultDisappearingSecs }, set: { v in
                    var n = core.settings; n.defaultDisappearingSecs = v; core.saveSettings(n)
                })) {
                    ForEach(Fmt.disappearingOptions, id: \.0) { Text($0.1).tag($0.0) }
                } label: {
                    SettingRow(icon: "timer", tint: .orange, title: "默认阅后即焚")
                }
                .pickerStyle(.navigationLink)
                Toggle(isOn: Binding(get: { lock.lockEnabled }, set: { lock.enable($0) })) {
                    SettingRow(icon: "lock.fill", tint: .indigo, title: "屏幕锁")
                }
                .accessibilityIdentifier("screenLockToggle")
                if lock.lockEnabled {
                    // 宽限时间：离开 App 不超过这么久再回来不用重新验证
                    Picker(selection: $lock.graceSecs) {
                        ForEach(ScreenLock.graceOptions, id: \.0) { Text($0.1).tag($0.0) }
                    } label: {
                        SettingRow(icon: "clock.fill", tint: .indigo, title: "自动锁定")
                    }
                    .pickerStyle(.navigationLink)
                    .accessibilityIdentifier("lockGracePicker")
                }
                Toggle(isOn: $lock.privacyScreen) { SettingRow(icon: "eye.slash.fill", tint: .gray, title: "屏幕安全") }
                    .accessibilityIdentifier("privacyScreenToggle")
                NavigationLink(value: Route.blocked) {
                    SettingRow(icon: "hand.raised.fill", tint: Theme.danger, title: "黑名单", value: "\(core.contacts.blocked.count)")
                }
            }
            Section {
                Toggle(isOn: bind(\.notifications)) { SettingRow(icon: "bell.badge.fill", tint: .red, title: "消息通知") }
                    .accessibilityIdentifier("notificationsToggle")
                SettingRow(icon: "text.bubble.fill", tint: .green, title: "显示内容", value: "仅提示新消息")
            } header: {
                Text("通知")
            } footer: {
                Text("为保护隐私，通知只显示“来自 xx 的新消息”，不显示消息内容。切到后台时启用“屏幕安全”会遮挡应用预览。")
            }
            Section("连接") {
                SettingRow(icon: "server.rack", tint: .gray, title: "服务器地址", value: core.serverURL)
                SettingRow(icon: "antenna.radiowaves.left.and.right", tint: connColor, title: "连接状态", value: connText)
            }
            Section {
                NavigationLink(value: Route.about) { SettingRow(icon: "info.circle.fill", title: "关于 Hush") }
            }
            Section {
                Button("退出登录", role: .destructive) { confirmLogout = true }
                    .frame(maxWidth: .infinity)
                    .foregroundStyle(Theme.danger)
                    .accessibilityIdentifier("logoutButton")
            }
        }
        .navigationTitle("设置")
        .navigationBarTitleDisplayMode(.inline)
        .confirmationDialog("退出后本机聊天记录将被清除", isPresented: $confirmLogout, titleVisibility: .visible) {
            Button("退出登录", role: .destructive) { Task { await core.logout() } }
        }
    }

    private func bind(_ kp: WritableKeyPath<Settings, Bool>) -> Binding<Bool> {
        Binding(get: { core.settings[keyPath: kp] }, set: { v in
            var n = core.settings
            n[keyPath: kp] = v
            core.saveSettings(n)
        })
    }

    private var connText: String {
        switch core.connection {
        case .connected: return "已连接"
        case .connecting: return "连接中…"
        case .disconnected: return "未连接"
        }
    }

    private var connColor: Color {
        core.connection == .connected ? Theme.success : (core.connection == .connecting ? .orange : Theme.danger)
    }
}

/// 黑名单
struct BlockedListView: View {
    var core = HushCore.shared
    var body: some View {
        List(core.contacts.blocked, id: \.self) { uid in
            HStack(spacing: 12) {
                UserAvatar(userId: uid, size: 40)
                Text(core.contacts.displayName(uid))
                Spacer()
                Button("取消拉黑") {
                    core.run {
                        try await core.contacts.setBlocked(uid, false)
                    }
                }
                .buttonStyle(.bordered)
            }
        }
        .overlay { if core.contacts.blocked.isEmpty { EmptyStateView(systemImage: "hand.raised", text: "黑名单为空") } }
        .navigationTitle("黑名单")
        .navigationBarTitleDisplayMode(.inline)
    }
}

/// 关于
struct AboutView: View {
    var body: some View {
        List {
            Section {
                VStack(spacing: 10) {
                    Image("Logo").resizable().frame(width: 80, height: 80)
                        .clipShape(RoundedRectangle(cornerRadius: 18, style: .continuous))
                    Text("Hush 密语").font(Fonts.title)
                    Text("版本 \(version)").font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
                }
                .frame(maxWidth: .infinity).padding(.vertical, 12)
            }
            .listRowBackground(Color.clear)
            Section("安全") {
                Text("端到端加密由 vodozemac（Olm/Megolm）提供")
                Text("附件使用 AES-256-GCM 加密，本地数据库字段级加密，密钥保存在系统钥匙串")
                    .font(Fonts.secondary).foregroundStyle(Theme.textSecondary)
            }
            Section("开源许可") {
                Text("vodozemac — Apache-2.0")
                Text("WebRTC — BSD-3-Clause")
                Text("UniFFI — MPL-2.0")
                Text("SQLite — Public Domain")
                Text("图标：SF Symbols（Apple）").foregroundStyle(Theme.textSecondary)
            }
        }
        .navigationTitle("关于")
        .navigationBarTitleDisplayMode(.inline)
    }

    private var version: String {
        let v = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "0"
        let b = Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "0"
        return "\(v) (\(b))"
    }
}
