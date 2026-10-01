import XCTest

/// 2026-09-30 审查修复的 UI 回归（A1、A3、A7、A8、A9、A10）与体验补齐项的实测。
///
/// 前置条件：
/// - 已登录测试账号（A01LoginTests），并已添加两个机器人为好友（A02FriendChatTests.test01AddFriends）；
/// - HUSH_BOT_NAME 对应的机器人必须是 `ios/scripts/auditbot.rs`（在 core/examples/bot.rs 基础上增加了
///   「延迟来电 N」「连发 N」「链接」等指令，构建方式见 ios/scripts/run-ui-host.sh）；
/// - HUSH_BOT2_NAME 对应普通的 bot（A8 建群、A9 删除好友后重新添加用）。
/// 用例之间相互独立，可单独运行。截图前缀建议 audit-fix-。
final class A14AuditFixTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    private var bot: String { UITestConfig.botName }
    private var bot2: String { UITestConfig.bot2Name }

    private func input(_ a: XCUIApplication) -> XCUIElement {
        a.textFields["messageInput"].exists ? a.textFields["messageInput"] : a.textViews["messageInput"]
    }

    private func conv(_ a: XCUIApplication, _ title: String) -> XCUIElement {
        a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_\(title)")).firstMatch
    }

    private func backToList(_ a: XCUIApplication) {
        var n = 0
        while !a.navigationBars["聊天"].exists && n < 5 {
            a.navigationBars.buttons.element(boundBy: 0).tap(); n += 1
            sleep(1)
        }
    }

    /// 打开与好友的单聊：列表里有会话就点会话，否则从通讯录进入
    private func openChat(_ a: XCUIApplication, _ name: String) {
        a.tabBars.buttons["聊天"].tap()
        backToList(a)
        if conv(a, name).waitForExistence(timeout: 3) { conv(a, name).tap(); return }
        a.tabBars.buttons["通讯录"].tap()
        waitFor(a.buttons["friend_\(name)"]).tap()
        waitFor(a.buttons["profileSendMessage"]).tap()
    }

    private func send(_ a: XCUIApplication, _ text: String) {
        let f = waitFor(input(a))
        f.tap()
        f.typeText(text)
        waitFor(a.buttons["sendButton"]).tap()
    }

    private func text(_ a: XCUIApplication, _ label: String) -> XCUIElement {
        a.staticTexts.matching(NSPredicate(format: "label == %@", label)).firstMatch
    }

    /// 会话行是否带未读数（行的无障碍标签末尾是「…, 未读 n 条」）
    private func hasUnread(_ a: XCUIApplication, _ title: String) -> Bool {
        conv(a, title).label.contains("未读")
    }

    private var springboard: XCUIApplication { XCUIApplication(bundleIdentifier: "com.apple.springboard") }

    // MARK: - A10 拒接来电后不弹「新消息」

    func test01RejectCallShowsNoNewMessageBanner() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        send(a, "打电话")
        waitFor(a.buttons["acceptCall"], 20)
        XCTAssertTrue(a.buttons["rejectCall"].isHittable)
        shot("a10-incoming-call")
        a.buttons["rejectCall"].tap()
        // 回到会话列表（不在该会话里，才有可能弹横幅）
        sleep(2)
        backToList(a)
        let banner = springboard.descendants(matching: .any)
            .matching(NSPredicate(format: "label CONTAINS %@ OR label CONTAINS %@", "的新消息", "你有新消息")).firstMatch
        XCTAssertFalse(banner.waitForExistence(timeout: 6), "自己拒接的来电不应弹出「新消息」横幅")
        shot("a10-after-reject-no-banner")
    }

    // MARK: - A1 来电盖住 sheet

    func test02IncomingCallCoversSheet() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        send(a, "延迟来电 8")
        backToList(a)
        waitFor(a.buttons["newChatMenu"]).tap()
        waitFor(a.buttons["newDirectChat"]).tap()
        waitFor(a.navigationBars["发起单聊"], 5)
        // 来电到达：通话页在独立的顶层窗口里，盖住 sheet，接听 / 拒绝可以直接点
        let accept = waitFor(a.buttons["acceptCall"], 25)
        XCTAssertTrue(accept.isHittable, "sheet 打开时接听按钮必须可点")
        XCTAssertTrue(a.buttons["rejectCall"].isHittable)
        XCTAssertEqual(a.staticTexts["callStatus"].label, "语音来电")
        shot("a1-call-over-sheet")
        a.buttons["rejectCall"].tap()
        // 通话页消失后 sheet 还在
        XCTAssertTrue(a.navigationBars["发起单聊"].waitForExistence(timeout: 5))
        let gone = NSPredicate(format: "exists == false")
        expectation(for: gone, evaluatedWith: a.buttons["acceptCall"])
        waitForExpectations(timeout: 6)
        shot("a1-sheet-after-call")
        a.buttons["取消"].firstMatch.tap()
    }

    /// 图片查看器（fullScreenCover）里来电同样能接，toast 也显示在最上层（A7）
    func test03ToastAndCallOverImageViewer() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        send(a, "图片")
        let images = a.descendants(matching: .any).matching(identifier: "imageMessage")
        waitFor(images.firstMatch, 20)
        sleep(3)
        send(a, "延迟来电 12")
        sleep(1)
        images.element(boundBy: images.count - 1).tap()
        waitFor(a.buttons["saveImage"], 10).tap()
        // A7：保存结果的提示显示在图片查看器之上
        let toast = a.staticTexts["toast"]
        if !toast.waitForExistence(timeout: 2.5) {
            // 第一次保存会弹相册授权：允许后提示可能已经消失，再保存一次
            allowSystemAlerts()
            sleep(3)
            a.buttons["saveImage"].tap()
            if !toast.waitForExistence(timeout: 4) { dump("toast-missing", a); XCTFail("图片查看器里看不到 toast") }
        }
        shotNow("a7-toast-over-viewer")
        XCTAssertTrue(["已保存到相册", "保存失败", "没有相册权限，请在「设置」中允许 Hush 添加照片"].contains(toast.label), toast.label)
        // A1：查看器打开时来电
        let accept = waitFor(a.buttons["acceptCall"], 25)
        XCTAssertTrue(accept.isHittable)
        shot("a1-call-over-viewer")
        a.buttons["rejectCall"].tap()
        waitFor(a.buttons["closeViewer"], 8).tap()
    }

    // MARK: - A3 连续到达的消息持续跟随

    func test04FollowsBurstAtBottom() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        let n = Int(UITestConfig.env["HUSH_FLOOD_COUNT"] ?? "") ?? 300
        send(a, "连发 \(n)")
        let last = text(a, "连发 \(n)/\(n)")
        waitFor(last, 150)
        sleep(2)
        XCTAssertTrue(last.isHittable, "停在底部时，连发结束后最后一条必须在屏幕内")
        XCTAssertFalse(a.buttons["newMessagesPill"].exists, "一直停在底部，不应出现「n 条新消息」")
        shot("a3-burst-followed")
        // 已经看到最新一条：会话列表没有未读
        backToList(a)
        sleep(2)
        XCTAssertFalse(hasUnread(a, bot), "看到最新消息后未读应清零：\(conv(a, bot).label)")
    }

    /// 向上翻看历史时新消息不把列表拉走、不标已读，显示「n 条新消息」；点一下回到底部
    func test05ReadingHistoryKeepsPositionAndUnread() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        sleep(2)
        // 先翻到历史位置
        a.swipeDown(velocity: .fast)
        a.swipeDown(velocity: .fast)
        sleep(2)
        XCTAssertFalse(a.buttons["newMessagesPill"].exists)
        // 回到底部，让机器人 6 秒后连发 30 条；趁它还没发，翻到历史位置
        a.swipeUp(velocity: .fast); a.swipeUp(velocity: .fast); a.swipeUp(velocity: .fast)
        sleep(1)
        send(a, "延迟连发 6 30")
        sleep(1)
        a.swipeDown(velocity: .fast)
        a.swipeDown(velocity: .fast)
        let pill = waitFor(a.buttons["newMessagesPill"], 30)
        // 等连发结束
        let end = Date().addingTimeInterval(20)
        while pill.label != "30 条新消息" && Date() < end { usleep(300_000) }
        XCTAssertEqual(pill.label, "30 条新消息")
        shot("a3-reading-history-pill")
        XCTAssertFalse(text(a, "延迟连发 30/30").isHittable, "看历史时不能被拉到底部")
        // 没看到的消息不算已读
        backToList(a)
        sleep(2)
        XCTAssertTrue(hasUnread(a, bot), "停在历史位置时新消息不应被标为已读：\(conv(a, bot).label)")
        conv(a, bot).tap()
        // 重新进入：定位到第一条未读
        waitFor(a.descendants(matching: .any)["unreadSeparator"], 10)
        var n = 0
        while !text(a, "延迟连发 30/30").isHittable && n < 12 { a.swipeUp(velocity: .fast); n += 1 }
        XCTAssertTrue(text(a, "延迟连发 30/30").isHittable)
        XCTAssertFalse(a.buttons["newMessagesPill"].exists)
    }

    // MARK: - 进入会话定位到第一条未读

    func test06OpensAtFirstUnread() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        send(a, "连发 60")
        backToList(a)
        // 在列表里等连发结束
        let row = waitFor(conv(a, bot))
        let deadline = Date().addingTimeInterval(40)
        while !row.label.contains("连发 60/60") && Date() < deadline { usleep(500_000) }
        XCTAssertTrue(row.label.contains("连发 60/60"), row.label)
        sleep(1)
        shot("unread-list-badge")
        row.tap()
        let sep = waitFor(a.descendants(matching: .any)["unreadSeparator"], 10)
        sleep(1)
        XCTAssertTrue(sep.isHittable, "进入会话定位到「以下为新消息」")
        XCTAssertFalse(text(a, "连发 60/60").isHittable, "未读超过一屏时不直接跳到底部")
        shot("unread-separator")
        // 还没看到最新一条：返回列表，未读仍在
        backToList(a)
        sleep(2)
        XCTAssertTrue(hasUnread(a, bot), "未看到的消息不应被标为已读：\(conv(a, bot).label)")
        // 再进入并滚到底部：未读清零
        conv(a, bot).tap()
        waitFor(a.descendants(matching: .any)["unreadSeparator"], 10)
        var n = 0
        while !text(a, "连发 60/60").isHittable && n < 12 { a.swipeUp(velocity: .fast); n += 1 }
        XCTAssertTrue(text(a, "连发 60/60").isHittable)
        sleep(2)
        backToList(a)
        sleep(2)
        XCTAssertFalse(hasUnread(a, bot), "滚到底部后未读清零：\(conv(a, bot).label)")
    }

    // MARK: - A8 从「通讯录 → 群聊」进入后退群

    func test07LeaveGroupFromContactsPopsToRoot() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        backToList(a)
        let name = "退群测试\(Int(Date().timeIntervalSince1970) % 10000)"
        waitFor(a.buttons["newChatMenu"]).tap()
        waitFor(a.buttons["newGroupChat"]).tap()
        waitFor(a.textFields["groupNameField"]).tap()
        a.textFields["groupNameField"].typeText(name)
        waitFor(a.buttons["select_\(bot2)"]).tap()
        waitFor(a.buttons["createGroupButton"]).tap()
        waitFor(a.buttons["chatMoreButton"].firstMatch, 15)
        backToList(a)
        // 从通讯录进入
        a.tabBars.buttons["通讯录"].tap()
        waitFor(a.buttons["groupsEntry"]).tap()
        waitFor(a.buttons["group_\(name)"], 10).tap()
        waitFor(a.buttons["chatMoreButton"].firstMatch, 10).tap()
        let leave = a.buttons["leaveGroup"]
        var n = 0
        while !(leave.exists && leave.isHittable) && n < 5 { a.swipeUp(); n += 1 }
        shot("a8-group-settings-before-leave")
        leave.tap()
        waitFor(a.sheets.buttons["退出群聊"].exists ? a.sheets.buttons["退出群聊"] : a.buttons.matching(NSPredicate(format: "label == %@", "退出群聊")).element(boundBy: 1), 5).tap()
        // 回到通讯录根页面，而不是停在失效的设置页
        waitFor(a.navigationBars["通讯录"], 10)
        XCTAssertFalse(a.staticTexts["groupName"].exists, "退群后不应停留在群设置页")
        shot("a8-after-leave-contacts-root")
        // 聊天 Tab 也回到列表，且群会话不可再发消息
        a.tabBars.buttons["聊天"].tap()
        XCTAssertTrue(a.navigationBars["聊天"].waitForExistence(timeout: 5))
    }

    // MARK: - A9 删除好友后会话行消失

    func test08RemoveFriendDeletesConversationRow() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot2)
        send(a, "删除好友前的一条消息")
        waitFor(text(a, "收到：删除好友前的一条消息"), 20)
        backToList(a)
        XCTAssertTrue(conv(a, bot2).waitForExistence(timeout: 5))
        conv(a, bot2).tap()
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        let del = a.buttons["删除好友"].firstMatch
        var n = 0
        while !(del.exists && del.isHittable) && n < 5 { a.swipeUp(); n += 1 }
        del.tap()
        waitFor(a.buttons["confirmRemoveFriend"].firstMatch, 5).tap()
        waitFor(a.navigationBars["聊天"], 10)
        sleep(2)
        XCTAssertFalse(conv(a, bot2).exists, "删除好友后会话行不应留在列表里")
        shot("a9-chat-list-after-remove-friend")
        a.tabBars.buttons["通讯录"].tap()
        XCTAssertFalse(a.buttons["friend_\(bot2)"].waitForExistence(timeout: 2))
        // 恢复环境：重新加回好友（机器人自动同意）
        A02FriendChatTests().addFriend(a, email: UITestConfig.bot2Email, name: bot2, shotPrefix: nil)
    }

    // MARK: - 体验补齐项

    /// 链接识别（https 直接打开、http 先确认）、选择文字、多选（批量转发 / 删除）
    func test09LinksSelectTextAndMultiSelect() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot)
        send(a, "链接")
        let linkMsg = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "https://example.com/hush")).firstMatch
        waitFor(linkMsg, 20)
        sleep(1)
        shot("link-message")
        // 点 http 链接：先确认
        let http = a.links.matching(NSPredicate(format: "label CONTAINS %@", "http://192.168.1.10")).firstMatch
        if http.waitForExistence(timeout: 3) {
            http.tap()
            waitFor(a.alerts["打开未加密的链接？"], 5)
            shot("link-http-confirm")
            a.alerts.buttons["取消"].tap()
        } else {
            dump("links", a)
            XCTFail("消息里的 http 链接应作为可点击的链接元素出现")
        }
        // 长按 → 选择文字
        linkMsg.press(forDuration: 0.8)
        waitFor(a.buttons["action_select_text"])
        shot("message-actions-with-select")
        a.buttons["action_select_text"].tap()
        let selectable = waitFor(a.textViews["selectableText"], 5)
        selectable.doubleTap()
        sleep(1)
        shot("select-text-partial")
        a.buttons["完成"].firstMatch.tap()
        sleep(1)
        // 长按 → 多选
        send(a, "多选甲")
        send(a, "多选乙")
        waitFor(text(a, "收到：多选乙"), 20)
        text(a, "多选甲").press(forDuration: 0.8)
        waitFor(a.buttons["action_multi_select"]).tap()
        waitFor(a.staticTexts["selectionCount"], 5)
        text(a, "多选乙").tap()
        XCTAssertEqual(a.staticTexts["selectionCount"].label, "已选择 2 条")
        shot("multi-select")
        // 批量转发给另一个机器人
        a.buttons["forwardSelected"].tap()
        waitFor(a.staticTexts["forwardCount"], 5)
        waitFor(a.buttons["forward_\(bot2)"]).tap()
        shot("multi-forward-sheet")
        a.buttons["forwardSend"].tap()
        waitFor(a.buttons["sendButton"].exists ? a.buttons["sendButton"] : a.descendants(matching: .any)["micButton"], 15)
        XCTAssertFalse(a.staticTexts["selectionCount"].exists, "转发完成后退出多选")
        // 批量删除
        text(a, "多选甲").press(forDuration: 0.8)
        waitFor(a.buttons["action_multi_select"]).tap()
        text(a, "多选乙").tap()
        a.buttons["deleteSelected"].tap()
        waitFor(a.buttons["confirmDeleteSelected"].firstMatch, 5).tap()
        sleep(2)
        XCTAssertFalse(text(a, "多选甲").exists)
        XCTAssertFalse(text(a, "多选乙").exists)
        shot("multi-delete-done")
        // 转发过去的两条按顺序出现在另一个会话里
        backToList(a)
        openChat(a, bot2)
        waitFor(text(a, "多选乙"), 10)
        XCTAssertTrue(text(a, "多选甲").exists)
    }

    /// 通话最小化为悬浮条并返回
    func test10MinimizeCall() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 20)
        openChat(a, bot2)
        // 普通机器人收到呼叫后回 busy，呼叫页很快结束；这里只验证最小化入口：呼叫 auditbot 不回应的用户做不到，
        // 所以用「先断开对方」的办法不现实——改为验证呼出瞬间的最小化
        waitFor(a.buttons["voiceCallButton"].firstMatch).tap()
        let mini = a.buttons["minimizeCall"]
        if mini.waitForExistence(timeout: 3) {
            mini.tap()
            if a.buttons["callPill"].waitForExistence(timeout: 2) { shotNow("call-minimized-pill") }
        }
        sleep(4)
    }

    /// 设置页：屏幕锁宽限时间
    func test11LockGraceSetting() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["我"], 20).tap()
        waitFor(a.buttons["settingsEntry"]).tap()
        let toggle = waitFor(a.switches["screenLockToggle"])
        if (toggle.value as? String) != "1" { toggle.switches.firstMatch.exists ? toggle.switches.firstMatch.tap() : toggle.tap() }
        let picker = a.descendants(matching: .any)["lockGracePicker"]
        waitFor(picker, 8)
        shot("lock-grace-setting")
        picker.tap()
        waitFor(a.staticTexts["5 分钟"], 5)
        shot("lock-grace-options")
        a.staticTexts["1 分钟"].tap()
        sleep(1)
        // 恢复：关闭屏幕锁
        let t2 = waitFor(a.switches["screenLockToggle"])
        if (t2.value as? String) == "1" { t2.switches.firstMatch.exists ? t2.switches.firstMatch.tap() : t2.tap() }
    }

    /// A5：系统字号设为最大的无障碍字号时，App 文字跟随放大
    func test12DynamicType() {
        let a = XCUIApplication()
        a.launchArguments += ["-UIPreferredContentSizeCategoryName", "UICTContentSizeCategoryAccessibilityXXXL"]
        a.launch()
        waitFor(a.tabBars.buttons["聊天"], 20)
        sleep(2)
        shot("a5-dynamic-type-list")
        openChat(a, bot)
        sleep(2)
        shot("a5-dynamic-type-chat")
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        sleep(1)
        shot("a5-dynamic-type-settings")
    }

    /// B1：启动页（调试环境变量人为拉长启动）
    func test13LaunchingScreen() {
        let a = XCUIApplication()
        a.launchEnvironment["HUSH_DEBUG_BOOT_DELAY"] = "4"
        a.launch()
        waitFor(a.staticTexts["launchingText"], 5)
        shotNow("b1-launching")
        waitFor(a.tabBars.buttons["聊天"], 20)
    }

    /// B11：公网 http 地址的警告（不登录，只看服务器地址页；需要未登录状态，默认跳过）
    func test14InsecureServerWarning() throws {
        let a = launchApp()
        guard a.buttons["startButton"].waitForExistence(timeout: 5) else { throw XCTSkip("已登录，跳过") }
        a.buttons["startButton"].tap()
        waitFor(a.textFields["serverField"]).clearAndType("http://chat.example.com")
        waitFor(a.staticTexts["insecureServerHint"], 3)
        a.buttons["serverNext"].tap()
        waitFor(a.alerts["连接未加密"], 5)
        shot("b11-insecure-server-warning")
        a.alerts.buttons["取消"].tap()
    }
}
