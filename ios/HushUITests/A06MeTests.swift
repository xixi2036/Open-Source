import XCTest

final class A06MeTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    func test09MeAndSettings() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["我"], 15).tap()
        waitFor(a.buttons["myCard"]).tap()
        let name = waitFor(a.textFields["editNameField"])
        name.tap()
        name.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: 20) + UITestConfig.name)
        let about = a.textFields["editAboutField"].exists ? a.textFields["editAboutField"] : a.textViews["editAboutField"]
        about.tap()
        about.typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: 30) + "你好，我是 \(UITestConfig.name)")
        shot("39-edit-profile")
        a.buttons["saveProfile"].tap()
        waitFor(a.staticTexts["myName"])
        sleep(2)
        shot("40-me")
        a.buttons["settingsEntry"].tap()
        waitFor(a.switches["readReceiptsToggle"].firstMatch)
        shot("41-settings")
        a.swipeUp()
        sleep(1)
        shot("42-settings-bottom")
        a.navigationBars.buttons.element(boundBy: 0).tap()
        a.buttons["meQRButton"].tap()
        waitFor(a.images["qrImage"])
        shot("43-my-qr")
        a.navigationBars.buttons.element(boundBy: 0).tap()

        a.tabBars.buttons["通讯录"].tap()
        waitFor(a.buttons["friend_\(UITestConfig.botName)"]).tap()
        waitFor(a.staticTexts["profileName"])
        sleep(1)
        shot("44-profile")
        a.navigationBars.buttons.element(boundBy: 0).tap()
        a.buttons["addFriendButton"].tap()
        waitFor(a.buttons["scanButton"])
        shot("45-add-friend")
        a.navigationBars.buttons.element(boundBy: 0).tap()

        a.tabBars.buttons["聊天"].tap()
        let row = waitFor(a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_\(UITestConfig.botName)")).firstMatch)
        row.swipeRight()
        sleep(1)
        shot("46-chat-list-swipe-leading")
        a.buttons["置顶"].firstMatch.tap()
        sleep(1)
        row.swipeLeft()
        sleep(1)
        shot("47-chat-list-swipe-trailing")
        row.swipeRight()
        sleep(1)
        shot("48-chat-list")
    }
}
