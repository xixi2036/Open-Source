import XCTest

final class A02FriendChatTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    func addFriend(_ a: XCUIApplication, email: String, name: String, shotPrefix: String?) {
        a.tabBars.buttons["通讯录"].tap()
        if a.buttons["friend_\(name)"].waitForExistence(timeout: 2) { return }
        waitFor(a.buttons["addFriendButton"]).tap()
        let f = waitFor(a.textFields["searchEmailField"])
        f.tap(); f.typeText(email)
        a.buttons["searchEmailButton"].tap()
        let result = waitFor(a.buttons["searchResult"], 10)
        if let p = shotPrefix { shot("\(p)-add-friend-search") }
        result.tap()
        let add = a.buttons["addToContacts"]
        if add.waitForExistence(timeout: 5) {
            add.tap()
            waitFor(a.alerts.firstMatch)
            if let p = shotPrefix { shot("\(p)-add-friend-request") }
            a.alerts.buttons["发送"].firstMatch.tap()
        }
        sleep(4)
        a.navigationBars.buttons.element(boundBy: 0).tap()
        a.navigationBars.buttons.element(boundBy: 0).tap()
        waitFor(a.buttons["friend_\(name)"], 20)
    }

    func test01AddFriends() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["通讯录"], 15)
        addFriend(a, email: UITestConfig.botEmail, name: UITestConfig.botName, shotPrefix: "07")
        addFriend(a, email: UITestConfig.bot2Email, name: UITestConfig.bot2Name, shotPrefix: nil)
        shot("09-contacts")
        a.buttons["friendRequestsEntry"].tap()
        sleep(1)
        shot("09b-friend-requests")
    }

    func test02TextChat() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        sleep(2)
        shot("10-chat-list")
        openConversation(a, UITestConfig.botName)
        sendMessage(a, "你好，\(UITestConfig.botName)")
        let typing = a.staticTexts["正在输入…"]
        if typing.waitForExistence(timeout: 8) { shot("11-chat-typing") } else { dump("typing", a) }
        waitFor(a.staticTexts["收到：你好，\(UITestConfig.botName)"], 15)
        waitFor(a.descendants(matching: .any)["status_read"].firstMatch, 10)
        sleep(2)
        shot("12-chat-read-reaction-quote")
        // 引用回复：长按对方消息
        let incoming = a.otherElements.matching(identifier: "bubble_in").element(boundBy: a.otherElements.matching(identifier: "bubble_in").count - 1)
        incoming.press(forDuration: 0.8)
        waitFor(a.buttons["action_reply"]).tap()
        sendMessage(a, "这是一条引用回复")
        sleep(3)
        // 自己给对方消息点回应
        incoming.press(forDuration: 0.8)
        waitFor(a.buttons["react_❤️"]).tap()
        sleep(3)
        shot("13-chat-quote-reply")
    }
}

final class A02bTypingTests: XCTestCase {
    func test02bTyping() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        let input = waitFor(a.textFields["messageInput"].exists ? a.textFields["messageInput"] : a.textViews["messageInput"])
        input.tap()
        input.typeText("看看正在输入")
        // 宿主机脚本监听此标记后连拍，捕捉短暂的“正在输入…”
        mark("typing-start")
        a.buttons["sendButton"].tap()
        waitFor(a.staticTexts["收到：看看正在输入"], 15)
    }
}
