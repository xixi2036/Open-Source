import Foundation

enum MessagePreview {
    /// 会话列表第二行：红色前缀（「[有人@我]」「[草稿]」）+ 正文。
    /// 优先级：正在输入 > 有人@我（未读提及，后接最后一条消息）> 草稿 > 最后一条消息。
    struct ListLine: Equatable {
        var prefix: String?
        var body: String
    }

    static func listLine(typing: Bool, unreadMentions: UInt32, draft: String?, message: String) -> ListLine {
        if typing { return ListLine(prefix: nil, body: "正在输入…") }
        if unreadMentions > 0 { return ListLine(prefix: "[有人@我]", body: message) }
        if let d = draft?.trimmingCharacters(in: .whitespacesAndNewlines), !d.isEmpty {
            return ListLine(prefix: "[草稿]", body: d.replacingOccurrences(of: "\n", with: " "))
        }
        return ListLine(prefix: nil, body: message)
    }

    /// 会话列表 / 引用 / 通知里使用的单行预览
    @MainActor
    static func text(_ m: MessageItem, inGroup: Bool = false) -> String {
        let core = HushCore.shared
        if m.deleted { return recallText(m, inGroup: inGroup) }
        let body: String
        switch m.body {
        case let .text(t): body = t
        case let .attachments(items, caption):
            if let first = items.first {
                switch first.kind {
                case .image: body = "[图片]"
                case .voice: body = "[语音]"
                case .video: body = "[视频]"
                case .file: body = "[文件] \(first.fileName)"
                }
            } else {
                body = caption ?? ""
            }
        case let .system(t): return t
        case .undecryptable: return "⚠️ 无法解密的消息"
        case let .callLog(video, secs, _, _, reason):
            return "[\(callLogPreview(video: video, secs: secs, reason: reason))]"
        }
        if inGroup && !m.isOutgoing && !m.senderId.isEmpty {
            return "\(core.contacts.displayName(m.senderId))：\(body)"
        }
        return body
    }

    /// 撤回提示（会话列表与聊天页共用）
    @MainActor
    static func recallText(_ m: MessageItem, inGroup: Bool) -> String {
        if m.isOutgoing { return "你撤回了一条消息" }
        return inGroup ? "\(HushCore.shared.contacts.displayName(m.senderId)) 撤回了一条消息" : "对方撤回了一条消息"
    }

    /// 通话记录文案（聊天页，旁边有电话 / 摄像头图标）：按核心给出的结束原因显示
    static func callLogText(video: Bool, secs: UInt32, reason: CallEndReason) -> String {
        switch reason {
        case .completed: return "\(video ? "视频通话" : "语音通话") \(Fmt.duration(Int(secs)))"
        case .missed: return "未接来电"
        case .rejected: return "已拒绝"
        case .busy: return "对方忙线"
        case .cancelled: return "已取消"
        case .failed: return "通话失败"
        }
    }

    /// 会话列表 / 搜索里的单行预览：没有图标，「已拒绝」「已取消」这类短语前补上通话类型
    static func callLogPreview(video: Bool, secs: UInt32, reason: CallEndReason) -> String {
        let text = callLogText(video: video, secs: secs, reason: reason)
        switch reason {
        case .completed, .missed: return text
        default: return "\(video ? "视频通话" : "语音通话") \(text)"
        }
    }
}
