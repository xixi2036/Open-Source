import XCTest
@testable import Hush

final class NotificationThrottleTests: XCTestCase {
    func testSoundAtMostOncePerConversationWindow() {
        var t = NotificationThrottle()
        let now = Date()
        XCTAssertTrue(t.allowSound(convId: "u:a", now: now))
        XCTAssertFalse(t.allowSound(convId: "u:a", now: now.addingTimeInterval(0.5)), "积压的 N 条消息不能响 N 次")
        XCTAssertTrue(t.allowSound(convId: "u:b", now: now.addingTimeInterval(0.5)), "不同会话互不影响")
        XCTAssertTrue(t.allowSound(convId: "u:a", now: now.addingTimeInterval(3.5)))
    }

    func testIdentifierIsPerConversationSoBannersReplaceEachOther() {
        XCTAssertEqual(NotificationThrottle.identifier(convId: "u:a"), NotificationThrottle.identifier(convId: "u:a"))
        XCTAssertNotEqual(NotificationThrottle.identifier(convId: "u:a"), NotificationThrottle.identifier(convId: "g:a"))
    }

    /// P-F：只有实时推送（backlog == false）的消息弹横幅，与本机时钟无关
    func testBannerOnlyForLiveMessages() {
        let text = MessageBody.text(text: "hi")
        func banner(backlog: Bool = false, on: Bool = true, viewing: Bool = false, muted: Bool = false,
                    body: MessageBody = text) -> Bool {
            NotificationThrottle.shouldBanner(backlog: backlog, notificationsOn: on, viewingConversation: viewing,
                                              muted: muted, body: body)
        }
        XCTAssertTrue(banner())
        XCTAssertFalse(banner(backlog: true), "连接建立后的积压 / HTTP 补拉不弹横幅")
        XCTAssertFalse(banner(on: false))
        XCTAssertFalse(banner(viewing: true), "正在看的会话不弹")
        XCTAssertFalse(banner(muted: true))
        XCTAssertFalse(banner(body: .system(text: "安全码已变更")))
        XCTAssertTrue(banner(body: .undecryptable))
    }

    /// U4：免打扰群里 @ 我的消息仍弹通知；其余限制不变
    func testMentionBypassesMuteOnly() {
        func banner(backlog: Bool = false, viewing: Bool = false, muted: Bool, mentionsMe: Bool) -> Bool {
            NotificationThrottle.shouldBanner(backlog: backlog, notificationsOn: true, viewingConversation: viewing,
                                              muted: muted, body: .text(text: "@我 hi"), mentionsMe: mentionsMe)
        }
        XCTAssertFalse(banner(muted: true, mentionsMe: false))
        XCTAssertTrue(banner(muted: true, mentionsMe: true))
        XCTAssertFalse(banner(backlog: true, muted: true, mentionsMe: true), "积压消息仍不弹")
        XCTAssertFalse(banner(viewing: true, muted: true, mentionsMe: true), "正在看的会话仍不弹")
    }
}
