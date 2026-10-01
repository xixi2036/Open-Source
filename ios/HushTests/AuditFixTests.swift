import XCTest
import AVFoundation
@testable import Hush

/// 2026-09-30 审查修复涉及的纯逻辑
@MainActor
final class AuditFixTests: XCTestCase {
    private func msg(_ id: String, ts: Int64, out: Bool = false, body: MessageBody? = nil, status: MessageStatus? = nil) -> MessageItem {
        MessageItem(id: id, convId: "u:peer", senderId: out ? "me" : "peer", isOutgoing: out, sentAt: ts, serverTs: ts, sortTs: ts,
                    status: status ?? (out ? .sent : .delivered), body: body ?? .text(text: id), quote: nil, reactions: [],
                    expireSecs: 0, expiresAt: nil, deleted: false, forwarded: false, mentions: [])
    }

    // MARK: - A1 / A7 顶层窗口

    func testOverlayModePriority() {
        func mode(ready: Bool = true, locked: Bool = false, cover: Bool = false, call: Bool = false, mini: Bool = false) -> OverlayMode {
            OverlayWindows.mode(ready: ready, locked: locked, cover: cover, callPresented: call, callMinimized: mini)
        }
        XCTAssertEqual(mode(), .none)
        XCTAssertEqual(mode(call: true), .call)
        XCTAssertEqual(mode(call: true, mini: true), .pill)
        XCTAssertEqual(mode(locked: true, call: true), .lock, "屏幕锁盖住通话页")
        XCTAssertEqual(mode(cover: true, call: true), .privacy, "应用切换器快照不能露出通话页")
        XCTAssertEqual(mode(ready: false, locked: true), .none, "未登录时不显示锁定页")
        XCTAssertEqual(mode(ready: false, cover: true), .privacy)
        XCTAssertEqual(mode(ready: false, call: true), .none)
    }

    func testCallToneFollowsPhase() {
        XCTAssertEqual(CallTones.tone(phase: .incoming, isCaller: false, offerSent: false), .ring)
        XCTAssertNil(CallTones.tone(phase: .outgoing, isCaller: true, offerSent: false), "offer 发出前音频会话还没配置好")
        XCTAssertEqual(CallTones.tone(phase: .outgoing, isCaller: true, offerSent: true), .ringback)
        for p in [CallPhase.idle, .connecting, .active, .ended("x")] {
            XCTAssertNil(CallTones.tone(phase: p, isCaller: true, offerSent: true), "\(p)")
        }
    }

    func testSynthesizedTonesArePlayableWAV() throws {
        for data in [CallTones.ringWAV, CallTones.ringbackWAV] {
            XCTAssertEqual(String(decoding: data.prefix(4), as: UTF8.self), "RIFF")
            let p = try AVAudioPlayer(data: data)
            XCTAssertGreaterThan(p.duration, 2.5)
        }
        let one = CallTones.wav(segments: [(freqs: [440], secs: 0.5)], sampleRate: 8000)
        XCTAssertEqual(one.count, 44 + 4000 * 2)
    }

    func testIncomingCallStartsRingAndRejectStopsIt() {
        let m = CallManager()
        m.handleSignal(from: "alice", signal: CallSignal(callId: "c1", action: .offer, video: false, sdp: "v=0\r\n", candidates: []))
        XCTAssertEqual(m.phase, .incoming)
        XCTAssertTrue(m.isPresented)
        m.reject()
        XCTAssertEqual(m.phase, .ended("已拒绝"))
        XCTAssertFalse(m.minimized)
    }

    // MARK: - A4 权限

    func testPermissionBlock() {
        XCTAssertNil(CallManager.permissionBlock(mic: true, camera: true, video: true))
        XCTAssertNil(CallManager.permissionBlock(mic: true, camera: false, video: false), "语音通话不需要摄像头")
        XCTAssertEqual(CallManager.permissionBlock(mic: false, camera: true, video: false), .microphone)
        XCTAssertEqual(CallManager.permissionBlock(mic: true, camera: false, video: true), .camera)
        XCTAssertEqual(CallManager.permissionBlock(mic: false, camera: false, video: true), .microphone)
    }

