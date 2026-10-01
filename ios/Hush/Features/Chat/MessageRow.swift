import SwiftUI

/// 聊天页的一行：系统消息 / 通话记录 / 普通气泡。
/// C2：实现 Equatable（只比较消息内容与选择状态，回调不参与比较），配合 .equatable()，
/// 列表里其他状态变化（新消息、高亮、正在输入）时没变的行不重新求值。
struct MessageRow: View, Equatable {
    let message: MessageItem
    let isGroup: Bool
    /// 多选模式
    var selecting = false
    var isSelected = false
    var onLongPress: () -> Void
    var onQuoteTap: (String) -> Void
    var onOpenImage: (String) -> Void
    var onOpenVideo: (String) -> Void = { _ in }
    var onShare: (URL) -> Void
    var onResend: () -> Void
    var onToggleSelect: () -> Void = {}

    nonisolated static func == (a: MessageRow, b: MessageRow) -> Bool {
        a.message == b.message && a.isGroup == b.isGroup && a.selecting == b.selecting && a.isSelected == b.isSelected
    }

    var body: some View {
        switch message.body {
        case let .system(text):
            SystemMessageView(text: message.deleted ? MessagePreview.recallText(message, inGroup: isGroup) : text)
        case let .callLog(video, secs, _, _, reason):
            CallLogRow(message: message, video: video, secs: secs, reason: reason)
        default:
            HStack(spacing: 0) {
                if selecting {
                    Image(systemName: isSelected ? "checkmark.circle.fill" : "circle")
                        .font(.system(size: 22))
                        .foregroundStyle(isSelected ? Theme.accent : Theme.textSecondary)
                        .padding(.leading, 12)
                        .accessibilityHidden(true)
                }
                BubbleRow(message: message, isGroup: isGroup, onLongPress: onLongPress, onQuoteTap: onQuoteTap,
                          onOpenImage: onOpenImage, onOpenVideo: onOpenVideo, onShare: onShare, onResend: onResend)
                    .allowsHitTesting(!selecting)
            }
            .contentShape(Rectangle())
            // 多选模式下整行都是选择区域（气泡里的链接、图片、重发按钮不再响应）
            .onTapGesture { if selecting { onToggleSelect() } }
            .accessibilityAddTraits(selecting && isSelected ? .isSelected : [])
        }
    }
}

struct SystemMessageView: View {
    let text: String
    var body: some View {
        Text(text)
            .font(Fonts.secondary)
            .foregroundStyle(Theme.textSecondary)
            .multilineTextAlignment(.center)
            .padding(.horizontal, 12).padding(.vertical, 5)
            .background(Capsule().fill(Theme.surface))
            .frame(maxWidth: .infinity)
            .padding(.vertical, 4)
            .accessibilityIdentifier("systemMessage")
    }
}

struct CallLogRow: View {
    let message: MessageItem
    let video: Bool
    let secs: UInt32
    let reason: CallEndReason

    var body: some View {
        let text = MessagePreview.callLogText(video: video, secs: secs, reason: reason)
        HStack(spacing: 6) {
            Image(systemName: video ? "video.fill" : "phone.fill")
            Text(text)
            Text(Fmt.bubbleTime(message.sentAt)).font(Fonts.timestamp).foregroundStyle(Theme.textSecondary)
        }
        .font(Fonts.secondary)
        .foregroundStyle(reason == .missed ? Theme.danger : Theme.textSecondary)
        .padding(.horizontal, 12).padding(.vertical, 6)
        .background(Capsule().fill(Theme.surface))
        .frame(maxWidth: .infinity)
        .padding(.vertical, 4)
        .accessibilityIdentifier("callLog")
    }
}

struct BubbleRow: View {
    let message: MessageItem
    let isGroup: Bool
    var onLongPress: () -> Void
    var onQuoteTap: (String) -> Void
    var onOpenImage: (String) -> Void
    var onOpenVideo: (String) -> Void = { _ in }
    var onShare: (URL) -> Void
    var onResend: () -> Void
    var core = HushCore.shared

    private var out: Bool { message.isOutgoing }
    private var isMediaOnly: Bool {
        if case let .attachments(items, caption) = message.body, let f = items.first, f.kind == .image || f.kind == .video,
           caption?.isEmpty ?? true, message.quote == nil { return true }
        return false
    }

