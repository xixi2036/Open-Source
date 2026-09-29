import XCTest
@testable import Hush

@MainActor
final class MessageTextTests: XCTestCase {
    private func item(_ body: MessageBody, out: Bool = false, deleted: Bool = false) -> MessageItem {
        MessageItem(id: "m", convId: "u:p", senderId: out ? "me" : "p", isOutgoing: out, sentAt: 0, serverTs: 0, sortTs: 0,
                    status: .sent, body: body, quote: nil, reactions: [], expireSecs: 0, expiresAt: nil, deleted: deleted,
                    forwarded: false, mentions: [])
    }

    /// P-B：按核心给出的结束原因显示
    func testCallLogTextByReason() {
        XCTAssertEqual(MessagePreview.callLogText(video: false, secs: 201, reason: .completed), "语音通话 3:21")
        XCTAssertEqual(MessagePreview.callLogText(video: true, secs: 201, reason: .completed), "视频通话 3:21")
        XCTAssertEqual(MessagePreview.callLogText(video: false, secs: 0, reason: .missed), "未接来电")
        XCTAssertEqual(MessagePreview.callLogText(video: true, secs: 0, reason: .rejected), "已拒绝")
        XCTAssertEqual(MessagePreview.callLogText(video: false, secs: 0, reason: .busy), "对方忙线")
        XCTAssertEqual(MessagePreview.callLogText(video: false, secs: 0, reason: .cancelled), "已取消")
        XCTAssertEqual(MessagePreview.callLogText(video: true, secs: 0, reason: .failed), "通话失败")
    }

    /// 会话列表预览没有图标：未接通的短语前补通话类型
    func testCallLogPreviewInList() {
        let call = { (video: Bool, secs: UInt32, reason: CallEndReason) in
            MessagePreview.text(self.item(.callLog(video: video, durationSecs: secs, missed: reason == .missed,
                                                   outgoing: true, reason: reason)))
        }
        XCTAssertEqual(call(false, 0, .cancelled), "[语音通话 已取消]")
        XCTAssertEqual(call(true, 0, .busy), "[视频通话 对方忙线]")
        XCTAssertEqual(call(false, 0, .missed), "[未接来电]")
        XCTAssertEqual(call(true, 65, .completed), "[视频通话 1:05]")
    }

    func testRecallText() {
        XCTAssertEqual(MessagePreview.recallText(item(.text(text: "x"), out: true, deleted: true), inGroup: true), "你撤回了一条消息")
        XCTAssertEqual(MessagePreview.recallText(item(.text(text: "x"), deleted: true), inGroup: false), "对方撤回了一条消息")
        XCTAssertEqual(MessagePreview.text(item(.text(text: "x"), deleted: true)), "对方撤回了一条消息")
    }
}
