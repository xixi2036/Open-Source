import XCTest
@testable import Hush

/// U4：@提及输入（UTF-16 区间）与气泡渲染
final class MentionTests: XCTestCase {
    private func typed(_ c: inout MentionComposer, _ text: String) -> (text: String, trigger: Int?) {
        c.textChanged(to: text)
    }

    func testTypingAtReportsTriggerOffsetInUTF16() {
        var c = MentionComposer()
        _ = typed(&c, "你好😀")                        // 2 + 2（代理对）
        XCTAssertNil(typed(&c, "你好😀 ").trigger)
        XCTAssertEqual(typed(&c, "你好😀 @").trigger, 5)
        XCTAssertNil(typed(&c, "你好😀 @x").trigger, "只有刚输入的 @ 才触发")
        var d = MentionComposer()
        XCTAssertEqual(typed(&d, "＠").trigger, 0, "全角 ＠ 也触发")
        XCTAssertNil(typed(&d, "＠ab@").trigger, "粘贴多个字符不算单独输入 @")
    }

    func testInsertMentionRecordsUTF16RangeWithoutTrailingSpace() {
        var c = MentionComposer()
        _ = typed(&c, "hi 😀@")
        let text = c.insertMention(userId: "u1", name: "小美🌸", replacing: 5, 6)
        XCTAssertEqual(text, "hi 😀@小美🌸 ")
        XCTAssertEqual(c.spans, [MentionSpan(userId: "u1", start: 5, length: 5)])   // @ + 小美 + 🌸(2)
        let p = c.payload()
        XCTAssertEqual(p.text, "hi 😀@小美🌸")
        XCTAssertEqual(p.mentions, [Mention(userId: "u1", start: 5, length: 5)])
        // 与核心一致：按 UTF-16 切出来正好是「@小美🌸」
        let u = Array(p.text.utf16)
        XCTAssertEqual(String(decoding: u[5..<10], as: UTF16.self), "@小美🌸")
    }

    func testInsertMentionReplacesFilterQuery() {
        var c = MentionComposer()
        _ = typed(&c, "@")
        _ = typed(&c, "@小")
        XCTAssertEqual(c.query(after: 0), "小")
        let text = c.insertMention(userId: "u1", name: "小美", replacing: 0, 2)
        XCTAssertEqual(text, "@小美 ")
        XCTAssertEqual(c.spans, [MentionSpan(userId: "u1", start: 0, length: 3)])
        XCTAssertNil(c.query(after: 0), "插入「@小美 」后筛选结束")
    }

    func testQueryClosesOnWhitespaceOrDeletedAt() {
        var c = MentionComposer()
        _ = typed(&c, "a@bo")
        XCTAssertEqual(c.query(after: 1), "bo")
        _ = typed(&c, "a@bo ")
        XCTAssertNil(c.query(after: 1))
        _ = typed(&c, "a")
        XCTAssertNil(c.query(after: 1))
    }

    func testBackspaceIntoMentionDeletesWholeFragment() {
        var c = MentionComposer()
        _ = typed(&c, "@")
        _ = c.insertMention(userId: "u1", name: "小美", replacing: 0, 1)
        _ = typed(&c, "@小美 你好")
        XCTAssertEqual(c.spans.count, 1)
        // 光标在「你好」前退格两次：先删空格，再删到提及片段 → 整段删除
        XCTAssertEqual(typed(&c, "@小美你好").text, "@小美你好")
        XCTAssertEqual(c.spans.count, 1)
        let r = typed(&c, "@小你好")
        XCTAssertEqual(r.text, "你好")
        XCTAssertEqual(c.text, "你好")
        XCTAssertTrue(c.spans.isEmpty)
    }

    func testEditsBeforeShiftAndInsideInvalidate() {
        var c = MentionComposer()
        _ = typed(&c, "hi @")
        _ = c.insertMention(userId: "u1", name: "Bob", replacing: 3, 4)       // "hi @Bob "
        _ = typed(&c, "hi @Bob and @")
        _ = c.insertMention(userId: "u2", name: "李雷", replacing: 12, 13)    // "hi @Bob and @李雷 "
        XCTAssertEqual(c.spans.map(\.start), [3, 12])
        // 在最前面插入 emoji（2 个码元）→ 两个区间都后移 2
        _ = typed(&c, "😀hi @Bob and @李雷 ")
        XCTAssertEqual(c.spans.map(\.start), [5, 14])
        XCTAssertEqual(c.spans.map(\.length), [4, 3])
        // 在 @Bob 中间插入字符 → 该提及失效，文字保留；后面的继续平移
        _ = typed(&c, "😀hi @Boxb and @李雷 ")
        XCTAssertEqual(c.spans, [MentionSpan(userId: "u2", start: 15, length: 3)])
        // 选中删除跨越提及的一段 → 提及整段删除
        let r = typed(&c, "😀hi @Boxb 雷 ")
        XCTAssertEqual(r.text, "😀hi @Boxb  ")
        XCTAssertTrue(c.spans.isEmpty)
    }

    func testPayloadTrimsAndShiftsRanges() {
        var c = MentionComposer()
        _ = typed(&c, "\n  @")
        _ = c.insertMention(userId: "u1", name: "Amy", replacing: 3, 4)
        _ = typed(&c, "\n  @Amy hello  ")
        let p = c.payload()
        XCTAssertEqual(p.text, "@Amy hello")
        XCTAssertEqual(p.mentions, [Mention(userId: "u1", start: 0, length: 4)])
        c.reset()
        XCTAssertEqual(c.payload().text, "")
        XCTAssertTrue(c.spans.isEmpty)
    }

