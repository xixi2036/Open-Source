import XCTest

/// 跨平台联调：iOS（Bob）与 Android（Alice）互通。需要 Android 端配合操作，单独按用例运行。
final class A09CrossPlatformTests: XCTestCase {
    override func setUpWithError() throws {
        continueAfterFailure = false
        // 需要 Android 端配合操作，只在显式设置 HUSH_CROSS 时运行
        guard ProcessInfo.processInfo.environment["HUSH_CROSS"] != nil else { throw XCTSkip("跨平台用例需以 TEST_RUNNER_HUSH_CROSS=1 运行，并由 Android 端配合") }
    }

    /// 接受 Alice（Android）发来的好友申请
    func test01AcceptAlice() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["通讯录"], 15).tap()
        if !a.buttons["friend_Alice"].waitForExistence(timeout: 3) {
            waitFor(a.buttons["friendRequestsEntry"]).tap()
            waitFor(a.buttons["accept_Alice"], 20).tap()
            sleep(2)
            shot("x01-accept-alice")
            a.navigationBars.buttons.element(boundBy: 0).tap()
        }
        waitFor(a.buttons["friend_Alice"], 20)
        shot("x02-contacts-with-alice")
    }

    /// 收到 Alice 的消息后回复
    func test02ChatWithAlice() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, "Alice")
        waitFor(a.staticTexts["Hello from Android"], 60)
        sendMessage(a, "你好 Alice，这条来自 iOS 🍎")
        sleep(6)
        shot("x03-chat-with-android")
    }

    /// 接听 Alice 从 Android 发起的通话，保持 20 秒后截图并挂断
    func test03AnswerCallFromAndroid() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        let accept = waitFor(a.buttons["acceptCall"], 90)
        shot("x04-incoming-from-android")
        accept.tap()
        allowSystemAlerts()
        waitActive(a)
        sleep(15)
        shot("x05-in-call-with-android")
        waitFor(a.buttons["hangupCall"]).tap()
        sleep(3)
        shot("x06-after-call")
    }

    /// 从 iOS 向 Alice 发起视频通话，等待接通 20 秒后挂断
    func test04VideoCallToAndroid() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, "Alice")
        waitFor(a.buttons["videoCallButton"]).tap()
        allowSystemAlerts()
        waitActive(a, 90)
        sleep(15)
        shot("x07-video-call-with-android")
        waitFor(a.buttons["hangupCall"]).tap()
        sleep(3)
    }

    /// 等待通话进入「通话中」（状态文字变为计时，如 0:05）
    func waitActive(_ a: XCUIApplication, _ timeout: TimeInterval = 40) {
        let timer = a.staticTexts.matching(NSPredicate(format: "identifier == 'callStatus' AND label MATCHES '^[0-9]+:[0-9]{2}$'")).firstMatch
        if !timer.waitForExistence(timeout: timeout) {
            dump("call-not-active", a)
            XCTFail("通话未接通：\(a.staticTexts["callStatus"].label)")
        }
    }
}
