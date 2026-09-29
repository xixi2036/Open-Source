import XCTest
@testable import Hush

/// U3 草稿 / U4 有人@我 的列表预览文案；U2 转发目标
final class PreviewAndForwardTests: XCTestCase {
    func testListLinePriority() {
        typealias L = MessagePreview.ListLine
        XCTAssertEqual(MessagePreview.listLine(typing: false, unreadMentions: 0, draft: nil, message: "hi"), L(prefix: nil, body: "hi"))
        XCTAssertEqual(MessagePreview.listLine(typing: false, unreadMentions: 0, draft: "写到一半\n第二行", message: "hi"),
                       L(prefix: "[草稿]", body: "写到一半 第二行"))
        XCTAssertEqual(MessagePreview.listLine(typing: false, unreadMentions: 0, draft: "  \n", message: "hi"),
                       L(prefix: nil, body: "hi"), "空白草稿不显示")
        XCTAssertEqual(MessagePreview.listLine(typing: false, unreadMentions: 2, draft: "草稿", message: "小美：@我 在吗"),
                       L(prefix: "[有人@我]", body: "小美：@我 在吗"), "未读 @ 优先于草稿")
        XCTAssertEqual(MessagePreview.listLine(typing: true, unreadMentions: 2, draft: "草稿", message: "x"),
                       L(prefix: nil, body: "正在输入…"))
    }

    private func conv(_ id: String, active: Bool = true) -> ConversationSummary {
        ConversationSummary(id: id, kind: id.hasPrefix("g:") ? .group : .direct, peerUserId: id.hasPrefix("u:") ? String(id.dropFirst(2)) : nil,
                            groupId: id.hasPrefix("g:") ? String(id.dropFirst(2)) : nil, title: id, avatarId: nil, lastMessage: nil,
                            lastActivityTs: 0, unreadCount: 0, pinned: false, muted: false, archived: false, disappearingSecs: 0,
                            active: active, draft: nil, unreadMentions: 0)
    }

    private func friend(_ id: String, _ name: String) -> Profile {
        Profile(id: id, name: name, about: "", avatarId: nil, updatedAt: 0, email: nil)
    }

    func testForwardTargetsRecentThenFriendsWithoutDuplicates() {
        let list = ForwardTargets.build(
            conversations: [conv("g:team"), conv("u:amy"), conv("g:left", active: false)],
            friends: [friend("amy", "Amy"), friend("bob", "Bob")],
            title: { $0 == "g:team" ? "项目组" : $0 == "u:amy" ? "Amy" : $0 },
            directId: { "u:\($0)" })
        XCTAssertEqual(list.map(\.id), ["g:team", "u:amy", "u:bob"], "已退出的群不能转发；已有会话的好友不重复")
        XCTAssertEqual(ForwardTargets.filter(list, query: " 项目 ").map(\.id), ["g:team"])
        XCTAssertEqual(ForwardTargets.filter(list, query: "bo").map(\.id), ["u:bob"], "搜索不区分大小写")
        XCTAssertEqual(ForwardTargets.filter(list, query: "").count, 3)
    }
}