    var body: some View {
        HStack(alignment: .bottom, spacing: 8) {
            if out { Spacer(minLength: 48) }
            if !out && isGroup {
                UserAvatar(userId: message.senderId, size: 30)
                    .onTapGesture { Router.shared.push(.profile(message.senderId)) }
            }
            VStack(alignment: out ? .trailing : .leading, spacing: 2) {
                if !out && isGroup {
                    Text(core.contacts.displayName(message.senderId))
                        .font(Fonts.sized(12, weight: .medium))
                        .foregroundStyle(Theme.namePalette[Theme.paletteIndex(for: message.senderId)])
                        .padding(.leading, 12)
                }
                HStack(alignment: .center, spacing: 6) {
                    if out && message.status == .failed {
                        Button(action: onResend) {
                            Image(systemName: "exclamationmark.circle.fill").font(.system(size: 20)).foregroundStyle(Theme.danger)
                        }
                        .accessibilityLabel("发送失败，重新发送")
                        .accessibilityIdentifier("resendButton")
                    }
                    bubble
                }
                if !message.reactions.isEmpty {
                    ReactionChips(reactions: message.reactions)
                        .padding(out ? .trailing : .leading, 6)
                }
            }
            if !out { Spacer(minLength: 48) }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 2)
    }

    /// 提到我的消息：气泡左侧强调竖线
    private var mentionsMe: Bool { MentionText.mentions(message, user: core.myId) }

    @ViewBuilder private var bubble: some View {
        VStack(alignment: .leading, spacing: 6) {
            if message.forwarded {
                Label("转发的消息", systemImage: "arrowshape.turn.up.right")
                    .font(Fonts.sized(11, weight: .medium))
                    .foregroundStyle(out ? Theme.onAccent.opacity(0.8) : Theme.textSecondary)
                    .padding(.horizontal, isMediaOnly ? 8 : 0).padding(.top, isMediaOnly ? 4 : 0)
                    .accessibilityIdentifier("forwardedLabel")
            }
            if let q = message.quote {
                QuoteBlock(quote: q, outgoing: out)
                    .onTapGesture { onQuoteTap(q.id) }
            }
            content
            footer
        }
        .padding(.horizontal, isMediaOnly ? 4 : 12)
        .padding(.vertical, isMediaOnly ? 4 : 8)
        .background(
            RoundedRectangle(cornerRadius: Theme.bubbleRadius, style: .continuous)
                .fill(out ? Theme.accent : Theme.incomingBubble)
        )
        .overlay(alignment: .leading) {
            if mentionsMe {
                Capsule().fill(Theme.accent).frame(width: 3)
                    .padding(.vertical, 2)
                    .offset(x: -7)
                    .accessibilityIdentifier("mentionBar")
            }
        }
        .contentShape(RoundedRectangle(cornerRadius: Theme.bubbleRadius))
        .onLongPressGesture(minimumDuration: 0.35) { onLongPress() }
        .accessibilityElement(children: .contain)
        .accessibilityIdentifier(out ? "bubble_out" : "bubble_in")
    }

    @ViewBuilder private var content: some View {
        switch message.body {
        case let .text(text):
            // 链接可点击（https 直接打开，http 先确认，见 ChatView 的 openURL）；部分选择走长按菜单里的「选择文字」
            Text(LinkText.attributed(text, message.mentions, outgoing: out))
                .font(Fonts.body)
                .foregroundStyle(out ? Theme.onAccent : Theme.textPrimary)
                .tint(out ? Theme.onAccent : Theme.accent)
                .textSelection(.disabled)
                .fixedSize(horizontal: false, vertical: true)
                .accessibilityIdentifier("messageText")
        case let .attachments(items, caption):
            ForEach(Array(items.enumerated()), id: \.offset) { i, item in
                switch item.kind {
                case .image:
                    ImageAttachmentView(message: message, item: item, index: i, onOpen: onOpenImage)
                case .voice:
                    VoiceAttachmentView(message: message, item: item, index: i, outgoing: out)
                case .video:
                    VideoAttachmentView(message: message, item: item, index: i, onOpen: onOpenVideo)
                case .file:
                    FileAttachmentView(message: message, item: item, index: i, outgoing: out, onShare: onShare)
                }
            }
            if let caption, !caption.isEmpty {
                Text(caption).font(Fonts.body).foregroundStyle(out ? Theme.onAccent : Theme.textPrimary)
                    .padding(.horizontal, isMediaOnly ? 8 : 0)
            }
        case .undecryptable:
            Text("⚠️ 无法解密的消息")
                .font(Fonts.body.italic())
                .foregroundStyle(Theme.textSecondary)
                .accessibilityIdentifier("undecryptable")
        default:
            EmptyView()
        }
    }

