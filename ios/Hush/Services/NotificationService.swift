import Foundation
import UserNotifications
import UIKit

/// 本地通知：不在对应会话（或 App 不在前台）时横幅提示；点击进入会话。
@MainActor
final class NotificationService: NSObject, UNUserNotificationCenterDelegate {
    static let shared = NotificationService()
    private var throttle = NotificationThrottle()

    func setup() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.requestAuthorization(options: [.alert, .sound, .badge]) { _, _ in }
    }

    func notifyIfNeeded(convId: String, message: MessageItem, backlog: Bool, mentionsMe: Bool = false) {
        let core = HushCore.shared
        guard NotificationThrottle.shouldBanner(
            backlog: backlog, notificationsOn: core.settings.notifications,
            viewingConversation: core.appActive && core.activeConvId == convId,
            muted: core.chats.summary(convId)?.muted ?? false, body: message.body, mentionsMe: mentionsMe
        ) else { return }
        let content = UNMutableNotificationContent()
        content.title = "Hush"
        content.body = NotificationThrottle.bannerText(title: core.chats.title(convId), body: message.body, mentionsMe: mentionsMe)
        // 同一会话 3 秒内只响一次
        content.sound = throttle.allowSound(convId: convId) ? .default : nil
        content.threadIdentifier = convId
        content.userInfo = ["convId": convId]
        // 按会话合并：同一会话的新通知替换旧的，而不是堆出 N 条
        let req = UNNotificationRequest(identifier: NotificationThrottle.identifier(convId: convId), content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req)
    }

    func reset() { throttle = NotificationThrottle() }

    /// 后台来电通知。B8：标识符由 callId 决定，通话结束（对方取消、超时、已在别处处理）时可以撤销
    func notifyIncomingCall(callId: String, from name: String, video: Bool) {
        guard !HushCore.shared.appActive else { return }
        let content = UNMutableNotificationContent()
        content.title = "Hush"
        content.body = "\(name) 邀请你\(video ? "视频" : "语音")通话"
        content.sound = .default
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: NotificationThrottle.callIdentifier(callId), content: content, trigger: nil))
    }

    /// 来电已结束：撤销还挂在通知中心里的「邀请你通话」
    func withdrawIncomingCall(callId: String) {
        guard !callId.isEmpty else { return }
        let ids = [NotificationThrottle.callIdentifier(callId)]
        let center = UNUserNotificationCenter.current()
        center.removePendingNotificationRequests(withIdentifiers: ids)
        center.removeDeliveredNotifications(withIdentifiers: ids)
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                            withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound, .list])
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping () -> Void) {
        let convId = response.notification.request.content.userInfo["convId"] as? String
        DispatchQueue.main.async {
            MainActor.assumeIsolated {
                if let convId { Router.shared.openChat(convId) }
            }
        }
        completionHandler()
    }
}

/// 通知合并与限流（纯逻辑，便于测试）
struct NotificationThrottle {
    var window: TimeInterval = 3
    private var lastSound: [String: Date] = [:]

    static func identifier(convId: String) -> String { "msg-\(convId)" }
    static func callIdentifier(_ callId: String) -> String { "call-\(callId)" }

    /// 横幅文案。隐私：只提示有新消息，不展示正文；未接来电单独说明
    static func bannerText(title: String, body: MessageBody, mentionsMe: Bool) -> String {
        if case .callLog = body { return title.isEmpty ? "你有未接来电" : "\(title) 的未接来电" }
        if mentionsMe { return title.isEmpty ? "有人@了你" : "\(title) 中有人@了你" }
        return title.isEmpty ? "你有新消息" : "来自 \(title) 的新消息"
    }

    /// 是否为这条收到的消息弹横幅：积压（连接建立后 queue_empty 之前的推送、HTTP 补拉）不弹，
    /// 会话列表已有未读数；正在看的会话、免打扰会话（除非这条消息 @ 了我）、系统提示、非未接的通话记录也不弹
    static func shouldBanner(backlog: Bool, notificationsOn: Bool, viewingConversation: Bool,
                             muted: Bool, body: MessageBody, mentionsMe: Bool = false) -> Bool {
        guard notificationsOn, !backlog, !viewingConversation, !muted || mentionsMe else { return false }
        switch body {
        case .system: return false
        // A10：通话记录只在「未接」时提示；自己拒接、已接通、对方忙线等不弹「新消息」
        case let .callLog(_, _, _, outgoing, reason): return !outgoing && reason == .missed
        default: return true
        }
    }

    mutating func allowSound(convId: String, now: Date = Date()) -> Bool {
        if let last = lastSound[convId], now.timeIntervalSince(last) < window { return false }
        lastSound[convId] = now
        return true
    }
}

enum UNBadge {
    static func set(_ n: Int) {
        UNUserNotificationCenter.current().setBadgeCount(n) { _ in }
    }
}
