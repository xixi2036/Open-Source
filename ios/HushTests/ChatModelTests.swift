import XCTest
@testable import Hush

@MainActor
final class ChatModelTests: XCTestCase {
    private let conv = "u:peer"

    private func msg(_ id: String, ts: Int64, out: Bool = false) -> MessageItem {
        MessageItem(id: id, convId: conv, senderId: out ? "me" : "peer", isOutgoing: out, sentAt: ts, serverTs: ts, sortTs: ts,
                    status: out ? .sent : .delivered, body: .text(text: id), quote: nil, reactions: [],
                    expireSecs: 0, expiresAt: nil, deleted: false, forwarded: false, mentions: [])
    }

    private func model(_ list: [MessageItem]) -> ChatModel {
        let m = ChatModel(convId: conv)
        m.messages = list
        return m
    }

    func testIncomingWhileReadingHistoryDoesNotScroll() {
        let m = model([msg("a", ts: 1_000), msg("b", ts: 2_000)])
        m.setAtBottom(false)
        let tick = m.bottomTick
        m.handle(.messageAdded(convId: conv, message: msg("c", ts: 3_000), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.bottomTick, tick, "用户在看历史时不能被拉到底部")
        XCTAssertEqual(m.unseenCount, 1)
        m.handle(.messageAdded(convId: conv, message: msg("d", ts: 4_000), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.unseenCount, 2)
        m.setAtBottom(true)
        XCTAssertEqual(m.unseenCount, 0)
    }

    func testOutgoingOrAtBottomScrolls() {
        let m = model([msg("a", ts: 1_000)])
        m.handle(.messageAdded(convId: conv, message: msg("b", ts: 2_000), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.bottomTick, 1)
        m.setAtBottom(false)
        m.handle(.messageAdded(convId: conv, message: msg("c", ts: 3_000, out: true), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.bottomTick, 2, "自己发的消息总是滚到底部")
        XCTAssertEqual(m.unseenCount, 0)
    }

    func testBacklogInsertedInMiddleNeitherScrollsNorCounts() {
        let m = model([msg("a", ts: 1_000), msg("c", ts: 3_000)])
        m.setAtBottom(false)
        m.handle(.messageAdded(convId: conv, message: msg("b", ts: 2_000), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.messages.map(\.id), ["a", "b", "c"])
        XCTAssertEqual(m.bottomTick, 0)
        XCTAssertEqual(m.unseenCount, 0)
    }

    /// P-A：排序、插入位置只看核心给出的 sortTs（对方时钟偏差、serverTs 缺失都不影响）
    func testOrderingUsesCoreSortTs() {
        let m = model([msg("a", ts: 1_000), msg("c", ts: 3_000)])
        var skewed = msg("b", ts: 2_000)
        skewed.sentAt = 99_000   // 对方手机时间快了
        skewed.serverTs = 0      // 服务端时间缺失
        m.handle(.messageAdded(convId: conv, message: skewed, backlog: true, mentionsMe: false))
        XCTAssertEqual(m.messages.map(\.id), ["a", "b", "c"])
    }

    /// P-A：上翻页的游标是最早一条的 sortTs（不是 sentAt / serverTs），边界上重复的消息按 id 去重
    func testLoadOlderUsesSortTsCursor() {
        var first = msg("b", ts: 5_000)
        first.sentAt = 9_000
        first.serverTs = 0
        let m = model([first, msg("c", ts: 6_000)])
        var cursors: [Int64?] = []
        m.loadPage = { before, _ in
            cursors.append(before)
            return [self.msg("a", ts: 4_000), self.msg("b", ts: 5_000)]
        }
        m.hasMore = true
        m.loadOlder()
        XCTAssertEqual(cursors, [5_000])
        XCTAssertEqual(m.messages.map(\.id), ["a", "b", "c"])
        XCTAssertFalse(m.hasMore, "不足一页说明没有更早的消息")
    }

    /// K3：自己发出的消息被服务端接受后 sortTs 变为服务端时间，聊天页按新时间重新排位
    func testUpdatedSortTsRepositionsMessage() {
        let m = model([msg("mine", ts: 5_000, out: true), msg("reply", ts: 6_000)])
        var sent = msg("mine", ts: 7_000, out: true)
        sent.status = .delivered
        m.handle(.messageUpdated(convId: conv, message: sent))
        XCTAssertEqual(m.messages.map(\.id), ["reply", "mine"])
        XCTAssertEqual(m.messages.last?.status, .delivered)
        var same = sent
        same.status = .read
        m.handle(.messageUpdated(convId: conv, message: same))
        XCTAssertEqual(m.messages.map(\.id), ["reply", "mine"], "排序时间不变时原地更新")
        XCTAssertEqual(m.messages.last?.status, .read)
    }

    func testOtherConversationIgnored() {
        let m = model([msg("a", ts: 1_000)])
        m.handle(.messageAdded(convId: "u:other", message: msg("x", ts: 2_000), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.messages.count, 1)
    }

    func testMergeLatestKeepsOlderPages() {
        let old = (0..<5).map { msg("old\($0)", ts: Int64($0) * 1_000) }
        let recent = [msg("r1", ts: 10_000), msg("r2", ts: 11_000), msg("gone", ts: 12_000)]
        let m = model(old + recent)
        m.setAtBottom(false)
        // 回来时最新一页：gone 已被删除（阅后即焚），新增 r3
        m.merge(latest: [msg("r1", ts: 10_000), msg("r2", ts: 11_000), msg("r3", ts: 13_000)])
        XCTAssertEqual(m.messages.map(\.id), old.map(\.id) + ["r1", "r2", "r3"], "已加载的历史页必须保留")
        XCTAssertEqual(m.unseenCount, 1)
        m.merge(latest: [])
        XCTAssertTrue(m.messages.isEmpty, "会话被清空")
    }

    func testDayBreaksUseSortTimeAndSnapshot() {
        let day: Int64 = 86_400_000
        let base: Int64 = 1_700_000_000_000
        let list = [msg("a", ts: base), msg("b", ts: base + 1_000), msg("c", ts: base + 2 * day)]
        XCTAssertEqual(ChatModel.dayBreaks(list), [true, false, true])
        XCTAssertEqual(ChatModel.dayBreaks([]), [])
    }

    // MARK: - U5 / fix2 搜索跳转：后台向上翻页直到找到

    /// 线程安全的计数与记录（分页在后台线程调用）
    private final class Recorder: @unchecked Sendable {
        private let lock = NSLock()
        private var _cursors: [Int64?] = []
        private var _mainThreadCalls = 0
        var cursors: [Int64?] { lock.withLock { _cursors } }
        var n: Int { lock.withLock { _cursors.count } }
        var mainThreadCalls: Int { lock.withLock { _mainThreadCalls } }
        func record(_ c: Int64?) {
            let main = Thread.isMainThread
            lock.withLock { _cursors.append(c); if main { _mainThreadCalls += 1 } }
        }
    }

    /// 模拟核心分页：共 n 条，sortTs = i * 1000，按 before 游标返回更早的 limit 条（升序）
    private func pager(_ n: Int, _ rec: Recorder = Recorder(), delay: TimeInterval = 0)
        -> @Sendable (Int64?, UInt32) -> [MessageItem]? {
        let all = (0..<n).map { msg("m\($0)", ts: Int64($0) * 1_000) }
        return { before, limit in
            rec.record(before)
            if delay > 0 { Thread.sleep(forTimeInterval: delay) }
            let older = all.filter { before == nil || $0.sortTs < before! }
            return Array(older.suffix(Int(limit)))
        }
    }

    private func model(pages n: Int, _ rec: Recorder = Recorder(), delay: TimeInterval = 0) -> ChatModel {
        let m = ChatModel(convId: conv)
        let p = pager(n, rec, delay: delay)
        m.loadPage = { p($0, $1) }
        m.backgroundPager = { p }
        m.reload()
        return m
    }

    func testLocatePagesUpInBackgroundUntilFound() async {
        let rec = Recorder()
        let m = model(pages: 130, rec)
        XCTAssertEqual(m.messages.count, 40)
        let found = await m.locate("m5")
        XCTAssertTrue(found, "第 4 页的消息要翻页找到")
        XCTAssertEqual(rec.cursors, [nil, 90_000, 50_000, 10_000], "首屏 1 页 + 向上 3 页（游标为每页最早一条的 sortTs），找到就停")
        XCTAssertEqual(rec.mainThreadCalls, 1, "只有首屏在主线程读取，定位翻页都在后台线程")
        XCTAssertEqual(m.messages.map(\.id), (0..<130).map { "m\($0)" }, "不重不漏、顺序正确")
        XCTAssertFalse(m.hasMore, "最后一页不足一页：已到最早")
        XCTAssertFalse(m.locating)
        XCTAssertFalse(m.loadingOlder)
    }

    func testLocateFoundOnFullPageKeepsHasMore() async {
        let m = model(pages: 200)
        let found = await m.locate("m100")                  // 第 3 页（m80..m119）
        XCTAssertTrue(found)
        XCTAssertEqual(m.messages.first?.id, "m80")
        XCTAssertTrue(m.hasMore, "还有更早的消息，顶部可以继续上滑加载")
    }

    func testLocateAlreadyLoadedDoesNotPage() async {
        let rec = Recorder()
        let m = model(pages: 100, rec)
        let found = await m.locate("m99")
        XCTAssertTrue(found)
        XCTAssertEqual(rec.n, 1)
    }

    func testLocateNotFoundLeavesListUnchanged() async {
        let m = model(pages: 90)
        let before = m.messages.map(\.id)
        let found = await m.locate("gone")
        XCTAssertFalse(found, "已删除的消息：翻到最早仍找不到")
        XCTAssertEqual(m.messages.map(\.id), before, "找不到时不把翻过的页塞进列表，保持当前位置")
        XCTAssertFalse(m.locating)
    }

    func testLocateStopsAtMaxPages() async {
        let rec = Recorder()
        let m = model(pages: 1_000, rec)
        let found = await m.locate("m0", maxPages: 3)
        XCTAssertFalse(found)
        XCTAssertEqual(rec.n, 1 + 3)
        XCTAssertEqual(m.messages.count, 40)
    }

    func testLocateStopsWhenPageMakesNoProgress() async {
        let m = model([msg("a", ts: 1_000)])
        m.hasMore = true
        let stuck: @Sendable (Int64?, UInt32) -> [MessageItem]? = { _, _ in
            // 游标卡住：返回的最早一条不早于游标
            [MessageItem(id: "a", convId: "u:peer", senderId: "peer", isOutgoing: false, sentAt: 1_000, serverTs: 1_000,
                         sortTs: 1_000, status: .delivered, body: .text(text: "a"), quote: nil, reactions: [],
                         expireSecs: 0, expiresAt: nil, deleted: false, forwarded: false, mentions: [])]
        }
        m.backgroundPager = { stuck }
        let found = await m.locate("x")
        XCTAssertFalse(found)
    }

    /// 离开页面：取消定位任务 → 后台翻页尽快停止，返回 false，列表不变，加载状态复位
    func testLocateCancelledStopsAndLeavesListUnchanged() async {
        let rec = Recorder()
        let m = model(pages: 5_000, rec, delay: 0.02)
        let before = m.messages.map(\.id)
        let task = Task { await m.locate("m0") }
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertTrue(m.locating, "定位进行中（界面显示加载指示）")
        XCTAssertTrue(m.loadingOlder, "定位期间顶部的上滑加载不并发翻页")
        task.cancel()
        let found = await task.value
        XCTAssertFalse(found)
        XCTAssertEqual(m.messages.map(\.id), before)
        XCTAssertFalse(m.locating)
        XCTAssertFalse(m.loadingOlder)
        let pagesAtCancel = rec.n
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(rec.n, pagesAtCancel, "取消后不再翻页")
        XCTAssertLessThan(rec.n, 20, "远没有翻完 125 页就停止")
    }

    /// 定位在后台进行时主线程不被占用：期间主线程上的其他任务可以执行
    func testLocateDoesNotBlockMainThread() async {
        let m = model(pages: 2_000, delay: 0.005)
        let task = Task { await m.locate("m0") }
        var ticks = 0
        while m.locating || ticks == 0 {
            ticks += 1
            try? await Task.sleep(for: .milliseconds(10))
            if ticks > 1_000 { break }
        }
        let found = await task.value
        XCTAssertTrue(found)
        XCTAssertGreaterThan(ticks, 5, "翻页期间主线程仍在处理其他任务")
        XCTAssertEqual(m.messages.count, 2_000)
    }

    // MARK: - fix2 @提及光标

    func testPickMentionSetsCursorAfterFragment() {
        let m = ChatModel(convId: "g:grp")
        m.draft = "你好再见"
        m.draftChanged()
        m.draft = "你好@再见"
        m.draftChanged()
        XCTAssertEqual(m.mentionTrigger, 2)
        XCTAssertEqual(m.mentionQuery, "", "光标后的「再见」不是筛选词")
        XCTAssertNil(m.pendingCursor, "普通输入不改光标（不打断输入法）")
        m.pickMention("u1")
        XCTAssertTrue(m.draft.hasPrefix("你好@"))
        XCTAssertTrue(m.draft.hasSuffix(" 再见"), "后面的文字保留：\(m.draft)")
        let cursor = try! XCTUnwrap(m.pendingCursor)
        XCTAssertEqual(cursor, m.draft.utf16.count - 2, "光标在「@昵称 」之后、「再见」之前")
    }

    func testWholeMentionDeleteSetsCursorAtDeletionPoint() {
        let m = ChatModel(convId: "g:grp")
        m.draft = "@"
        m.draftChanged()
        m.pickMention("u1")
        let tag = m.draft                                     // "@昵称 "
        m.draft = tag + "你好"
        m.draftChanged()
        m.pendingCursor = nil
        // 退格删掉空格，再删到片段 → 整段删除，光标在句首
        m.draft = String(tag.dropLast()) + "你好"
        m.draftChanged()
        XCTAssertNil(m.pendingCursor)
        m.draft = String(tag.dropLast(2)) + "你好"
        m.draftChanged()
        XCTAssertEqual(m.draft, "你好")
        XCTAssertEqual(m.pendingCursor, 0)
    }
}