    private var footer: some View {
        HStack(spacing: 3) {
            if message.expireSecs > 0 {
                Image(systemName: "timer").font(.system(size: 10))
                    .accessibilityIdentifier("disappearingIcon")
            }
            Text(Fmt.bubbleTime(message.sentAt)).font(Fonts.timestamp)
            if out { StatusIcon(status: message.status) }
        }
        .foregroundStyle(out ? Theme.onAccent.opacity(0.8) : Theme.textSecondary)
        .frame(maxWidth: .infinity, alignment: .trailing)
        .padding(.horizontal, isMediaOnly ? 6 : 0)
        .padding(.bottom, isMediaOnly ? 2 : 0)
    }
}

/// 状态图标：发送中=时钟，已发送=单勾，已送达=双勾，已读=实心双勾，失败=感叹号
struct StatusIcon: View {
    let status: MessageStatus
    var body: some View {
        Group {
            switch status {
            case .sending: Image(systemName: "clock").accessibilityIdentifier("status_sending")
            case .sent: Image(systemName: "checkmark").accessibilityIdentifier("status_sent")
            case .delivered: DoubleCheck(filled: false).accessibilityIdentifier("status_delivered")
            case .read: DoubleCheck(filled: true).accessibilityIdentifier("status_read")
            case .failed: Image(systemName: "exclamationmark.circle").accessibilityIdentifier("status_failed")
            }
        }
        .font(.system(size: 10, weight: .bold))
    }
}

struct DoubleCheck: View {
    let filled: Bool
    var body: some View {
        HStack(spacing: -5) {
            Image(systemName: filled ? "checkmark.circle.fill" : "checkmark.circle")
            Image(systemName: filled ? "checkmark.circle.fill" : "checkmark.circle")
        }
        .accessibilityElement()
        .accessibilityLabel(filled ? "已读" : "已送达")
    }
}

struct QuoteBlock: View {
    let quote: Quote
    let outgoing: Bool
    var core = HushCore.shared

    var body: some View {
        HStack(spacing: 6) {
            Rectangle().fill(outgoing ? Color.white : Theme.accent).frame(width: 3)
            VStack(alignment: .leading, spacing: 2) {
                Text(quote.author == core.myId ? "你" : core.contacts.displayName(quote.author))
                    .font(Fonts.sized(12, weight: .semibold))
                Text(quote.text).font(Fonts.sized(13)).lineLimit(2)
            }
            .foregroundStyle(outgoing ? Theme.onAccent.opacity(0.9) : Theme.textPrimary.opacity(0.8))
            Spacer(minLength: 0)
        }
        .padding(6)
        .background(RoundedRectangle(cornerRadius: 8).fill(outgoing ? Color.white.opacity(0.18) : Color.black.opacity(0.06)))
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("quoteBlock")
    }
}

struct ReactionChips: View {
    let reactions: [ReactionGroup]
    var body: some View {
        HStack(spacing: 4) {
            ForEach(reactions, id: \.emoji) { r in
                Text(r.userIds.count > 1 ? "\(r.emoji) \(r.userIds.count)" : r.emoji)
                    .font(Fonts.sized(13))
                    .padding(.horizontal, 7).padding(.vertical, 2)
                    .background(Capsule().fill(Theme.surface))
                    .overlay(Capsule().stroke(Theme.divider, lineWidth: 1))
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityIdentifier("reactions")
    }
}

struct DaySeparator: View {
    let ts: Int64
    var body: some View {
        Text(Fmt.daySeparator(ts))
            .font(Fonts.timestamp)
            .foregroundStyle(Theme.textSecondary)
            .padding(.horizontal, 10).padding(.vertical, 3)
            .background(Capsule().fill(Theme.surface))
            .frame(maxWidth: .infinity)
            .padding(.vertical, 8)
    }
}
