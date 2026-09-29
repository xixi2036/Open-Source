import XCTest

final class A01LoginTests: XCTestCase {
    override func setUp() { continueAfterFailure = false }

    func test01Login() {
        let a = launchApp()
        let start = waitFor(a.buttons["startButton"], 15)
        shot("01-welcome")
        start.tap()
        let server = waitFor(a.textFields["serverField"])
        if let url = UITestConfig.server {
            server.clearAndType(url)
        }
        shot("02-login-server")
        a.buttons["serverNext"].tap()
        let email = waitFor(a.textFields["emailField"])
        email.tap()
        email.typeText(UITestConfig.email)
        shot("03-login-email")
        a.buttons["emailNext"].tap()
        let code = waitFor(a.textFields["codeField"])
        code.typeText("12345")
        shot("04-login-code")
        code.typeText("6")
        // 新用户：资料页
        let name = a.textFields["nameField"]
        if name.waitForExistence(timeout: 8) {
            // 新账号的昵称预填为邮箱前缀：清空后再输入
            name.clearAndType(UITestConfig.name)
            shot("05-login-profile")
            a.buttons["profileDone"].tap()
        }
        waitFor(a.tabBars.buttons["聊天"], 15)
        sleep(2)
        shot("06-chats-empty")
    }
}
