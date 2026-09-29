import XCTest

/// fix2 回归：朋友圈第二页评论后保持位置、@ 插入 / 整段删除后的光标位置、搜索跳转在后台翻页。
/// 需要：已登录的测试账号；第三个账号（HUSH_PEER_NAME，默认「路人甲」）是好友并发了 >20 条动态，
/// 最早一条为「U6目标动态」；群「v3测试群」里有两个测试机器人（HUSH_BOT_NAME / HUSH_BOT2_NAME）。截图前缀建议 fix2-。
final class A13Fix2Tests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    private var peerName: String { UITestConfig.env["HUSH_PEER_NAME"] ?? "路人甲" }
    private var bot: String { UITestConfig.botName }
    private var bot2: String { UITestConfig.bot2Name }
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

    private func conv(_ a: XCUIApplication, _ title: String) -> XCUIElement {
        a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_\(title)")).firstMatch
    }

    // MARK: - 1 朋友圈第二页评论后不跳回顶部

    func test01CommentOnSecondPageKeepsPosition() {
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
            return bestDy < 400 ? best : nil
        }
        var n = 0
        var found: XCUIElement?
        while n < 40 {
            if let e = targetActions() { found = e; break }
            if target.exists && target.isHittable { a.swipeUp(velocity: .slow) } else { a.swipeUp() }
            n += 1
        }
        guard let button = found else { return XCTFail("向下翻到第二页找到目标动态") }
        button.tap()
        waitFor(a.buttons["commentButton"]).tap()
        let field = waitFor(a.textFields["commentField"])
        let text = "第二页评论\(Int(Date().timeIntervalSince1970) % 100000)"
        field.typeText(text)
        shot("fix2-01-comment-page2-typing")
        waitFor(a.buttons["sendComment"]).tap()
        let comment = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", text)).firstMatch
        waitFor(comment, 10)
        // 键盘收起动画与评论写回都完成后再检查：没有跳回顶部，目标卡片与新评论仍在屏幕内
        sleep(3)
        shot("fix2-02-comment-page2-kept")
        XCTAssertTrue(target.exists && target.isHittable, "评论后目标动态仍在视野中")
        XCTAssertTrue(comment.isHittable, "新评论在视野中")
        XCTAssertFalse(a.buttons["momentNotesHistory"].exists && a.buttons["momentNotesHistory"].isHittable, "没有滚回顶部")
    }

    /// 第一条动态（列表顶部）评论：评论框弹出时卡片对齐到评论框上方，发送后回到可视区域，下拉刷新仍可用
    func test04CommentOnFirstCard() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["朋友圈"], 15).tap()
        let first = waitFor(a.buttons["momentActions_\(peerName)"].firstMatch, 15)
        first.tap()
        waitFor(a.buttons["commentButton"]).tap()
        let field = waitFor(a.textFields["commentField"])
        let text = "首条评论\(Int(Date().timeIntervalSince1970) % 100000)"
        field.typeText(text)
        sleep(1)
        shot("fix2-07-comment-first-card-typing")
        waitFor(a.buttons["sendComment"]).tap()
        let comment = a.staticTexts.matching(NSPredicate(format: "label CONTAINS %@", text)).firstMatch
        waitFor(comment, 10)
        sleep(2)
        XCTAssertTrue(comment.isHittable, "新评论在视野中")
        shot("fix2-08-comment-first-card-sent")
        // 下拉刷新后回到顶部，封面下的入口可见
        var n = 0
        while !(a.buttons["momentNotesHistory"].exists && a.buttons["momentNotesHistory"].isHittable)
                && !(a.buttons["newMomentNotes"].exists && a.buttons["newMomentNotes"].isHittable) && n < 6 {
            a.swipeDown(); n += 1
        }
        XCTAssertLessThan(n, 6, "能回到顶部")
    }

    // MARK: - 2 @ 插入 / 整段删除后的光标

    /// 「你好 @测试助手 再见」→ 双击选中片段里的字并删除 → 整段删除，光标停在删除处，接着输入 X 落在原位置；
    /// 再在中间输入 @ 选择成员 → 插入到光标处，光标在「@昵称 」之后，接着输入 Y 紧跟其后。
    /// XCUITest 读不到光标，用后续输入落在哪里来验证（旧行为：光标跳到末尾，X / Y 会追加在「再见」之后）。
    func test02MentionCursorStaysInPlace() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        waitFor(conv(a, groupName), 15).tap()
        let field = waitFor(input(a))
        field.tap()
        field.typeText("你好 @")
        waitFor(a.buttons["mention_\(bot)"]).tap()
        input(a).typeText("再见")
        XCTAssertEqual(input(a).value as? String, "你好 @\(bot) 再见")
        // 双击片段中间的字（选中一个词），删除 → 碰到提及片段，整段删除
        let f = input(a)
        let charW: CGFloat = 17
        let tagMid = f.coordinate(withNormalizedOffset: CGVector(dx: 0, dy: 0.5))
            .withOffset(CGVector(dx: charW * 2 + 6 + charW * 2.5, dy: 0))
        tagMid.doubleTap()
        sleep(1)
        shot("fix2-03-mention-selected")
        input(a).typeText(XCUIKeyboardKey.delete.rawValue)
        sleep(1)
        // 系统的「智能删除」可能顺带删掉片段后的空格：只要求提及整段消失、前后文字保留
        let afterDelete = (input(a).value as? String) ?? ""
        XCTAssertFalse(afterDelete.contains("@"), "整段删除提及片段：\(afterDelete)")
        XCTAssertTrue(afterDelete.hasPrefix("你好 ") && afterDelete.hasSuffix("再见"), afterDelete)
        let rest = String(afterDelete.dropFirst(3))           // 删除处之后的文字（「 再见」或「再见」）
        input(a).typeText("X")
        XCTAssertEqual(input(a).value as? String, "你好 X" + rest, "删除后光标停在删除处（不是末尾）")
        // 在中间输入 @：筛选词只取到光标处，弹出全部成员；插入后光标在「@昵称 」之后
        input(a).typeText("@")
        waitFor(a.buttons["mention_\(bot2)"], 5).tap()
        XCTAssertEqual(input(a).value as? String, "你好 X@\(bot2) " + rest)
        input(a).typeText("Y")
        XCTAssertEqual(input(a).value as? String, "你好 X@\(bot2) Y" + rest, "插入提及后光标在片段之后（不是末尾）")
        shot("fix2-04-mention-cursor-in-middle")
        // 清空输入框，不留草稿
        let len = ((input(a).value as? String) ?? "").count
        input(a).coordinate(withNormalizedOffset: CGVector(dx: 0.97, dy: 0.5)).tap()
        input(a).typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: len + 4))
    }

    // MARK: - 3 搜索跳转：后台翻页定位

    /// 与 A11V3Tests.test06 相同的数据：「定位目标V3」之后还有 50+ 条消息，不在首屏一页里。
    /// 点击搜索结果后在后台向上翻页（页面顶部显示「正在定位消息…」），找到后滚动并高亮。
    func test03SearchJumpLocatesInBackground() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15).tap()
        backToList(a)
        waitFor(a.buttons["searchButton"]).tap()
        a.searchFields.firstMatch.typeText("定位目标V3")
        let mine = a.buttons.matching(NSPredicate(format: "label CONTAINS %@ AND NOT (label CONTAINS %@)", "定位目标V3", "收到")).firstMatch
        waitFor(mine, 15)
        shot("fix2-05-search-results")
        mine.tap()
        usleep(1_300_000)
        shotNow("fix2-06-search-jump-highlight")
        let target = a.staticTexts.matching(NSPredicate(format: "label == %@", "定位目标V3")).firstMatch
        waitFor(target, 10)
        XCTAssertTrue(target.isHittable, "跳转后目标消息在屏幕内")
        XCTAssertFalse(a.descendants(matching: .any)["locatingIndicator"].exists, "定位完成后加载指示消失")
        // 输入框仍可用（主线程没有被翻页阻塞）
        input(a).tap()
        XCTAssertTrue(input(a).exists)
    }
}
