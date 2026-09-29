import XCTest

/// v3 收尾实测（U1–U6、K2/K3 回归）。需要：已登录的测试账号、两个测试机器人（HUSH_BOT_NAME / HUSH_BOT2_NAME），
/// 第三个账号（HUSH_PEER_NAME，默认「路人甲」，没有设备密钥，转发给它会失败）是本账号好友并发了 >20 条动态，
/// 最早一条为「U6目标动态」；宿主机脚本监听 HUSH_DUMP_DIR 下的标记文件（暂停 / 恢复机器人、让第三个账号回复评论）。
/// 用例按编号顺序单独运行。截图前缀建议 v3-。
final class A11V3Tests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    private var bot: String { UITestConfig.botName }
    private var bot2: String { UITestConfig.bot2Name }
    private var peerName: String { UITestConfig.env["HUSH_PEER_NAME"] ?? "路人甲" }
    private let groupName = "v3测试群"

    private func input(_ a: XCUIApplication) -> XCUIElement {
        a.textFields["messageInput"].exists ? a.textFields["messageInput"] : a.textViews["messageInput"]
    }

    private func backToList(_ a: XCUIApplication) {
        var n = 0
        while !a.navigationBars["聊天"].exists && n < 5 {
            a.navigationBars.buttons.element(boundBy: 0).tap(); n += 1
            sleep(1)
        }
    }

    /// 等宿主机脚本处理完标记
    private func hostDo(_ name: String, timeout: TimeInterval = 15) {
        let done = UITestConfig.dumpDir.appendingPathComponent("\(name)-done")
        try? FileManager.default.removeItem(at: done)
        mark(name)
        let end = Date().addingTimeInterval(timeout)
        while !FileManager.default.fileExists(atPath: done.path) && Date() < end { usleep(200_000) }
        XCTAssertTrue(FileManager.default.fileExists(atPath: done.path), "宿主机未处理 \(name)")
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

    private func conv(_ a: XCUIApplication, _ title: String) -> XCUIElement {
        a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_\(title)")).firstMatch
    }

    // MARK: - U4 @提及

    /// 建群（两个机器人），输入 @ 弹出成员选择，插入 @测试助手 发送；机器人回复 @我 → 我的气泡强调色、被 @ 的气泡左侧竖线
    func test01MentionInGroup() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        if !conv(a, groupName).waitForExistence(timeout: 3) {
            waitFor(a.buttons["newChatMenu"]).tap()
            waitFor(a.buttons["newGroupChat"]).tap()
            waitFor(a.textFields["groupNameField"]).tap()
            a.textFields["groupNameField"].typeText(groupName)
            waitFor(a.buttons["select_\(bot)"]).tap()
            waitFor(a.buttons["select_\(bot2)"]).tap()
            waitFor(a.buttons["createGroupButton"]).tap()
            waitFor(a.buttons["chatMoreButton"].firstMatch, 15)
        } else {
            conv(a, groupName).tap()
        }
        let field = waitFor(input(a))
        field.tap()
        field.typeText("你好 @")
        waitFor(a.descendants(matching: .any)["mentionPicker"], 5)
        waitFor(a.buttons["mention_\(bot)"])
        waitFor(a.buttons["mention_\(bot2)"])
        XCTAssertFalse(a.buttons["mention_\(UITestConfig.name)"].exists, "不能 @ 自己")
        shot("01-mention-picker")
        // 继续输入筛选
        field.typeText("测试助")
        sleep(1)
        XCTAssertFalse(a.buttons["mention_\(bot2)"].exists, "筛选后只剩匹配的成员：\(input(a).value ?? "")")
        a.buttons["mention_\(bot)"].tap()
        XCTAssertEqual(input(a).value as? String, "你好 @\(bot) ")
        // 退格碰到提及片段 → 整段删除
        input(a).typeText(XCUIKeyboardKey.delete.rawValue)       // 删空格
        input(a).typeText(XCUIKeyboardKey.delete.rawValue)       // 删到片段 → 整段删除
        let afterDelete = (input(a).value as? String) ?? ""
        XCTAssertEqual(afterDelete, "你好 ", "删除碰到 @片段时整段删除")
        shot("02-mention-deleted-whole")
        input(a).typeText("@")
        waitFor(a.buttons["mention_\(bot)"]).tap()
        input(a).typeText("在吗")
        waitFor(a.buttons["sendButton"]).tap()
        // 机器人回复「@我 收到@」（带提及）
        let bar = a.descendants(matching: .any)["mentionBar"].firstMatch
        waitFor(bar, 20)
        sleep(1)
        shot("03-mention-bubbles")
    }

    /// 免打扰群里被 @：列表红色「[有人@我]」，仍弹通知
    func test02MentionInMutedGroupNotifies() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        let row = waitFor(conv(a, groupName), 15)
        // 设为免打扰（长按菜单）
        if !(row.label.contains("免打扰")) {
            row.press(forDuration: 1.0)
            let mute = a.buttons["免打扰"]
            if mute.waitForExistence(timeout: 3) { mute.tap() } else { a.buttons["取消免打扰"].firstMatch.tap(); XCTFail("已是免打扰") }
            sleep(1)
        }
        row.tap()
        let field = waitFor(input(a))
        hostDo("pause-ibot")
        field.tap()
        field.typeText("@")
        waitFor(a.buttons["mention_\(bot)"]).tap()
        input(a).typeText("离开后再回复我")
        waitFor(a.buttons["sendButton"]).tap()
        sleep(2)
        backToList(a)
        hostDo("resume-ibot")
        let springboard = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        let banner = springboard.descendants(matching: .any).matching(NSPredicate(format: "label CONTAINS %@", "有人@了你")).firstMatch
        waitFor(banner, 20)
        shotNow("04-muted-mention-banner")
        sleep(5)
        let mention = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "[有人@我]")).firstMatch
        if !mention.exists {
            XCTAssertTrue(conv(a, groupName).label.contains("[有人@我]"), "列表预览显示 [有人@我]：\(conv(a, groupName).label)")
        }
        shot("05-list-mention-prefix")
        // 进入会话后清零
        conv(a, groupName).tap()
        sleep(2)
        backToList(a)
        sleep(1)
        XCTAssertFalse(conv(a, groupName).label.contains("[有人@我]"), "mark_read 后不再显示")
    }

    // MARK: - U3 草稿

    func test03Draft() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        openChat(a, bot)
        let field = waitFor(input(a))
        field.tap()
        field.typeText("这是一段没发出去的草稿")
        backToList(a)
        let row = waitFor(conv(a, bot))
        let deadline = Date().addingTimeInterval(5)
        while !row.label.contains("[草稿]") && Date() < deadline { usleep(300_000) }
        XCTAssertTrue(row.label.contains("[草稿] 这是一段没发出去的草稿"), row.label)
        shot("06-list-draft")
        // 杀进程重启：草稿由核心保存
        a.terminate()
        let b = launchApp()
        waitFor(b.tabBars.buttons["聊天"], 15).tap()
        XCTAssertTrue(waitFor(conv(b, bot)).label.contains("[草稿]"))
        openChat(b, bot)
        XCTAssertEqual(input(b).value as? String, "这是一段没发出去的草稿", "进入聊天页恢复草稿")
        shot("07-draft-restored")
        // 发送后草稿清空
        waitFor(b.buttons["sendButton"]).tap()
        sleep(2)
        backToList(b)
        sleep(1)
        XCTAssertFalse(conv(b, bot).label.contains("[草稿]"))
    }

    // MARK: - U2 转发

    func test04Forward() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        openChat(a, bot)
        sendMessage(a, "图片")
        // 机器人回一张图片：长按最后一张图片转发（附件复用原密文，不重新上传）
        sleep(4)
        let images = a.descendants(matching: .any).matching(identifier: "imageMessage")
        waitFor(images.firstMatch, 15)
        images.element(boundBy: images.count - 1).press(forDuration: 0.8)
        waitFor(a.buttons["action_forward"]).tap()
        let search = waitFor(a.searchFields.firstMatch)
        waitFor(a.buttons["forward_\(bot2)"]).tap()
        waitFor(a.buttons["forward_\(groupName)"]).tap()
        search.tap()
        search.typeText(String(peerName.prefix(2)))
        waitFor(a.buttons["forward_\(peerName)"]).tap()
        shot("08-forward-picker")
        waitFor(a.buttons["forwardSend"]).tap()
        // 第三个账号没有设备密钥：那一条失败，其余成功
        let retry = a.buttons["forwardRetry"].firstMatch
        waitFor(retry, 30)
        shot("09-forward-partial-failure")
        retry.firstMatch.tap()
        sleep(3)
        backToList(a)
        openChat(a, bot2)
        waitFor(a.descendants(matching: .any)["forwardedLabel"].firstMatch, 15)
        sleep(2)
        shot("10-forwarded-label")
        backToList(a)
        openChat(a, peerName)
        waitFor(a.buttons["resendButton"].firstMatch, 10)
        shot("11-forward-failed-resend")
    }

    // MARK: - U1 删除会话

    func test05DeleteConversation() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        let row = waitFor(conv(a, peerName), 10)
        row.swipeLeft()
        waitFor(a.buttons["删除"].firstMatch).tap()
        waitFor(a.buttons["confirmDeleteConversation"].firstMatch)
        shot("12-delete-confirm")
        a.buttons["confirmDeleteConversation"].firstMatch.tap()
        sleep(2)
        XCTAssertFalse(conv(a, peerName).exists)
        shot("13-deleted")
        a.terminate()
        let b = launchApp()
        waitFor(b.tabBars.buttons["聊天"], 15).tap()
        sleep(2)
        XCTAssertFalse(conv(b, peerName).exists, "重启后仍不出现（核心已删除，不是 App 隐藏）")
    }

    // MARK: - U5 搜索跳转

    /// 先发目标消息，再发 25 条（机器人每条都回复，共 50+ 条，目标不在首屏一页 40 条里），搜索后点击跳转并高亮
    func test06SearchJump() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        // HUSH_V3_SKIP_FILL=1：数据已在上一次运行中发好，只做搜索跳转（再发一条目标会落在最新一页）
        if UITestConfig.env["HUSH_V3_SKIP_FILL"] != "1" {
            openChat(a, bot2)
            sendMessage(a, "定位目标V3")
            sleep(1)
            for i in 1...25 {
                input(a).tap()
                input(a).typeText("填充 \(i)")
                a.buttons["sendButton"].tap()
            }
            sleep(5)
            backToList(a)
        }
        waitFor(a.buttons["searchButton"]).tap()
        a.searchFields.firstMatch.typeText("定位目标V3")
        let result = a.buttons.matching(NSPredicate(format: "label BEGINSWITH %@ AND label CONTAINS %@", bot2, "定位目标V3")).firstMatch
        waitFor(result, 15)
        shot("14-search-results")
        // 选自己发的那条（不是机器人的「收到：…」）
        let mine = a.buttons.matching(NSPredicate(format: "label CONTAINS %@ AND NOT (label CONTAINS %@)", "定位目标V3", "收到")).firstMatch
        (mine.exists ? mine : result).tap()
        // 高亮持续 2.5 秒：先截图再查询界面树（查询本身较慢）
        usleep(1_300_000)
        shotNow("15-search-jump-highlight")
        let target = a.staticTexts.matching(NSPredicate(format: "label == %@", "定位目标V3")).firstMatch
        waitFor(target, 10)
        XCTAssertTrue(target.isHittable, "跳转后目标消息在屏幕内")
    }

    // MARK: - U6 朋友圈通知对应的动态不在第一页

    func test07MomentNotificationNotOnFirstPage() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["朋友圈"], 15).tap()
        let target = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "U6目标动态")).firstMatch
        let actions = a.buttons.matching(identifier: "momentActions_\(peerName)")
        /// 目标卡片的「··」：在目标文字下方、离它最近的那个
        func targetActions() -> XCUIElement? {
            guard target.exists, target.isHittable else { return nil }
            var best: XCUIElement?
            var bestDy = CGFloat.greatestFiniteMagnitude
            for i in 0..<actions.count {
                let e = actions.element(boundBy: i)
                guard e.exists, e.isHittable else { continue }
                let dy = e.frame.minY - target.frame.maxY
                if dy >= 0 && dy < bestDy { bestDy = dy; best = e }
            }
            // 下一张卡片的按钮离得更远：只接受卡片内的距离
            return bestDy < 400 ? best : nil
        }
        var n = 0
        var found: XCUIElement?
        while n < 40 {
            if let e = targetActions() { found = e; break }
            if target.exists && target.isHittable { a.swipeUp(velocity: .slow) } else { a.swipeUp() }
            n += 1
        }
        XCTAssertNotNil(found, "向下翻到第二页找到目标动态")
        let best = found
        best!.tap()
        waitFor(a.buttons["commentButton"]).tap()
        let field = waitFor(a.textFields["commentField"])
        field.typeText("第二页的评论")
        shot("16-feed-page2-target")
        waitFor(a.buttons["sendComment"]).tap()
        sleep(2)
        let before = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "U6回复")).count
        hostDo("zed-reply")
        // 通知到达：feed 中目标卡片直接出现回复（P-D：不在第一页时单独拉取这一条，不重新加载整个列表）
        // 发送评论后列表保持位置（fix2，见 A13Fix2Tests.test01）；回复到达前若用户滚动过，这里向下找回目标卡片
        sleep(3)
        let replies = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "U6回复"))
        var k = 0
        while !(replies.count > before && replies.element(boundBy: replies.count - 1).isHittable) && k < 30 {
            a.swipeUp(); k += 1
        }
        XCTAssertGreaterThan(replies.count, before, "目标卡片（第二页）收到回复后就地更新")
        let targetText = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "U6目标动态")).firstMatch
        if targetText.exists && targetText.frame.minY > 400 { a.swipeUp(velocity: .slow) }
        shot("17-feed-card-updated")
        // 通知列表 → 详情
        var m = 0
        func hittable(_ e: XCUIElement) -> Bool { e.exists && e.isHittable }
        // 回到顶部：入口在封面下方，被导航栏盖住时点不到
        while m < 40 {
            a.swipeDown(velocity: .fast); m += 1
            if (hittable(a.buttons["newMomentNotes"]) || hittable(a.buttons["momentNotesHistory"])) && m >= 3 { break }
        }
        sleep(1)
        if a.buttons["newMomentNotes"].exists { a.buttons["newMomentNotes"].tap() } else { waitFor(a.buttons["momentNotesHistory"]).tap() }
        let note = waitFor(a.buttons["momentNote_\(peerName)"].firstMatch, 10)
        shot("18-moment-notes")
        note.tap()
        waitFor(a.navigationBars["详情"], 10)
        waitFor(a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "U6目标动态")).firstMatch, 10)
        waitFor(a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", "U6回复")).firstMatch, 10)
        shot("19-moment-detail-from-note")
    }

    // MARK: - K2 / K3 回归

    /// 邮箱搜索好友之后，单聊设置页仍显示对方邮箱
    func test08EmailAfterSearch() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["通讯录"], 15).tap()
        waitFor(a.buttons["addFriendButton"]).tap()
        let f = waitFor(a.textFields["searchEmailField"])
        f.tap()
        f.typeText(UITestConfig.botEmail)
        waitFor(a.buttons["searchEmailButton"]).tap()
        waitFor(a.buttons["searchResult"], 10)
        a.navigationBars.buttons.element(boundBy: 0).tap()
        waitFor(a.buttons["friend_\(bot)"]).tap()
        waitFor(a.buttons["profileSendMessage"]).tap()
        waitFor(a.buttons["chatMoreButton"].firstMatch).tap()
        let email = waitFor(a.staticTexts["peerEmail"], 10)
        XCTAssertTrue(email.label.contains(UITestConfig.botEmail), email.label)
        shot("20-k2-email-after-search")
    }

    /// 连发多条，机器人逐条回复；记录实时顺序，杀进程重启后（从库里按 sort_ts 读取）顺序一致
    func test09OrderStableAfterRelaunch() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        openChat(a, bot)
        for i in 1...5 {
            input(a).tap()
            input(a).typeText("顺序\(i)")
            a.buttons["sendButton"].tap()
        }
        sleep(6)
        func order(_ app: XCUIApplication) -> [String] {
            app.staticTexts.matching(identifier: "messageText").allElementsBoundByIndex
                .map(\.label).filter { $0.contains("顺序") }
        }
        let live = order(a)
        shot("21-k3-order-live")
        a.terminate()
        let b = launchApp()
        waitFor(b.tabBars.buttons["聊天"], 15).tap()
        openChat(b, bot)
        sleep(3)
        let reloaded = order(b)
        XCTAssertEqual(live, reloaded, "实时顺序与重启后按 sort_ts 读取的顺序一致")
        XCTAssertEqual(live.filter { !$0.contains("收到") }, (1...5).map { "顺序\($0)" })
        shot("22-k3-order-relaunched")
    }
}
