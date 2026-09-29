import XCTest
@testable import Hush

@MainActor
final class TempFileTests: XCTestCase {
    private var root: URL!

    override func setUp() {
        root = FileManager.default.temporaryDirectory.appendingPathComponent("hush-tests-\(UUID().uuidString)", isDirectory: true)
        try? FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
    }
    override func tearDown() { try? FileManager.default.removeItem(at: root) }

    private func makeSource() throws -> URL {
        let src = root.appendingPathComponent("msg123_0_report.pdf")
        try Data("hello".utf8).write(to: src)
        return src
    }

    func testSafeFileName() {
        XCTAssertEqual(FileAttachmentView.safeFileName("..", fallback: "f"), "f")
        XCTAssertEqual(FileAttachmentView.safeFileName(".", fallback: "f"), "f")
        XCTAssertEqual(FileAttachmentView.safeFileName("", fallback: "f"), "f")
        XCTAssertEqual(FileAttachmentView.safeFileName("  ", fallback: "f"), "f")
        XCTAssertEqual(FileAttachmentView.safeFileName("../../etc/passwd", fallback: "f"), ".._.._etc_passwd")
        XCTAssertEqual(FileAttachmentView.safeFileName("a/b:c", fallback: "f"), "a_b_c")
        XCTAssertEqual(FileAttachmentView.safeFileName("报告.pdf", fallback: "f"), "报告.pdf")
    }

    /// 回归：文件名为 ".." 时曾删除 share 的父目录（整个 tmp）
    func testDotDotNameCannotDeleteOutsideShareDir() throws {
        let src = try makeSource()
        let base = root.appendingPathComponent("share", isDirectory: true)
        let sibling = root.appendingPathComponent("sibling.txt")
        try Data("keep".utf8).write(to: sibling)
        for name in ["..", ".", "", "../x", "report.pdf"] {
            let out = FileAttachmentView.shareCopy(src.path, name: name, base: base)
            XCTAssertTrue(out.path.hasPrefix(base.path + "/"), "\(name) -> \(out.path)")
            XCTAssertTrue(FileManager.default.fileExists(atPath: out.path))
            XCTAssertTrue(FileManager.default.fileExists(atPath: sibling.path), "父目录中的文件被删除了（name=\(name)）")
            XCTAssertTrue(FileManager.default.fileExists(atPath: src.path))
        }
        let out = FileAttachmentView.shareCopy(src.path, name: "..", base: base)
        XCTAssertEqual(out.lastPathComponent, src.lastPathComponent)
        FileAttachmentView.clearShareCopies(base: base)
        XCTAssertFalse(FileManager.default.fileExists(atPath: base.path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: sibling.path))
    }

    func testDiscardOutboxRemovesFileAndItsSubdirectoryOnly() throws {
        let file = AppPaths.newOutboxFile(ext: "jpg")
        try Data("x".utf8).write(to: file)
        AppPaths.discardOutbox(file.path)
        XCTAssertFalse(FileManager.default.fileExists(atPath: file.path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: AppPaths.outbox.path), "outbox 目录本身保留")

        let dir = AppPaths.outbox.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let named = dir.appendingPathComponent("合同.pdf")
        try Data("x".utf8).write(to: named)
        AppPaths.discardOutbox(named.path)
        XCTAssertFalse(FileManager.default.fileExists(atPath: dir.path))

        let outside = try makeSource()
        AppPaths.discardOutbox(outside.path)
        XCTAssertTrue(FileManager.default.fileExists(atPath: outside.path), "outbox 之外的文件不能删")
        AppPaths.discardOutbox(AppPaths.outbox.appendingPathComponent("../\(outside.lastPathComponent)").path)
        XCTAssertTrue(FileManager.default.fileExists(atPath: outside.path))
    }

    func testMimeTypes() {
        XCTAssertEqual(MimeType.of(URL(fileURLWithPath: "/a/b.docx")), "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
        XCTAssertEqual(MimeType.of(URL(fileURLWithPath: "/a/b.mov")), "video/quicktime")
        XCTAssertEqual(MimeType.of(URL(fileURLWithPath: "/a/b.JPG")), "image/jpeg")
        XCTAssertEqual(MimeType.of(URL(fileURLWithPath: "/a/b.pdf")), "application/pdf")
        XCTAssertEqual(MimeType.of(URL(fileURLWithPath: "/a/b")), "application/octet-stream")
    }
}
