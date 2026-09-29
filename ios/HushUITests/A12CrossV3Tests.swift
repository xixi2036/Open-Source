import XCTest

/// v3 跨平台回归（需 Android 端先发送 @提及与转发）：只截图，不修改数据。
final class A12CrossV3Tests: XCTestCase {
    override func setUpWithError() throws {
        continueAfterFailure = true
        guard ProcessInfo.processInfo.environment["HUSH_CROSS"] != nil else { throw XCTSkip("跨平台用例需以 TEST_RUNNER_HUSH_CROSS=1 运行") }
    }

    func test01ShowListGroupAndDirect() {
        let a = launchApp()
        waitFor(a.tabBars.buttons["聊天"], 15)
        sleep(4)
        shotNow("cross-v3-01-list")
        let cell = a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_Alice")).firstMatch
        if cell.waitForExistence(timeout: 10) { cell.tap(); sleep(3); shotNow("cross-v3-02-forwarded") }
        a.navigationBars.buttons.element(boundBy: 0).tap()
        sleep(1)
        let groups = a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_机器人测试群"))
        for i in 0..<groups.count {
            let g = groups.element(boundBy: i)
            g.tap(); sleep(3); shotNow("cross-v3-03-group-\(i)")
            a.navigationBars.buttons.element(boundBy: 0).tap(); sleep(1)
        }
    }
}