    func testSurrogatePairsNotSplitByDiff() {
        var c = MentionComposer()
        _ = typed(&c, "@")
        _ = c.insertMention(userId: "u1", name: "A", replacing: 0, 1)          // "@A "
        _ = typed(&c, "@A 😀😁")
        // 删除第一个 emoji：两个 emoji 高位代理相同，前缀比较不能停在代理对中间
        XCTAssertEqual(typed(&c, "@A 😁").text, "@A 😁")
        XCTAssertEqual(c.spans, [MentionSpan(userId: "u1", start: 0, length: 2)])
    }

    // MARK: - 光标位置（fix2）

    /// 在文字中间输入 @：触发位置、筛选词只取到光标处（后面的原有文字不算），插入后光标在「@昵称 」之后
    func testInsertMentionInMiddleCursorAfterFragment() {
        var c = MentionComposer()
        _ = typed(&c, "你好😀再见")                            // 你好(2) 😀(2) 再见(2)
        XCTAssertEqual(c.cursor, 6)
        let r = typed(&c, "你好😀@再见")
        XCTAssertEqual(r.trigger, 4)
        XCTAssertEqual(c.cursor, 5, "光标在刚输入的 @ 之后")
        XCTAssertEqual(c.query(after: 4), "", "光标后面的「再见」不是筛选词")
        _ = typed(&c, "你好😀@小再见")
        XCTAssertEqual(c.cursor, 6)
        XCTAssertEqual(c.query(after: 4), "小")
        let text = c.insertMention(userId: "u1", name: "小美🌸", replacing: 4, 6)
        XCTAssertEqual(text, "你好😀@小美🌸 再见")
        XCTAssertEqual(c.spans, [MentionSpan(userId: "u1", start: 4, length: 5)])
        XCTAssertEqual(c.cursor, 10, "光标在「@小美🌸 」之后（4 + 6）")
        XCTAssertEqual(String(decoding: Array(text.utf16)[..<c.cursor], as: UTF16.self), "你好😀@小美🌸 ")
    }

    /// 整段删除提及：光标停在被删片段原来的起点，而不是末尾
    func testWholeFragmentDeleteCursorAtDeletionPoint() {
        var c = MentionComposer()
        _ = typed(&c, "说@")
        _ = c.insertMention(userId: "u1", name: "Bob", replacing: 1, 2)     // "说@Bob "
        _ = typed(&c, "说@Bob 你好")
        // 光标在「@Bob」末尾退格 → 删掉 b → 整段删除
        let r = typed(&c, "说@Bo 你好")
        XCTAssertEqual(r.text, "说 你好")
        XCTAssertEqual(c.cursor, 1)
        XCTAssertTrue(c.spans.isEmpty)
        // 普通删除：光标在删除处
        _ = typed(&c, "说 好")
        XCTAssertEqual(c.cursor, 2)
        // 普通插入：光标在插入内容之后
        _ = typed(&c, "说 你们好")
        XCTAssertEqual(c.cursor, 4)
    }

    /// 选中一段（跨越提及）替换成新文字：提及失效，光标在新文字之后
    func testReplaceSelectionCursorAfterInserted() {
        var c = MentionComposer()
        _ = typed(&c, "@")
        _ = c.insertMention(userId: "u1", name: "A", replacing: 0, 1)      // "@A "
        _ = typed(&c, "@A 😀x")
        _ = typed(&c, "@A 😁😁x")                                          // 替换 emoji（前缀不切开代理对）
        XCTAssertEqual(c.cursor, 7)
        XCTAssertEqual(c.spans, [MentionSpan(userId: "u1", start: 0, length: 2)])
        c.reset("草稿")
        XCTAssertEqual(c.cursor, 2, "恢复草稿后光标在末尾")
    }

    /// 光标移到 @ 之前（在前面继续输入）时关闭成员选择
    func testQueryNilWhenCursorBeforeTrigger() {
        var c = MentionComposer()
        _ = typed(&c, "ab@")
        XCTAssertEqual(c.query(after: 2), "")
        _ = typed(&c, "aXb@")                                              // 在 @ 前面输入
        XCTAssertNil(c.query(after: 2))
    }

    // MARK: - 气泡渲染

    func testSegmentsSplitByUTF16Ranges() {
        let text = "😀@小美 看这里 @Bob"
        let segs = MentionText.segments(text, [Mention(userId: "b", start: 10, length: 4),
                                               Mention(userId: "a", start: 2, length: 3)])
        XCTAssertEqual(segs, [
            .init(text: "😀"), .init(text: "@小美", userId: "a"), .init(text: " 看这里 "), .init(text: "@Bob", userId: "b"),
        ])
        XCTAssertEqual(segs.map(\.text).joined(), text)
    }

    func testInvalidRangesRenderAsPlainText() {
        let text = "😀abc"
        // 越界、切开代理对、长度 0、重叠 → 忽略
        let segs = MentionText.segments(text, [Mention(userId: "x", start: 1, length: 2),
                                               Mention(userId: "y", start: 4, length: 9),
                                               Mention(userId: "z", start: 2, length: 0)])
        XCTAssertEqual(segs.map(\.text).joined(), text)
        XCTAssertTrue(segs.allSatisfy { $0.userId == nil })
        let overlap = MentionText.segments("abcdef", [Mention(userId: "a", start: 0, length: 3),
                                                     Mention(userId: "b", start: 2, length: 2)])
        XCTAssertEqual(overlap, [.init(text: "abc", userId: "a"), .init(text: "def")])
    }
}
