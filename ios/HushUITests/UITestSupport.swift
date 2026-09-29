import XCTest

/// UI 测试配置：默认值保持原样（bob / 8080 预填地址 / 小助手 / 小美）。
/// 通过 xcodebuild 的 TEST_RUNNER_ 前缀环境变量覆盖，例如
/// `TEST_RUNNER_HUSH_SERVER=http://127.0.0.1:8092 TEST_RUNNER_HUSH_EMAIL=erin@hush.local TEST_RUNNER_HUSH_SHOT_PREFIX=fix-`
enum UITestConfig {
    static let env = ProcessInfo.processInfo.environment
    /// 登录时填写的服务器地址；不设置则使用输入框预填的默认值
    static let server = env["HUSH_SERVER"]
    static let email = env["HUSH_EMAIL"] ?? "bob@hush.local"
    static let name = env["HUSH_NAME"] ?? "Bob"
    static let botEmail = env["HUSH_BOT_EMAIL"] ?? "bot@hush.local"
    static let botName = env["HUSH_BOT_NAME"] ?? "小助手"
    static let bot2Email = env["HUSH_BOT2_EMAIL"] ?? "bot2@hush.local"
    static let bot2Name = env["HUSH_BOT2_NAME"] ?? "小美"
    /// 截图文件名前缀（例如 fix-），避免覆盖已有截图
    static let shotPrefix = env["HUSH_SHOT_PREFIX"] ?? ""
    static let shotDir: URL = env["HUSH_SHOT_DIR"].map { URL(fileURLWithPath: $0) }
        ?? XCTestCase.repoRoot.appendingPathComponent("docs/screenshots/ios")
    /// 调试转储（截图 + 界面树）与宿主机脚本标记文件所在目录
    static let dumpDir = URL(fileURLWithPath: env["HUSH_DUMP_DIR"] ?? "/tmp/hush-ui")
}

/// UI 测试公共工具：截图保存到 docs/screenshots/ios/，界面树转储到临时目录供调试
extension XCTestCase {
    static let repoRoot: URL = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()

    var app: XCUIApplication { XCUIApplication(bundleIdentifier: "com.hush.app") }

    func launchApp() -> XCUIApplication {
        let a = XCUIApplication()
        a.launch()
        allowSystemAlerts()
        return a
    }

    func allowSystemAlerts() {
        let sb = XCUIApplication(bundleIdentifier: "com.apple.springboard")
        for label in ["允许", "Allow", "好", "OK", "允许完全访问"] {
            let b = sb.buttons[label]
            if b.waitForExistence(timeout: 1.0) { b.tap() }
        }
    }

    func shot(_ name: String) {
        sleep(1)
        let png = XCUIScreen.main.screenshot().pngRepresentation
        let dir = UITestConfig.shotDir
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try? png.write(to: dir.appendingPathComponent("\(UITestConfig.shotPrefix)\(name).png"))
        let att = XCTAttachment(data: png, uniformTypeIdentifier: "public.png")
        att.name = name
        att.lifetime = .keepAlways
        add(att)
    }

    /// 不等待，立即截图（用于短暂出现的界面）
    func shotNow(_ name: String) {
        let png = XCUIScreen.main.screenshot().pngRepresentation
        let dir = UITestConfig.shotDir
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try? png.write(to: dir.appendingPathComponent("\(UITestConfig.shotPrefix)\(name).png"))
    }

    /// 调试用：保存截图与界面树到 dumpDir（默认 /tmp/hush-ui/）
    func dump(_ name: String, _ a: XCUIApplication) {
        let dir = UITestConfig.dumpDir
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try? XCUIScreen.main.screenshot().pngRepresentation.write(to: dir.appendingPathComponent("\(name).png"))
        try? a.debugDescription.write(to: dir.appendingPathComponent("\(name).txt"), atomically: true, encoding: .utf8)
    }

    @discardableResult
    func waitFor(_ e: XCUIElement, _ timeout: TimeInterval = 10, file: StaticString = #filePath, line: UInt = #line) -> XCUIElement {
        if !e.waitForExistence(timeout: timeout) {
            dump("fail", XCUIApplication())
            XCTFail("未出现：\(e)", file: file, line: line)
        }
        return e
    }

    func tapIfExists(_ e: XCUIElement, timeout: TimeInterval = 2) {
        if e.waitForExistence(timeout: timeout) { e.tap() }
    }

    func typeSlowly(_ e: XCUIElement, _ text: String) {
        e.tap()
        e.typeText(text)
    }

    /// 打开与指定名称的会话（从聊天 Tab）
    func openConversation(_ a: XCUIApplication, _ title: String) {
        a.tabBars.buttons["聊天"].tap()
        let back = a.navigationBars.buttons.element(boundBy: 0)
        var guardCount = 0
        while !a.navigationBars["聊天"].exists && back.exists && guardCount < 5 { back.tap(); guardCount += 1 }
        let cell = a.buttons.matching(NSPredicate(format: "identifier == %@", "conv_\(title)")).firstMatch
        waitFor(cell, 15).tap()
    }

    /// 宿主机脚本监听的标记文件
    func mark(_ name: String) {
        try? FileManager.default.createDirectory(at: UITestConfig.dumpDir, withIntermediateDirectories: true)
        try? "1".write(to: UITestConfig.dumpDir.appendingPathComponent(name), atomically: true, encoding: .utf8)
    }

    func sendMessage(_ a: XCUIApplication, _ text: String) {
        let input = waitFor(a.textFields["messageInput"].exists ? a.textFields["messageInput"] : a.textViews["messageInput"])
        input.tap()
        input.typeText(text)
        waitFor(a.buttons["sendButton"]).tap()
    }
}

extension XCUIElement {
    /// 清空输入框后输入
    func clearAndType(_ text: String) {
        tap()
        let current = (value as? String) ?? ""
        // 光标移到末尾后逐字删除
        coordinate(withNormalizedOffset: CGVector(dx: 0.95, dy: 0.5)).tap()
        typeText(String(repeating: XCUIKeyboardKey.delete.rawValue, count: current.count + 2))
        typeText(text)
    }
}
