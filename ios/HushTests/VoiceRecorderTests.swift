import XCTest
@testable import Hush

/// 不碰真实音频会话与麦克风的录音后端：模拟"没有麦克风"，录音结束时由录音器生成静音文件
@MainActor
private final class FakeVoiceCapture: VoiceCaptureBackend {
    private(set) var begins = 0
    private(set) var ends = 0
    func begin(url: URL) -> Bool { begins += 1; return false }
    func end() { ends += 1 }
}

@MainActor
final class VoiceRecorderTests: XCTestCase {
    private func makeRecorder() -> VoiceRecorder {
        let r = VoiceRecorder()
        r.permission = { .granted }
        r.backend = FakeVoiceCapture()
        return r
    }

    func testReachingLimitDeliversRecordingInsteadOfDiscarding() throws {
        let r = makeRecorder()
        r.maxDuration = 2
        var delivered: (URL, UInt64)?
        r.onAutoFinish = { delivered = ($0, $1) }
        r.pressChanged(translationY: 0)
        XCTAssertTrue(r.isRecording)
        r.tick(now: Date().addingTimeInterval(2.5))
        XCTAssertFalse(r.isRecording)
        let (url, ms) = try XCTUnwrap(delivered, "达到上限的录音必须交给发送方")
        XCTAssertEqual(ms, 2000, "时长按上限截断")
        XCTAssertTrue(FileManager.default.fileExists(atPath: url.path))
        // 手指仍按着：不能重新开始录音，松手也不会重复发送
        r.pressChanged(translationY: 0)
        XCTAssertFalse(r.isRecording)
        XCTAssertNil(r.pressEnded())
        try? FileManager.default.removeItem(at: url)
    }

    func testNewPressAfterAutoFinishStartsAgain() {
        let r = makeRecorder()
        r.maxDuration = 1
        r.onAutoFinish = { url, _ in try? FileManager.default.removeItem(at: url) }
        r.pressChanged(translationY: 0)
        r.tick(now: Date().addingTimeInterval(1.5))
        _ = r.pressEnded()
        r.pressChanged(translationY: 0)
        XCTAssertTrue(r.isRecording)
        r.cancel()
        XCTAssertFalse(r.isRecording)
    }

    func testCancelledGestureStopsRecording() {
        let r = makeRecorder()
        r.pressChanged(translationY: 0)
        XCTAssertTrue(r.isRecording)
        r.pressReset()           // 手势被系统取消，没有 onEnded
        XCTAssertFalse(r.isRecording)
        XCTAssertFalse(r.pressActive)
    }

    func testNormalReleaseIsNotCancelledByGestureReset() {
        let r = makeRecorder()
        r.pressChanged(translationY: 0)
        let result = r.stop(now: Date().addingTimeInterval(1.5))
        XCTAssertNotNil(result)
        _ = r.pressEnded()
        r.pressReset()
        if let url = result?.0 { try? FileManager.default.removeItem(at: url) }
    }

    func testDeniedPermissionDoesNotRecord() {
        let r = VoiceRecorder()
        r.permission = { .denied }
        r.backend = FakeVoiceCapture()
        r.pressChanged(translationY: 0)
        XCTAssertFalse(r.isRecording)
        XCTAssertNil(r.pressEnded())
    }

    func testSwipeUpCancels() {
        let r = makeRecorder()
        r.pressChanged(translationY: 0)
        r.pressChanged(translationY: -80)
        XCTAssertTrue(r.willCancel)
        XCTAssertNil(r.pressEnded())
    }
}
