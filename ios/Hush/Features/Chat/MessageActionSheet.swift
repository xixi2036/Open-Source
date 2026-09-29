import SwiftUI

/// 长按消息菜单：快捷回应 + 回复 / 复制 / 撤回 / 删除
struct MessageActionSheet: View {
    let message: MessageItem
    let model: ChatModel
    @Environment(\.dismiss) private var dismiss
    static let emojis = ["👍", "❤️", "😂", "😮", "😢", "🙏"]

    private var text: String? {
        if case let .text(t) = message.body { return t }
        return nil
    }
    private var canInteract: Bool {
        switch message.body {
        case .text, .attachments: return !message.deleted
        default: return false
        }
    }

    /// 转发：未撤回的文字，或已上传的附件（发送中 / 失败的附件还没有服务端密文）
    private var canForward: Bool {
        guard !message.deleted else { return false }
        switch message.body {
        case .text: return true
        case .attachments: return !(message.isOutgoing && (message.status == .sending || message.status == .failed))
        default: return false
        }
    }

    var body: some View {
        VStack(spacing: 0) {
            if canInteract {
                HStack(spacing: 0) {
                    ForEach(Self.emojis, id: \.self) { e in
                        Button {
                            model.react(message, e); dismiss()
                        } label: {
                            Text(e).font(.system(size: 30)).frame(maxWidth: .infinity, minHeight: 52)
                        }
                        .accessibilityIdentifier("react_\(e)")
                    }
                }
                .padding(.horizontal, 8)
                .background(RoundedRectangle(cornerRadius: 26).fill(Theme.surface))
                .padding(16)
            }
            VStack(spacing: 0) {
                if canInteract {
                    action("回复", "arrowshape.turn.up.left", id: "action_reply") { model.quoting = message }
                }
                if canForward {
                    action("转发", "arrowshape.turn.up.right", id: "action_forward") { model.pendingForward = message }
                }
                if let text {
                    action("复制", "doc.on.doc", id: "action_copy") { UIPasteboard.general.string = text; HushCore.shared.toast = "已复制" }
                }
                if model.canRecall(message) {
                    action("撤回", "arrow.uturn.backward", id: "action_recall") { model.recall(message) }
                }
                action("删除", "trash", role: .destructive, id: "action_delete") { model.deleteLocal(message) }
            }
            .background(RoundedRectangle(cornerRadius: Theme.cardRadius).fill(Theme.surface))
            .padding(.horizontal, 16)
            Spacer(minLength: 0)
        }
        .padding(.top, 8)
        .presentationDetents([.height(canInteract ? (canForward ? 378 : 330) : 120)])
        .presentationDragIndicator(.visible)
    }

    private func action(_ title: String, _ icon: String, role: ButtonRole? = nil, id: String, _ run: @escaping () -> Void) -> some View {
        Button(role: role) {
            dismiss()
            run()
        } label: {
            HStack {
                Text(title)
                Spacer()
                Image(systemName: icon)
            }
            .foregroundStyle(role == .destructive ? Theme.danger : Theme.textPrimary)
            .padding(.horizontal, 16).frame(height: 48)
            .contentShape(Rectangle())
        }
        .accessibilityIdentifier(id)
    }
}
