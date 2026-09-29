import XCTest
@testable import Hush

@MainActor
final class CallManagerTests: XCTestCase {
    private func offer(_ id: String, video: Bool = false) -> CallSignal {
        CallSignal(callId: id, action: .offer, video: video, sdp: "v=0\r\n", candidates: [])
    }
    private func signal(_ id: String, _ action: CallAction) -> CallSignal {
        CallSignal(callId: id, action: action, video: false, sdp: nil, candidates: [])
    }

    func testSignalsFromOtherUserAreIgnored() {
        let m = CallManager()
        m.handleSignal(from: "alice", signal: offer("c1"))
        XCTAssertEqual(m.phase, .incoming)
        m.handleSignal(from: "mallory", signal: signal("c1", .hangup))
        m.handleSignal(from: "mallory", signal: signal("c1", .reject))
        m.handleSignal(from: "mallory", signal: signal("c1", .busy))
        XCTAssertEqual(m.phase, .incoming)
        m.handleSignal(from: "alice", signal: signal("c1", .hangup))
        XCTAssertEqual(m.phase, .ended("对方已取消"))
    }

    func testLateRejectAfterCallEndedDoesNotReopenCallView() {
        let m = CallManager()
        m.handleSignal(from: "alice", signal: offer("c1"))
        m.handleSignal(from: "alice", signal: signal("c1", .hangup))
        m.phase = .idle                     // 1.5 秒后回到 idle
        m.handleSignal(from: "alice", signal: signal("c1", .reject))
        m.handleSignal(from: "alice", signal: signal("c1", .busy))
        XCTAssertEqual(m.phase, .idle)
    }

    func testEndedReasonNotOverwrittenByLateSignal() {
        let m = CallManager()
        m.handleSignal(from: "alice", signal: offer("c1"))
        m.handleSignal(from: "alice", signal: signal("c1", .hangup))
        m.handleSignal(from: "alice", signal: signal("c1", .reject))
        XCTAssertEqual(m.phase, .ended("对方已取消"))
    }

    func testNewOfferDuringEndedWindowIsTreatedAsIdle() {
        let m = CallManager()
        m.handleSignal(from: "alice", signal: offer("c1"))
        m.handleSignal(from: "alice", signal: signal("c1", .hangup))
        m.handleSignal(from: "carol", signal: offer("c2", video: true))
        XCTAssertEqual(m.phase, .incoming)
        XCTAssertEqual(m.peerId, "carol")
        XCTAssertEqual(m.callId, "c2")
        XCTAssertTrue(m.video)
    }

    func testAttemptBecomesStaleAfterEnd() {
        let m = CallManager()
        m.handleSignal(from: "alice", signal: offer("c1"))
        let token = m.currentAttempt
        XCTAssertTrue(m.isCurrent(token))
        m.handleSignal(from: "alice", signal: signal("c1", .hangup))
        XCTAssertFalse(m.isCurrent(token), "挂断后，接听/呼叫流程在 await 之后必须停止")
        m.handleSignal(from: "bob", signal: offer("c2"))
        XCTAssertFalse(m.isCurrent(token), "新来电不能让旧流程复活")
        XCTAssertTrue(m.isCurrent(m.currentAttempt))
    }

    func testDeviceStateForCallPhases() {
        XCTAssertEqual(CallManager.deviceState(phase: .idle, video: false, speaker: false), .init(idleTimerDisabled: false, proximity: false))
        XCTAssertEqual(CallManager.deviceState(phase: .ended("x"), video: true, speaker: true), .init(idleTimerDisabled: false, proximity: false))
        XCTAssertEqual(CallManager.deviceState(phase: .active, video: true, speaker: true), .init(idleTimerDisabled: true, proximity: false))
        XCTAssertEqual(CallManager.deviceState(phase: .active, video: false, speaker: false), .init(idleTimerDisabled: true, proximity: true))
        XCTAssertEqual(CallManager.deviceState(phase: .active, video: false, speaker: true), .init(idleTimerDisabled: true, proximity: false))
        XCTAssertEqual(CallManager.deviceState(phase: .incoming, video: false, speaker: false), .init(idleTimerDisabled: true, proximity: false))
    }
}
