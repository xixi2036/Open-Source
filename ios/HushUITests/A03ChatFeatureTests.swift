import XCTest

final class A03ChatFeatureTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    private func count(_ a: XCUIApplication, _ id: String) -> Int {
        a.descendants(matching: .any).matching(identifier: id).count
    }

    func test03Images() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        let before = count(a, "imageMessage")
        waitFor(a.buttons["attachButton"]).tap()
        waitFor(a.buttons["相册"].firstMatch).tap()
        waitFor(a.images.matching(identifier: "PXGGridLayout-Info").element(boundBy: 1), 10).tap()
        a.buttons["完成"].firstMatch.tap()
        sleep(5)
        XCTAssertGreaterThan(count(a, "imageMessage"), before)
        shot("14-chat-image-sent")
        sendMessage(a, "图片")
        let deadline = Date().addingTimeInterval(20)
        while count(a, "imageMessage") < before + 2 && Date() < deadline { sleep(1) }
        sleep(4)
        shot("15-chat-image-received")
        let imgs = a.descendants(matching: .any).matching(identifier: "imageMessage")
        imgs.element(boundBy: imgs.count - 1).tap()
        let zoom = waitFor(a.images["zoomImage"], 10)
        zoom.pinch(withScale: 2.5, velocity: 2)
        sleep(1)
        shot("16-image-viewer-zoom")
        a.buttons["关闭"].firstMatch.tap()
    }

    func test04Voice() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        let before = count(a, "voiceMessage")
        let mic = waitFor(a.images["micButton"].exists ? a.images["micButton"] : a.buttons["micButton"])
        // 宿主机脚本监听此标记，在按住期间截取录音浮层
        mark("rec-start")
        mic.press(forDuration: 3.5)
        sleep(4)
        dump("voice", a)
        XCTAssertGreaterThan(count(a, "voiceMessage"), before)
        let v = a.descendants(matching: .any).matching(identifier: "voiceMessage")
        v.element(boundBy: v.count - 1).tap()
        sleep(1)
        shot("18-voice-sent")
    }

    /// 宿主机需先把示例文件放入 App 的 Documents（文件 App 中“我的 iPhone/Hush”）
    func test04bFile() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        waitFor(a.buttons["attachButton"]).tap()
        waitFor(a.buttons["文件"].firstMatch).tap()
        waitFor(a.buttons["浏览"].firstMatch, 10).tap()
        sleep(2)
        let file = a.descendants(matching: .any).matching(NSPredicate(format: "label BEGINSWITH %@", "Hush说明")).firstMatch
        if !file.waitForExistence(timeout: 2) {
            let phone = a.cells["DOC.sidebar.item.我的iPhone"]
            if phone.exists { phone.tap(); sleep(2) }
            let folder = a.cells.matching(NSPredicate(format: "label BEGINSWITH %@", "Hush")).firstMatch
            if !file.waitForExistence(timeout: 2) && folder.exists { folder.tap(); sleep(2) }
        }
        if !file.waitForExistence(timeout: 5) { dump("files-nav", a) }
        waitFor(file, 10).tap()
        let msg = a.descendants(matching: .any).matching(identifier: "fileMessage")
        let deadline = Date().addingTimeInterval(15)
        while msg.count == 0 && Date() < deadline { sleep(1) }
        sleep(4)
        shot("18b-file-sent")
        msg.element(boundBy: msg.count - 1).tap()
        sleep(3)
        shot("18c-file-share")
    }

    func test05RecallDisappearing() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        sendMessage(a, "撤回")
        waitFor(a.staticTexts["这条消息马上会被撤回"], 15)
        shot("19-before-recall")
        waitFor(a.staticTexts["对方撤回了一条消息"], 15)
        shot("20-recalled")
        sendMessage(a, "阅后即焚")
        waitFor(a.staticTexts["这条消息 30 秒后消失 🔥"], 15)
        sleep(1)
        shot("21-disappearing")
        let gone = NSPredicate(format: "exists == false")
        expectation(for: gone, evaluatedWith: a.staticTexts["这条消息 30 秒后消失 🔥"])
        waitForExpectations(timeout: 50)
        shot("22-disappeared")
        // 关闭阅后即焚，并查看会话设置 / 安全码
        a.buttons["chatMoreButton"].firstMatch.tap()
        waitFor(a.buttons["safetyNumberEntry"])
        shot("23-conv-settings")
        a.buttons["disappearingPicker"].firstMatch.tap()
        waitFor(a.buttons["关闭"].firstMatch).tap()
        sleep(2)
        a.buttons["safetyNumberEntry"].tap()
        waitFor(a.descendants(matching: .any)["safetyDigits"], 10)
        shot("24-safety-number")
        let t = a.buttons["toggleVerified"]
        if t.label == "标记为已核验" { t.tap() }
        waitFor(a.descendants(matching: .any)["verifiedBadge"], 5)
        shot("25-safety-verified")
    }

    func test06Group() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        sendMessage(a, "群")
        sleep(6)
        a.navigationBars.buttons.element(boundBy: 0).tap()
        let g = a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_机器人测试群")).firstMatch
        waitFor(g, 15).tap()
        sleep(2)
        sendMessage(a, "机器人你好")
        waitFor(a.staticTexts["收到：机器人你好"], 20)
        sleep(2)
        shot("26-group-chat")
        a.buttons["chatMoreButton"].firstMatch.tap()
        sleep(2)
        shot("27-group-settings")
    }
}
