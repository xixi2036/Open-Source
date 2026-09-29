import XCTest
@testable import Hush

@MainActor
final class MomentsModelTests: XCTestCase {
    private func profile(_ id: String) -> Profile { Profile(id: id, name: id, about: "", avatarId: nil, updatedAt: 0, email: nil) }
    private func moment(_ id: String, at ts: Int64, comments: Int = 0) -> Moment {
        Moment(id: id, author: profile("a"), text: id, media: [], visibility: nil, likes: [],
               comments: (0..<comments).map { Comment(id: "\(id)c\($0)", author: profile("b"), replyTo: nil, text: "c", createdAt: ts) },
               createdAt: ts)
    }

    func testMergeKeepsLoadedPagesAndUpdatesInPlace() {
        let existing = (0..<30).map { moment("m\($0)", at: Int64(1000 - $0)) }
        let latest = [moment("new", at: 2000), moment("m0", at: 1000, comments: 1), moment("m1", at: 999)]
        let merged = MomentsModel.merge(existing: existing, latest: latest)
        XCTAssertEqual(merged.count, 31, "已加载的后续页不能被丢弃")
        XCTAssertEqual(merged.first?.id, "new")
        XCTAssertEqual(merged[1].comments.count, 1, "机器人的回复评论要合并进来")
        XCTAssertEqual(merged.last?.id, "m29")
    }

    func testMergeWithNothingNew() {
        let existing = [moment("a", at: 2), moment("b", at: 1)]
        XCTAssertEqual(MomentsModel.merge(existing: existing, latest: []).map(\.id), ["a", "b"])
    }
}