    func testCallAbortsWhenMicrophoneDenied() async {
        let m = CallManager()
        m.permissionRequest = { _ in (mic: false, camera: true) }
        m.startCall(to: "bob", video: false)
        XCTAssertEqual(m.phase, .outgoing)
        for _ in 0..<50 where m.blocked == nil { try? await Task.sleep(for: .milliseconds(20)) }
        XCTAssertEqual(m.blocked, .microphone)
        XCTAssertEqual(m.phase, .ended("未获得麦克风权限"))
        XCTAssertTrue(m.isPresented, "说明页保持显示，直到用户关闭")
        m.blocked = nil
        m.phase = .idle
        XCTAssertFalse(m.isPresented)
    }

    func testReconnectingOnlyShownForConnectedCalls() {
        XCTAssertTrue(CallManager.shouldShowReconnecting(phase: .active))
        XCTAssertFalse(CallManager.shouldShowReconnecting(phase: .connecting))
    }

    // MARK: - A3 是否停在底部

    func testPinnedLogic() {
        func pinned(_ cur: Bool, _ d: CGFloat?, user: Bool = false, layout: Bool = false, prog: Bool = false) -> Bool {
            ChatScrollTracker.pinned(current: cur, distance: d, userScrolling: user, layoutChanged: layout, programmatic: prog)
        }
        XCTAssertTrue(pinned(false, 0), "锚点在屏幕内 → 在底部")
        XCTAssertTrue(pinned(false, 40, user: true))
        XCTAssertTrue(pinned(true, 900, layout: true), "新消息把锚点顶出屏幕不算离开底部（A3 的根因）")
        XCTAssertTrue(pinned(true, nil, layout: true), "连发时锚点暂时没有渲染")
        XCTAssertTrue(pinned(true, 300, prog: true), "程序滚动还没结束")
        XCTAssertFalse(pinned(false, 900, layout: true), "本来就在看历史，内容变化不会变成在底部")
        XCTAssertFalse(pinned(true, 900, user: true, layout: true), "用户自己滚走了，即使同时有新消息")
        XCTAssertFalse(pinned(true, 900), "位置变了但布局没变 → 离开底部")
        XCTAssertFalse(pinned(true, nil))
    }

    func testMarkReadOnlyWhenAtBottom() {
        let m = ChatModel(convId: "u:peer")
        m.messages = [msg("a", ts: 1)]
        m.setAtBottom(false)
        m.handle(.messageAdded(convId: "u:peer", message: msg("b", ts: 2), backlog: false, mentionsMe: false))
        XCTAssertEqual(m.unseenCount, 1)
        XCTAssertFalse(m.atBottom)
        m.setAtBottom(true)
        XCTAssertEqual(m.unseenCount, 0)
    }

    // MARK: - 第一条未读

    func testFirstUnreadIndex() {
        let list = [msg("a", ts: 1), msg("b", ts: 2, out: true), msg("c", ts: 3),
                    msg("s", ts: 4, body: .system(text: "x")), msg("d", ts: 5)]
        XCTAssertNil(ChatModel.firstUnreadIndex(list, unread: 0))
        XCTAssertEqual(ChatModel.firstUnreadIndex(list, unread: 1), 4)
        XCTAssertEqual(ChatModel.firstUnreadIndex(list, unread: 2), 2, "自己发的和系统提示不计入未读")
        XCTAssertEqual(ChatModel.firstUnreadIndex(list, unread: 3), 0)
        XCTAssertNil(ChatModel.firstUnreadIndex(list, unread: 4), "已加载的不够数：需要继续向上翻页")
    }

    func testInitialLoadFindsFirstUnreadAcrossPages() {
        // 无法注入未读数（来自核心的会话摘要），这里验证翻页辅助：loadRange
        let all = (1...100).map { msg("m\($0)", ts: Int64($0)) }
        let pager: (Int64?, UInt32) -> [MessageItem]? = { before, limit in
            let upTo = all.filter { $0.sortTs < (before ?? .max) }
            return Array(upTo.suffix(Int(limit)))
        }
        let r = ChatModel.loadRange(downTo: 35, pager: pager, pageSize: 20, maxPages: 10)
        XCTAssertEqual(r?.first?.id, "m35")
        XCTAssertEqual(r?.last?.id, "m100")
        XCTAssertEqual(r?.count, 66)
        XCTAssertNil(ChatModel.loadRange(downTo: 1, pager: { _, _ in nil }, pageSize: 20, maxPages: 3), "读取失败不替换列表")
    }

    // MARK: - C2 日期分隔

