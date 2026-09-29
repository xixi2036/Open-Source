import XCTest

final class A08NotificationTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    /// 不在对应会话时收到消息：系统横幅“来自 xx 的新消息”，点击进入会话。
    /// 做法：删除好友小美后重新添加，机器人同意后会主动发招呼消息（此时停留在资料页）。
    func test11NotificationBanner() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.bot2Name)
        a.buttons["chatMoreButton"].firstMatch.tap()
        waitFor(a.buttons["safetyNumberEntry"])
        a.swipeUp()
        waitFor(a.buttons["删除好友"].firstMatch).tap()
        waitFor(a.buttons["删除好友"].firstMatch).tap()
        sleep(3)
        a.tabBars.buttons["通讯录"].tap()
        waitFor(a.buttons["addFriendButton"]).tap()
        let f = waitFor(a.textFields["searchEmailField"])
        f.tap(); f.typeText(UITestConfig.bot2Email)
        a.buttons["searchEmailButton"].tap()
        waitFor(a.buttons["searchResult"], 10).tap()
        waitFor(a.buttons["addToContacts"], 10).tap()
        waitFor(a.alerts.firstMatch)
        a.alerts.buttons["发送"].firstMatch.tap()
        let sb = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let banner = sb.descendants(matching: .any).matching(NSPredicate(format: "label CONTAINS %@", "的新消息")).firstMatch
        waitFor(banner, 20)
        shotNow("54-notification-banner")
        banner.tap()
        let title = waitFor(a.staticTexts["chatTitle"], 10)
        XCTAssertEqual(title.label, UITestConfig.bot2Name)
        sleep(1)
        shot("55-notification-opened-chat")
    }
}
