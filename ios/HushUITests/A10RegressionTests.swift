import XCTest

/// 接口 v2 与实测回归：朋友圈通知 → 单条动态详情、积压消息不弹横幅、实时消息弹横幅、分页、搜索、登录过期。
/// 需要测试服务端上有第三个账号（HUSH_PEER_NAME，默认「路人甲」）向本账号发好友申请并评论本账号的动态（由宿主机脚本完成），
/// 用例按编号顺序单独运行。
final class A10RegressionTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    private var peerName: String { UITestConfig.env["HUSH_PEER_NAME"] ?? "路人甲" }
    private var springboard: XCUIApplication { XCUIApplication(bundleIdentifier: "com.apple.springboard") }

    private func banner() -> XCUIElement {
        springboard.descendants(matching: .any).matching(NSPredicate(format: "label CONTAINS %@", "的新消息")).firstMatch
    }

    private func backToRoot(_ a: XCUIApplication) {
        var n = 0
        while a.navigationBars.buttons.element(boundBy: 0).exists && n < 5 {
            let back = a.navigationBars.buttons.element(boundBy: 0)
            // 大标题页的第一个按钮是工具栏按钮，不是返回
            if ["searchButton", "addFriendButton", "composeMoment", "newChatMenu"].contains(back.identifier) { break }
            back.tap(); n += 1
        }
    }

    /// 接受第三个账号的好友申请；资料页显示对方邮箱（P-C）
    func test01AcceptPeerAndEmail() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["通讯录"], 15).tap()
        if !a.buttons["friend_\(peerName)"].waitForExistence(timeout: 3) {
            waitFor(a.buttons["friendRequestsEntry"]).tap()
            waitFor(a.buttons["accept_\(peerName)"], 15).tap()
            sleep(2)
            a.navigationBars.buttons.element(boundBy: 0).tap()
        }
        waitFor(a.buttons["friend_\(peerName)"], 15).tap()
        waitFor(a.staticTexts["profileEmail"], 10)
        shot("60-profile-email")
        backToRoot(a)
        // 单聊设置页的邮箱
        a.tabBars.buttons["通讯录"].tap()
        waitFor(a.buttons["friend_\(UITestConfig.botName)"]).tap()
        waitFor(a.buttons["profileSendMessage"]).tap()
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        waitFor(a.staticTexts["peerEmail"], 10)
        shot("61-direct-settings-email")
    }

    /// 朋友圈通知 → 单条动态详情（P-D）；删除后再点通知提示「该动态已不可见」
    func test02MomentNotificationDetail() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["朋友圈"], 15).tap()
        let bar = a.buttons["newMomentNotes"]
        let history = a.buttons["momentNotesHistory"]
        if bar.waitForExistence(timeout: 10) { bar.tap() } else { waitFor(history, 5).tap() }
        let note = waitFor(a.buttons["momentNote_\(peerName)"].firstMatch, 10)
        shot("62-moment-notifications")
        note.tap()
        waitFor(a.navigationBars["详情"], 10)
        let comment = a.staticTexts.containing(NSPredicate(format: "label CONTAINS %@", "路过点个评论")).firstMatch
        waitFor(comment, 10)
        shot("63-moment-detail")
        // 自己的动态：在详情页删除，返回通知列表
        waitFor(a.buttons["deleteMoment"]).tap()
        waitFor(a.buttons["删除"].firstMatch).tap()
        waitFor(a.buttons["momentNote_\(peerName)"].firstMatch, 10).tap()
        waitFor(a.descendants(matching: .any)["momentUnavailable"], 10)
        XCTAssertTrue(a.staticTexts["该动态已不可见"].exists)
        shot("64-moment-unavailable")
    }

    /// 杀进程期间到达的消息：重启后作为积压同步，不弹横幅，只计未读（P-F / 杀进程重启）
    func test03BacklogAfterRelaunchHasNoBanner() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        let input = waitFor(a.textFields["messageInput"].exists ? a.textFields["messageInput"] : a.textViews["messageInput"])
        input.tap()
        input.typeText("离线期间的回复5")
        // 宿主机事先暂停机器人进程（kill -STOP），看到 app-killed 标记后再恢复，保证回复在 App 不在线时到达
        a.buttons["sendButton"].tap()
        sleep(1)
        a.terminate()
        mark("app-killed")
        sleep(8)
        a.launch()
        waitFor(a.tabBars.buttons["聊天"], 15)
        a.tabBars.buttons["通讯录"].tap()
        XCTAssertFalse(banner().waitForExistence(timeout: 8), "积压消息不应弹横幅")
        a.tabBars.buttons["聊天"].tap()
        sleep(1)
        shot("65-backlog-unread-no-banner")
        openConversation(a, UITestConfig.botName)
        waitFor(a.staticTexts["收到：离线期间的回复5"], 10)
    }

    /// 不在对应会话时的实时消息弹横幅，点击进入会话（P-F）
    func test04LiveMessageBannerOpensChat() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.botName)
        // 机器人建新群并在群里发欢迎消息：当前不在该群聊中，应弹横幅
        sendMessage(a, "群")
        let b = banner()
        waitFor(b, 20)
        shotNow("66-live-banner")
        b.tap()
        let title = waitFor(a.staticTexts["chatTitle"], 10)
        XCTAssertTrue(title.label.hasPrefix("机器人测试群"), title.label)
        sleep(1)
        shot("67-banner-opened-group")
    }

    /// 超过一页（40 条）的消息：上翻加载更早的记录，不重复、不遗漏（P-A）
    func test05Pagination() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.bot2Name)
        let first = a.staticTexts["分页 1"]
        if !first.exists {
            for i in 1...22 {
                sendMessage(a, "分页 \(i)")
                usleep(300_000)
            }
            waitFor(a.staticTexts["收到：分页 22"], 20)
        }
        backToRoot(a)
        openConversation(a, UITestConfig.bot2Name)
        sleep(2)
        var n = 0
        while !a.staticTexts["分页 1"].exists && n < 25 {
            a.swipeDown(velocity: .fast)
            n += 1
        }
        waitFor(a.staticTexts["分页 1"], 5)
        XCTAssertEqual(a.staticTexts.matching(identifier: "messageText").matching(NSPredicate(format: "label == %@", "分页 1")).count, 1)
        XCTAssertEqual(a.staticTexts.matching(NSPredicate(format: "label == %@", "收到：分页 1")).count, 1)
        shot("68-pagination-top")
    }

    /// 消息搜索
    func test06Search() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        waitFor(a.buttons["searchButton"]).tap()
        let field = waitFor(a.searchFields.firstMatch)
        field.typeText("引用回复")
        waitFor(a.staticTexts["消息"].firstMatch, 15)
        shot("69-search")
    }

    /// 登录过期（P-G）：以 HUSH_SERVER 指向 --token-ttl 很短的服务端运行。
    /// 已登录则等待令牌过期，下拉刷新触发 401，应回到欢迎页并提示「登录已过期，请重新登录」。
    func test07SessionExpired() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["通讯录"], 15).tap()
        let wait = Double(UITestConfig.env["HUSH_EXPIRE_WAIT"] ?? "70") ?? 70
        sleep(UInt32(wait))
        // 回前台会补拉（HTTP），同样会触发 401
        XCUIDevice.shared.press(.home)
        sleep(2)
        a.activate()
        let tip = a.staticTexts["登录已过期，请重新登录"]
        if !tip.waitForExistence(timeout: 10) {
            a.tabBars.buttons["通讯录"].firstMatch.tap()
            a.swipeDown()
        }
        waitFor(tip, 15)
        waitFor(a.buttons["startButton"])
        shot("70-session-expired")
    }

    /// 深色模式补充页面（宿主机先执行 `xcrun simctl ui <id> appearance dark`）
    func test08DarkModeMorePages() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["通讯录"], 15).tap()
        sleep(1)
        shot("71-dark-contacts")
        waitFor(a.buttons["friend_\(UITestConfig.botName)"]).tap()
        waitFor(a.buttons["profileSendMessage"]).tap()
        sleep(1)
        shot("72-dark-chat-call-logs")
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        waitFor(a.buttons["safetyNumberEntry"])
        shot("73-dark-direct-settings")
        a.buttons["safetyNumberEntry"].tap()
        waitFor(a.descendants(matching: .any)["safetyDigits"], 10)
        shot("74-dark-safety-number")
        backToRoot(a)
        a.tabBars.buttons["通讯录"].tap()
        backToRoot(a)
        waitFor(a.buttons["groupsEntry"]).tap()
        waitFor(a.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "group_")).firstMatch).tap()
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        sleep(2)
        shot("75-dark-group-settings")
        backToRoot(a)
        a.tabBars.buttons["朋友圈"].tap()
        let notes = a.buttons["momentNotesHistory"].exists ? a.buttons["momentNotesHistory"] : a.buttons["newMomentNotes"]
        if notes.waitForExistence(timeout: 5) {
            notes.tap()
            sleep(1)
            shot("76-dark-moment-notifications")
            let row = a.buttons.matching(NSPredicate(format: "identifier BEGINSWITH %@", "momentNote_")).element(boundBy: 1)
            if row.exists { row.tap(); sleep(2); shot("77-dark-moment-detail") }
        }
        backToRoot(a)
        a.tabBars.buttons["我"].tap()
        sleep(1)
        shot("78-dark-me")
    }

    private func lastBubble(_ a: XCUIApplication, _ id: String) -> XCUIElement {
        let q = a.otherElements.matching(identifier: id)
        return q.element(boundBy: max(q.count - 1, 0))
    }

    /// 长按菜单：复制 / 撤回 / 删除
    func test09MessageActions() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.bot2Name)
        sendMessage(a, "这条要撤回")
        waitFor(a.staticTexts["收到：这条要撤回"], 15)
        let mine = a.otherElements.matching(identifier: "bubble_out").containing(.staticText, identifier: "messageText")
            .matching(NSPredicate(format: "label CONTAINS %@", "这条要撤回")).firstMatch
        let target = mine.exists ? mine : lastBubble(a, "bubble_out")
        target.press(forDuration: 0.8)
        waitFor(a.buttons["action_copy"]).tap()
        waitFor(a.staticTexts["已复制"], 5)
        target.press(forDuration: 0.8)
        shot("79-message-actions")
        waitFor(a.buttons["action_recall"]).tap()
        waitFor(a.staticTexts["你撤回了一条消息"], 10)
        // 删除（仅本机）对方的回复
        let reply = a.staticTexts["收到：这条要撤回"]
        reply.press(forDuration: 0.8)
        waitFor(a.buttons["action_delete"]).tap()
        let gone = NSPredicate(format: "exists == false")
        expectation(for: gone, evaluatedWith: reply)
        waitForExpectations(timeout: 5)
        shot("80-recalled-and-deleted")
    }

    /// 新建群聊并管理：改群名、发公告、移除 / 添加成员、设管理员、退群
    func test10GroupAdmin() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        let existing = a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_回归测试群")).firstMatch
        if existing.waitForExistence(timeout: 3) {
            existing.tap()
        } else {
            waitFor(a.buttons["newChatMenu"]).tap()
            waitFor(a.buttons["newGroupChat"]).tap()
            let name = waitFor(a.textFields["groupNameField"])
            name.tap(); name.typeText("回归测试群")
            a.buttons["select_\(UITestConfig.botName)"].tap()
            a.buttons["select_\(UITestConfig.bot2Name)"].tap()
            a.buttons["createGroupButton"].tap()
        }
        waitFor(a.staticTexts["chatTitle"], 15)
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        waitFor(a.buttons["editGroupName"]).tap()
        let field = waitFor(a.alerts.textFields.firstMatch)
        field.clearAndType("回归测试群改名")
        a.alerts.buttons["保存"].tap()
        waitFor(a.staticTexts["回归测试群改名"], 10)
        a.buttons["groupNotice"].tap()
        let notice = waitFor(a.alerts.textFields.firstMatch)
        notice.clearAndType("这是群公告")
        a.alerts.buttons["发布"].tap()
        waitFor(a.buttons.matching(NSPredicate(format: "identifier == %@ AND label == %@", "groupNotice", "这是群公告")).firstMatch, 10)
        // 移除测试小美
        waitFor(a.buttons["removeMode"]).tap()
        waitFor(a.buttons["member_\(UITestConfig.bot2Name)"]).tap()
        let removed = NSPredicate(format: "exists == false")
        expectation(for: removed, evaluatedWith: a.buttons["member_\(UITestConfig.bot2Name)"])
        waitForExpectations(timeout: 10)
        a.buttons["removeMode"].tap()
        // 再加回来
        a.buttons["addMember"].tap()
        waitFor(a.buttons["select_\(UITestConfig.bot2Name)"]).tap()
        a.buttons.matching(NSPredicate(format: "label BEGINSWITH %@", "添加")).firstMatch.tap()
        waitFor(a.buttons["member_\(UITestConfig.bot2Name)"], 10)
        // 设为管理员
        a.buttons["member_\(UITestConfig.botName)"].tap()
        waitFor(a.buttons["设为管理员"]).tap()
        waitFor(a.staticTexts["管理员"], 10)
        sleep(1)
        shot("81-group-admin")
        a.navigationBars.buttons.element(boundBy: 0).tap()
        sleep(1)
        shot("82-group-chat-system-messages")
    }

    /// 停在底部时弹出键盘：最后一条消息仍然可见（不被键盘挡住）
    func test11KeyboardKeepsLatestVisible() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        openConversation(a, UITestConfig.bot2Name)
        sleep(2)
        let texts = a.staticTexts.matching(identifier: "messageText")
        let last = texts.element(boundBy: texts.count - 1)
        let label = last.label
        XCTAssertTrue(last.isHittable, "进入会话时最后一条可见")
        let input = waitFor(a.textFields["messageInput"].exists ? a.textFields["messageInput"] : a.textViews["messageInput"])
        input.tap()
        sleep(2)
        shot("83-keyboard-latest-visible")
        let after = a.staticTexts.matching(NSPredicate(format: "identifier == 'messageText' AND label == %@", label)).firstMatch
        XCTAssertTrue(after.isHittable, "弹出键盘后最后一条消息被挡住：\(label)")
    }

    /// 设置页退出登录（二次确认），回到欢迎页
    func test12Logout() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["我"], 15).tap()
        waitFor(a.buttons["settingsEntry"]).tap()
        let logout = a.buttons["logoutButton"]
        var n = 0
        let tabTop = a.tabBars.firstMatch.frame.minY
        while n < 5 && !(logout.exists && logout.frame.maxY < tabTop) { a.swipeUp(); n += 1 }
        shot("84a-settings-scrolled-bottom")
        waitFor(logout).coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        let confirm = a.buttons.matching(NSPredicate(format: "label == %@ AND identifier != %@", "退出登录", "logoutButton")).firstMatch
        waitFor(confirm).tap()
        waitFor(a.buttons["startButton"], 15)
        shot("84b-logged-out")
    }

    /// 库密钥不匹配（P-E）：宿主机先 `xcrun simctl keychain <id> reset` 丢掉钥匙串里的密钥，App 生成新密钥后打不开旧库。
    /// 应显示专门的说明，只提供「重置本地数据并重新登录」，不显示无用的「重试」。
    func test13KeyMismatch() {
        let a = launchApp()
        let reset = waitFor(a.buttons["resetLocalData"], 15)
        XCTAssertFalse(a.buttons["retryBootstrap"].exists, "密钥不匹配时重试没有意义")
        shot("85-key-mismatch")
        reset.tap()
        waitFor(a.buttons["删除并重新登录"].firstMatch).tap()
        waitFor(a.buttons["startButton"], 15)
    }
}
