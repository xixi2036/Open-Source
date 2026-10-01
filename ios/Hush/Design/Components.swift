import SwiftUI
import UIKit

/// 空状态：插图 + 文案
struct EmptyStateView: View {
    let systemImage: String
    let text: String
    var body: some View {
        VStack(spacing: 16) {
            Image(systemName: systemImage)
                .font(.system(size: 56, weight: .light))
                .foregroundStyle(Theme.accent.opacity(0.6))
            Text(text)
                .font(Fonts.body)
                .foregroundStyle(Theme.textSecondary)
                .multilineTextAlignment(.center)
        }
        .padding(32)
        .frame(maxWidth: .infinity)
    }
}

/// 主按钮（accent 背景）
struct PrimaryButtonStyle: ButtonStyle {
    var enabled = true
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(Fonts.sized(17, weight: .semibold))
            .foregroundStyle(Theme.onAccent)
            .frame(maxWidth: .infinity, minHeight: 50)
            .background(RoundedRectangle(cornerRadius: Theme.cardRadius).fill(Theme.accent.opacity(enabled ? 1 : 0.4)))
            .opacity(configuration.isPressed ? 0.8 : 1)
    }
}

/// 红点 / 数字徽章
struct BadgeView: View {
    let count: Int
    var muted = false
    var body: some View {
        if muted {
            Circle().fill(Theme.textSecondary.opacity(0.6)).frame(width: 10, height: 10)
        } else if count > 0 {
            Text(count > 99 ? "99+" : "\(count)")
                .font(Fonts.sized(12, weight: .semibold))
                .foregroundStyle(.white)
                .padding(.horizontal, count > 9 ? 6 : 0)
                .frame(minWidth: 20, minHeight: 20)
                .background(Capsule().fill(Theme.accent))
        }
    }
}

/// 顶部连接状态细条
struct ConnectionBanner: View {
    var core = HushCore.shared
    var body: some View {
        if core.connection != .connected {
            HStack(spacing: 6) {
                if core.connection == .connecting { ProgressView().controlSize(.mini) }
                Text(core.connection == .connecting ? "连接中…" : "未连接")
                    .font(Fonts.secondary)
            }
            .foregroundStyle(Theme.textSecondary)
            .frame(maxWidth: .infinity)
            .padding(.vertical, 4)
            .background(Theme.surface)
            .accessibilityIdentifier("connectionBanner")
        }
    }
}

/// 设置页的一行（图标 + 标题 + 右侧值）
struct SettingRow: View {
    let icon: String
    var tint: Color = Theme.accent
    let title: String
    var value: String? = nil
    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: icon)
                .font(.system(size: 15, weight: .medium))
                .foregroundStyle(.white)
                .frame(width: 28, height: 28)
                .background(RoundedRectangle(cornerRadius: 7).fill(tint))
            Text(title).foregroundStyle(Theme.textPrimary)
            Spacer()
            if let value { Text(value).foregroundStyle(Theme.textSecondary) }
        }
    }
}

/// 收起键盘
@MainActor
func hideKeyboard() {
    UIApplication.shared.sendAction(#selector(UIResponder.resignFirstResponder), to: nil, from: nil, for: nil)
}
