import XCTest
@testable import Hush

@MainActor
final class SessionResetTests: XCTestCase {
    func testResetStateClearsPreviousAccountCaches() throws {
        let core = HushCore.shared
        core.media.avatarPaths["av1"] = "/deleted/files/media/av1"
        core.media.mediaPaths["m1"] = "/deleted/files/media/m1"
        core.contacts.verifiedUsers = ["u1"]
        core.moments.notesSeenTs = 42
        core.activeConvId = "u:u1"
        // 缓存里放一张图，确认被清空
        let png = FileManager.default.temporaryDirectory.appendingPathComponent("reset-\(UUID().uuidString).png")
        try UIGraphicsImageRenderer(size: CGSize(width: 4, height: 4)).pngData { ctx in
            UIColor.red.setFill(); ctx.fill(CGRect(x: 0, y: 0, width: 4, height: 4))
        }.write(to: png)
        XCTAssertNotNil(ImageCache.image(png.path, maxPixel: 10))
        XCTAssertNotNil(ImageCache.cached(png.path, maxPixel: 10))
        let share = AppPaths.shareDir.appendingPathComponent("x", isDirectory: true)
        try FileManager.default.createDirectory(at: share, withIntermediateDirectories: true)

        core.resetState()

        XCTAssertTrue(core.media.avatarPaths.isEmpty, "登出后头像路径必须作废，否则重新登录后头像一直是占位图")
        XCTAssertTrue(core.media.mediaPaths.isEmpty)
        XCTAssertTrue(core.contacts.verifiedUsers.isEmpty)
        XCTAssertEqual(core.moments.notesSeenTs, 0)
        XCTAssertNil(core.activeConvId)
        XCTAssertNil(ImageCache.cached(png.path, maxPixel: 10), "上一个账号的图片不能继续显示")
        XCTAssertFalse(FileManager.default.fileExists(atPath: AppPaths.shareDir.path))
        try? FileManager.default.removeItem(at: png)
    }

    func testMediaCacheRefetchesWhenFileWasDeleted() async throws {
        let cache = MediaCache()
        let file = FileManager.default.temporaryDirectory.appendingPathComponent("media-\(UUID().uuidString)")
        try Data("x".utf8).write(to: file)
        var calls = 0
        await cache.loadAvatar("a") { _ in calls += 1; return file.path }
        await cache.loadAvatar("a") { _ in calls += 1; return file.path }
        XCTAssertEqual(calls, 1, "文件还在时不重复拉取")
        try FileManager.default.removeItem(at: file)
        try Data("y".utf8).write(to: file.appendingPathExtension("new"))
        await cache.loadAvatar("a") { _ in calls += 1; return file.appendingPathExtension("new").path }
        XCTAssertEqual(calls, 2, "文件已被核心删除时要重新拉取")
        XCTAssertEqual(cache.avatarPaths["a"], file.appendingPathExtension("new").path)
        try? FileManager.default.removeItem(at: file.appendingPathExtension("new"))
    }

    func testMediaCacheDropsResultsFromBeforeReset() async {
        let cache = MediaCache()
        let gate = AsyncStream<Void>.makeStream()
        let task = Task { await cache.loadMedia("m") { _ in for await _ in gate.stream { break }; return "/old/account/m" } }
        await Task.yield()
        cache.reset()
        gate.continuation.yield()
        await task.value
        XCTAssertNil(cache.mediaPaths["m"], "退出登录前发出的请求结果不能写回")
    }

    func testMomentNotesSeenIsPerAccountAndMigratesLegacyValue() throws {
        let suite = "hush-tests-\(UUID().uuidString)"
        let d = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { d.removePersistentDomain(forName: suite) }
        d.set(1000, forKey: "momentNotesSeenTs")
        XCTAssertEqual(MomentsStore.notesSeen(userId: "bob", defaults: d), 1000)
        XCTAssertNil(d.object(forKey: "momentNotesSeenTs"))
        XCTAssertEqual(MomentsStore.notesSeen(userId: "carol", defaults: d), 0, "新账号不能继承别人的已读时间")
        XCTAssertEqual(MomentsStore.notesSeen(userId: "bob", defaults: d), 1000)
        XCTAssertEqual(MomentsStore.notesSeen(userId: "", defaults: d), 0)
    }

    /// P-G：令牌过期（服务端 401）登出后的提示
    func testLoggedOutReasonText() {
        XCTAssertEqual(HushCore.loggedOutText("session_expired"), "登录已过期，请重新登录")
        XCTAssertEqual(HushCore.loggedOutText("logged_in_elsewhere"), "你的账号已在其他设备登录")
        XCTAssertEqual(HushCore.loggedOutText("x"), "登录已失效（x）")
    }
}
