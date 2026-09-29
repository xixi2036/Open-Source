import XCTest

/// 运行前由宿主机执行 `xcrun simctl ui <id> appearance dark`
final class A07DarkModeTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    func test10DarkMode() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        sleep(2)
        shot("49-dark-chat-list")
        openConversation(a, UITestConfig.botName)
        sleep(2)
        shot("50-dark-chat")
        a.navigationBars.buttons.element(boundBy: 0).tap()
        let g = a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_机器人测试群")).firstMatch
        waitFor(g).tap()
        sleep(2)
        shot("51-dark-group-chat")
        a.navigationBars.buttons.element(boundBy: 0).tap()
        a.tabBars.buttons["朋友圈"].tap()
        sleep(3)
        shot("52-dark-moments")
        a.tabBars.buttons["我"].tap()
        a.buttons["settingsEntry"].tap()
        sleep(1)
        shot("53-dark-settings")
    }
}
