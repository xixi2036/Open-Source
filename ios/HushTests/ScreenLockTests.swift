import XCTest
import LocalAuthentication
@testable import Hush

final class ScreenLockTests: XCTestCase {
    /// 回归：真机上任何无法验证的情况都曾直接解锁
    func testDeviceNeverUnlocksWithoutAuthentication() {
        XCTAssertEqual(ScreenLock.actionWhenPolicyUnavailable(.biometryLockout, simulator: false), .stayLocked)
        XCTAssertEqual(ScreenLock.actionWhenPolicyUnavailable(nil, simulator: false), .stayLocked)
        XCTAssertEqual(ScreenLock.actionWhenPolicyUnavailable(.passcodeNotSet, simulator: false), .disableLock)
    }

    func testSimulatorUnlocks() {
        XCTAssertEqual(ScreenLock.actionWhenPolicyUnavailable(.passcodeNotSet, simulator: true), .unlock)
    }
}
