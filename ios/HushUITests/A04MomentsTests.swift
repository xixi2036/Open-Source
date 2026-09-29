import XCTest

final class A04MomentsTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    func test07Moments() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["朋友圈"], 15).tap()
        let actions = waitFor(a.buttons["momentActions_\(UITestConfig.botName)"].firstMatch, 20)
        sleep(3)
        shot("28-moments-feed")
        if !actions.isHittable { a.swipeUp() }
        actions.tap()
        let like = waitFor(a.buttons["likeButton"])
        if like.label.contains("赞") && !like.label.contains("取消") { like.tap() } else { actions.tap() }
        sleep(1)
        a.buttons["momentActions_\(UITestConfig.botName)"].firstMatch.tap()
        waitFor(a.buttons["commentButton"]).tap()
        let field = waitFor(a.textFields["commentField"])
        field.typeText("写得真好")
        a.buttons["sendComment"].tap()
        let reply = a.staticTexts.containing(NSPredicate(format: "label CONTAINS %@", "谢谢评论")).firstMatch
        if !reply.waitForExistence(timeout: 8) {
            a.swipeDown()
            sleep(3)
        }
        waitFor(reply, 15)
        reply.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5))
            .press(forDuration: 0.05, thenDragTo: a.coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.55)))
        sleep(1)
        shot("29-moments-like-comment")
        a.swipeDown()
        sleep(2)
        let bar = a.buttons["newMomentNotes"]
        if bar.waitForExistence(timeout: 8) {
            shot("30-moments-new-notes-bar")
            bar.tap()
            sleep(2)
            shot("31-moment-notifications")
            a.navigationBars.buttons.element(boundBy: 0).tap()
        }
        // 发布带图动态
        waitFor(a.buttons["composeMoment"]).tap()
        let text = waitFor(a.textFields["momentText"].exists ? a.textFields["momentText"] : a.textViews["momentText"])
        text.tap()
        text.typeText("Hush 朋友圈初体验 📷")
        a.buttons["addMomentPhotos"].tap()
        let grid = a.images.matching(identifier: "PXGGridLayout-Info")
        waitFor(grid.element(boundBy: 0), 10)
        sleep(1)
        grid.element(boundBy: 0).coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        grid.element(boundBy: 2).coordinate(withNormalizedOffset: CGVector(dx: 0.5, dy: 0.5)).tap()
        a.buttons["完成"].firstMatch.tap()
        sleep(3)
        shot("32-compose-moment")
        a.buttons["postMoment"].tap()
        waitFor(a.staticTexts["Hush 朋友圈初体验 📷"], 20)
        a.swipeDown()
        sleep(4)
        shot("33-moment-posted")
    }
}
