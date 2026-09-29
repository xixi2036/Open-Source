import XCTest

final class A05CallTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    func test08Calls() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        // 机器人发来语音来电
        sendMessage(a, "打电话")
        let accept = waitFor(a.buttons["acceptCall"], 20)
        sleep(1)
        shot("34-incoming-call")
        XCTAssertTrue(a.buttons["rejectCall"].exists)
        a.buttons["rejectCall"].tap()
        sleep(3)
        _ = accept
        // 自己发起语音通话：机器人回 busy
        waitFor(a.buttons["voiceCallButton"].firstMatch).tap()
        // 机器人会立即回 busy，呼叫页通常一闪而过：立即截图（可能是“正在呼叫…”或“对方忙线中”）
        shotNow("35-outgoing-call")
        sleep(4)
        // 视频来电
        sendMessage(a, "视频")
        waitFor(a.buttons["acceptCall"], 20)
        sleep(1)
        shot("37-incoming-video-call")
        a.buttons["rejectCall"].tap()
        sleep(4)
        shot("38-call-logs")
    }
}