    func testDayBreakerMatchesCalendar() {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "Asia/Shanghai")!
        func ts(_ d: Int, _ h: Int) -> Int64 {
            Int64(cal.date(from: DateComponents(year: 2026, month: 9, day: d, hour: h))!.timeIntervalSince1970 * 1000)
        }
        let list = [msg("a", ts: ts(1, 9)), msg("b", ts: ts(1, 23)), msg("c", ts: ts(2, 0)), msg("d", ts: ts(2, 12)), msg("e", ts: ts(5, 1))]
        XCTAssertEqual(DayBreaker.dayStartIds(list, calendar: cal), ["a", "c", "e"])
        XCTAssertTrue(DayBreaker.dayStartIds([], calendar: cal).isEmpty)
    }

    func testMessageRowEquatableIgnoresClosures() {
        let m = msg("a", ts: 1)
        func row(_ m: MessageItem, selecting: Bool = false, sel: Bool = false) -> MessageRow {
            MessageRow(message: m, isGroup: false, selecting: selecting, isSelected: sel,
                       onLongPress: {}, onQuoteTap: { _ in }, onOpenImage: { _ in }, onShare: { _ in }, onResend: {})
        }
        XCTAssertTrue(row(m) == row(m))
        XCTAssertFalse(row(m) == row(m, selecting: true))
        XCTAssertFalse(row(m, selecting: true) == row(m, selecting: true, sel: true))
        var changed = m
        changed.status = .read
        XCTAssertFalse(row(m) == row(changed))
    }

    // MARK: - A10 / B8 通知

    func testCallLogBannerOnlyWhenMissed() {
        func banner(_ reason: CallEndReason, outgoing: Bool = false) -> Bool {
            NotificationThrottle.shouldBanner(backlog: false, notificationsOn: true, viewingConversation: false, muted: false,
                                              body: .callLog(video: false, durationSecs: 0, missed: reason == .missed, outgoing: outgoing, reason: reason))
        }
        XCTAssertTrue(banner(.missed))
        XCTAssertFalse(banner(.rejected), "自己拒接的来电不提示")
        XCTAssertFalse(banner(.completed))
        XCTAssertFalse(banner(.busy, outgoing: true))
        XCTAssertFalse(banner(.cancelled, outgoing: true))
        XCTAssertFalse(banner(.missed, outgoing: true))
    }

    func testBannerText() {
        let log = MessageBody.callLog(video: false, durationSecs: 0, missed: true, outgoing: false, reason: .missed)
        XCTAssertEqual(NotificationThrottle.bannerText(title: "小美", body: log, mentionsMe: false), "小美 的未接来电")
        XCTAssertEqual(NotificationThrottle.bannerText(title: "小美", body: .text(text: "hi"), mentionsMe: false), "来自 小美 的新消息")
        XCTAssertEqual(NotificationThrottle.bannerText(title: "群", body: .text(text: "hi"), mentionsMe: true), "群 中有人@了你")
        XCTAssertEqual(NotificationThrottle.bannerText(title: "", body: .text(text: "hi"), mentionsMe: false), "你有新消息")
        XCTAssertEqual(NotificationThrottle.callIdentifier("c1"), "call-c1")
    }

    // MARK: - B2 屏幕锁

    func testLockGrace() {
        XCTAssertFalse(ScreenLock.shouldLock(enabled: false, graceSecs: 0, elapsed: 100))
        XCTAssertTrue(ScreenLock.shouldLock(enabled: true, graceSecs: 0, elapsed: 1))
        XCTAssertFalse(ScreenLock.shouldLock(enabled: true, graceSecs: 60, elapsed: 59))
        XCTAssertTrue(ScreenLock.shouldLock(enabled: true, graceSecs: 60, elapsed: 60))
        XCTAssertTrue(ScreenLock.shouldLock(enabled: true, graceSecs: 300, elapsed: 301))
        XCTAssertFalse(ScreenLock.shouldLock(enabled: true, graceSecs: 60, elapsed: nil), "没进过后台（Face ID 弹窗造成的 inactive）不上锁")
    }

    func testCoverVisible() {
        XCTAssertFalse(ScreenLock.coverVisible(sceneActive: true, privacyScreen: true, lockEnabled: true))
        XCTAssertTrue(ScreenLock.coverVisible(sceneActive: false, privacyScreen: true, lockEnabled: false))
        XCTAssertTrue(ScreenLock.coverVisible(sceneActive: false, privacyScreen: false, lockEnabled: true), "宽限期内未上锁，快照也不能露出内容")
        XCTAssertFalse(ScreenLock.coverVisible(sceneActive: false, privacyScreen: false, lockEnabled: false))
    }

    func testFailedAuthenticationSuppressesAutoPrompt() {
        let lock = ScreenLock()
        lock.lockEnabled = true
        defer { lock.lockEnabled = false }
        lock.locked = true
        lock.finishAuthentication(false)
        XCTAssertTrue(lock.locked)
        XCTAssertTrue(lock.autoPromptSuppressed, "验证失败 / 取消后不再自动弹出，避免循环")
        lock.onBackground(now: Date())
        XCTAssertFalse(lock.autoPromptSuppressed, "再次从后台回来可以自动验证一次")
        lock.finishAuthentication(true)
        XCTAssertFalse(lock.locked)
    }

    // MARK: - B5 下载失败

    func testDownloadFailureClassification() {
        XCTAssertEqual(DownloadFailure.classify(HushError.Server(code: "not_found", message: "")), .expired)
        XCTAssertEqual(DownloadFailure.classify(HushError.NotFound(message: "附件")), .expired)
        XCTAssertEqual(DownloadFailure.classify(HushError.Network(message: "timeout")), .failed)
        XCTAssertEqual(DownloadFailure.classify(HushError.Server(code: "http_500", message: "")), .failed)
        XCTAssertEqual(DownloadFailure.expired.text, "附件已过期")
    }

    // MARK: - 自动重发

    func testAutoResendQueueKeepsOrderAndRetriesOnce() {
        var q = AutoResendQueue()
        q.note(id: "b", sortTs: 200)
        q.note(id: "a", sortTs: 100)
        q.note(id: "c", sortTs: 200)
        q.note(id: "a", sortTs: 100)
        XCTAssertEqual(q.takePending(), ["a", "b", "c"], "按原来的先后顺序")
        XCTAssertEqual(q.takePending(), [])
        q.note(id: "a", sortTs: 100)
        XCTAssertEqual(q.pendingCount, 0, "每条只自动重发一次")
        q.note(id: "d", sortTs: 300)
        q.markAttempted("d")
        XCTAssertEqual(q.takePending(), [], "手动重发过的不再自动重发")
    }

    // MARK: - B10 钥匙串

    private final class Backend: DBKeyBackend {
        var readError: OSStatus? = -34018
        var file: Data?
        func readKeychain() -> KeychainRead { readError.map { .error($0) } ?? .notFound }
        func writeKeychain(_ data: Data) -> OSStatus { errSecSuccess }
        func deleteKeychain() {}
        func readFile() -> Data? { file }
        func writeFile(_ data: Data) -> Bool { file = data; return true }
        func deleteFile() { file = nil }
        func randomKey() -> Result<Data, KeyStoreError> { .success(Data(repeating: 9, count: 32)) }
    }

    func testKeychainErrorWithExistingDatabaseNeverCreatesNewKey() {
        let b = Backend()
        XCTAssertThrowsError(try KeyStore.resolve(b, allowFileFallback: true, databaseExists: true)) { e in
            XCTAssertEqual(e as? KeyStoreError, .keychain(-34018))
        }
        XCTAssertNil(b.file, "不能写入新密钥")
        // 首次启动（没有库）仍可在模拟器上退回文件
        XCTAssertEqual(try KeyStore.resolve(b, allowFileFallback: true, databaseExists: false).count, 32)
        // 已有文件副本：照常使用
        XCTAssertEqual(try KeyStore.resolve(b, allowFileFallback: true, databaseExists: true), b.file)
    }

    // MARK: - A2 备份排除

    func testProtectDataDirExcludesFromBackup() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("protect-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: dir.appendingPathComponent("files"), withIntermediateDirectories: true)
        try Data("x".utf8).write(to: dir.appendingPathComponent("hush.sqlite"))
        try Data("y".utf8).write(to: dir.appendingPathComponent("files/a.jpg"))
        defer { try? FileManager.default.removeItem(at: dir) }
        AppPaths.protectDataDir(dir)
        XCTAssertEqual(try dir.resourceValues(forKeys: [.isExcludedFromBackupKey]).isExcludedFromBackup, true)
    }

    // MARK: - B11 服务器地址

    func testServerAddressCheck() {
        XCTAssertEqual(ServerAddress.check("https://chat.example.com"), .ok)
        XCTAssertEqual(ServerAddress.check(" http://127.0.0.1:8080 "), .ok)
        XCTAssertEqual(ServerAddress.check("http://localhost:8080"), .ok)
        XCTAssertEqual(ServerAddress.check("http://192.168.1.20:8080"), .ok)
        XCTAssertEqual(ServerAddress.check("http://10.0.0.5"), .ok)
        XCTAssertEqual(ServerAddress.check("http://172.20.1.1"), .ok)
        XCTAssertEqual(ServerAddress.check("http://nas.local:8080"), .ok)
        XCTAssertEqual(ServerAddress.check("http://[::1]:8080"), .ok)
        XCTAssertEqual(ServerAddress.check("http://[fd12:3456::1]:8080"), .ok)
        XCTAssertEqual(ServerAddress.check("http://chat.example.com"), .insecure(host: "chat.example.com"))
        XCTAssertEqual(ServerAddress.check("http://8.8.8.8:8080"), .insecure(host: "8.8.8.8"))
        XCTAssertEqual(ServerAddress.check("http://172.32.0.1"), .insecure(host: "172.32.0.1"))
        XCTAssertEqual(ServerAddress.check("http://[2001:db8::1]"), .insecure(host: "2001:db8::1"))
        XCTAssertEqual(ServerAddress.check("ftp://x"), .invalid)
        XCTAssertEqual(ServerAddress.check("chat.example.com"), .invalid)
        XCTAssertEqual(ServerAddress.check(""), .invalid)
        XCTAssertEqual(ServerAddress.check("http://"), .invalid)
    }

    // MARK: - 链接

    func testLinkDetectionAndPolicy() {
        let text = "官网 https://example.com/a?b=1 ，内网 http://192.168.1.10:8080/x 结束"
        let links = LinkText.links(in: text)
        XCTAssertEqual(links.map(\.url.absoluteString), ["https://example.com/a?b=1", "http://192.168.1.10:8080/x"])
        let pieces = LinkText.pieces(text)
        XCTAssertEqual(pieces.map(\.text).joined(), text, "切分后拼回原文")
        XCTAssertEqual(pieces.filter { $0.url != nil }.count, 2)
        XCTAssertTrue(LinkText.links(in: "没有链接的普通文字").isEmpty)
        XCTAssertEqual(LinkPolicy.decision(URL(string: "https://a.b")!), .open)
        XCTAssertEqual(LinkPolicy.decision(URL(string: "http://a.b")!), .confirm)
        XCTAssertEqual(LinkPolicy.decision(URL(string: "tel:10086")!), .block)
        XCTAssertEqual(LinkPolicy.decision(URL(string: "hush://x")!), .block)
    }

    func testAttributedTextKeepsContentWithMentionsAndLinks() {
        let text = "@小美 看 https://example.com 😀"
        let mention = Mention(userId: "u1", start: 0, length: 3)
        let a = LinkText.attributed(text, [mention], outgoing: false)
        XCTAssertEqual(String(a.characters), text)
        XCTAssertEqual(a.runs.compactMap(\.link).map(\.absoluteString), ["https://example.com"])
    }

    // MARK: - 多选 / 转发

    func testSelectableAndForwardable() {
        XCTAssertTrue(ChatModel.selectable(msg("a", ts: 1)))
        XCTAssertFalse(ChatModel.selectable(msg("s", ts: 1, body: .system(text: "x"))))
        XCTAssertTrue(ChatModel.forwardable(msg("a", ts: 1)))
        XCTAssertFalse(ChatModel.forwardable(msg("u", ts: 1, body: .undecryptable)))
        let att = MessageBody.attachments(items: [], caption: nil)
        XCTAssertFalse(ChatModel.forwardable(msg("f", ts: 1, out: true, body: att, status: .failed)))
        XCTAssertTrue(ChatModel.forwardable(msg("f", ts: 1, out: true, body: att, status: .sent)))
        var deleted = msg("d", ts: 1)
        deleted.deleted = true
        XCTAssertFalse(ChatModel.forwardable(deleted))
    }

    func testSelectionToggle() {
        let m = ChatModel(convId: "u:peer")
        let a = msg("a", ts: 1), b = msg("b", ts: 2), s = msg("s", ts: 3, body: .system(text: "x"))
        m.messages = [a, b, s]
        m.beginSelecting(b)
        XCTAssertTrue(m.selecting)
        m.toggleSelected(a)
        m.toggleSelected(s)
        XCTAssertEqual(m.selectedMessages.map(\.id), ["a", "b"], "按时间顺序，系统消息不可选")
        m.toggleSelected(b)
        XCTAssertEqual(m.selected, ["a"])
        m.handle(.messageRemoved(convId: "u:peer", messageId: "a"))
        XCTAssertTrue(m.selected.isEmpty)
        m.endSelecting()
        XCTAssertFalse(m.selecting)
    }

    // MARK: - 视频 / 图片

    func testVideoMeta() {
        XCTAssertNil(VideoMeta.sizeError(10, max: 100))
        XCTAssertEqual(VideoMeta.sizeError(101, max: 100), .tooLarge(101))
        XCTAssertEqual(VideoMeta.sizeError(0, max: 100), .unreadable)
        let portrait = VideoMeta.displaySize(natural: CGSize(width: 1920, height: 1080), transform: CGAffineTransform(rotationAngle: .pi / 2))
        XCTAssertEqual(portrait, CGSize(width: 1080, height: 1920))
        XCTAssertTrue(VideoMeta.tooLargeText(150 * 1024 * 1024).contains("100MB"))
    }

    func testCompressDownsamplesWithImageIO() throws {
        let big = UIGraphicsImageRenderer(size: CGSize(width: 3000, height: 1500), format: {
            let f = UIGraphicsImageRendererFormat(); f.scale = 1; return f
        }()).jpegData(withCompressionQuality: 0.7) { ctx in
            UIColor.orange.setFill(); ctx.fill(CGRect(x: 0, y: 0, width: 3000, height: 1500))
        }
        let r = try XCTUnwrap(ImageUtil.compressToJPEG(big, maxSide: 2048))
        defer { try? FileManager.default.removeItem(at: r.0) }
        XCTAssertEqual(r.1, 2048)
        XCTAssertEqual(r.2, 1024)
        XCTAssertNotNil(ImageUtil.thumbnail(r.0))
        let small = try XCTUnwrap(ImageUtil.compressToJPEG(big, maxSide: 4096))
        defer { try? FileManager.default.removeItem(at: small.0) }
        XCTAssertEqual(small.1, 3000, "不放大")
        XCTAssertNil(ImageUtil.compressToJPEG(Data("not an image".utf8)))
    }

    func testImageViewerLoadsOnlyNeighbours() {
        XCTAssertTrue(ImageViewer.shouldLoad(page: 5, current: 5))
        XCTAssertTrue(ImageViewer.shouldLoad(page: 4, current: 5))
        XCTAssertTrue(ImageViewer.shouldLoad(page: 6, current: 5))
        XCTAssertFalse(ImageViewer.shouldLoad(page: 7, current: 5))
        XCTAssertFalse(ImageViewer.shouldLoad(page: 0, current: 5))
    }

    // MARK: - C7 / 朋友圈

    func testAttachmentFileExistenceIsCached() throws {
        AttachmentFiles.reset()
        let f = FileManager.default.temporaryDirectory.appendingPathComponent("att-\(UUID().uuidString)")
        try Data("x".utf8).write(to: f)
        XCTAssertTrue(AttachmentFiles.exists(f.path, now: 100))
        try FileManager.default.removeItem(at: f)
        XCTAssertTrue(AttachmentFiles.exists(f.path, now: 105), "10 秒内用缓存结果，不再访问文件系统")
        XCTAssertFalse(AttachmentFiles.exists(f.path, fresh: true, now: 105), "用户点击时重新检查")
        XCTAssertFalse(AttachmentFiles.exists(f.path, now: 106))
    }

    func testMomentsAutoRefreshThrottle() {
        let t0 = Date()
        XCTAssertTrue(MomentsModel.shouldAutoRefresh(last: .distantPast, now: t0, minInterval: 15))
        XCTAssertFalse(MomentsModel.shouldAutoRefresh(last: t0, now: t0.addingTimeInterval(5), minInterval: 15))
        XCTAssertTrue(MomentsModel.shouldAutoRefresh(last: t0, now: t0.addingTimeInterval(15), minInterval: 15))
    }

    func testCameraAuthorizationMapping() {
        XCTAssertEqual(CameraPicker.authorization(.authorized), .granted)
        XCTAssertEqual(CameraPicker.authorization(.notDetermined), .undetermined)
        XCTAssertEqual(CameraPicker.authorization(.denied), .denied)
        XCTAssertEqual(CameraPicker.authorization(.restricted), .denied)
    }

    func testVoicePlayerCategoryForRoute() {
        XCTAssertEqual(VoicePlayer.category(earpiece: false), .playback)
        XCTAssertEqual(VoicePlayer.category(earpiece: true), .playAndRecord)
    }
}
