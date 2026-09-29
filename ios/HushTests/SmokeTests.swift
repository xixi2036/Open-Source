import XCTest
@testable import Hush

final class SmokeTests: XCTestCase {
    func testHostDoesNotBootstrap() {
        XCTAssertTrue(AppEnvironment.isUnitTesting)
    }
}
