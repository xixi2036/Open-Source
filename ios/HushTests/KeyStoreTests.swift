import XCTest
import Security
@testable import Hush

private final class FakeBackend: DBKeyBackend {
    var keychain: Data?
    var readError: OSStatus?
    var writeStatus: OSStatus = errSecSuccess
    var file: Data?
    var fileWritable = true
    var random: Result<Data, KeyStoreError> = .success(Data(repeating: 7, count: 32))
    var keychainDeletes = 0

    func readKeychain() -> KeychainRead {
        if let e = readError { return .error(e) }
        return keychain.map { .found($0) } ?? .notFound
    }
    func writeKeychain(_ data: Data) -> OSStatus {
        if writeStatus == errSecSuccess { keychain = data }
        return writeStatus
    }
    func deleteKeychain() { keychainDeletes += 1; keychain = nil }
    func readFile() -> Data? { file }
    func writeFile(_ data: Data) -> Bool { if fileWritable { file = data }; return fileWritable }
    func deleteFile() { file = nil }
    func randomKey() -> Result<Data, KeyStoreError> { random }
}

final class KeyStoreTests: XCTestCase {
    private let stored = Data(repeating: 1, count: 32)

    func testReturnsExistingKeychainKey() throws {
        let b = FakeBackend(); b.keychain = stored
        XCTAssertEqual(try KeyStore.resolve(b, allowFileFallback: false), stored)
    }

    /// 回归：任何读取错误都曾被当作「没有密钥」，生成新密钥并删除 Keychain 中的真密钥
    func testReadErrorOnDeviceNeverRegeneratesOrDeletes() {
        let b = FakeBackend(); b.keychain = stored; b.readError = errSecInteractionNotAllowed
        XCTAssertThrowsError(try KeyStore.resolve(b, allowFileFallback: false)) { e in
            XCTAssertEqual(e as? KeyStoreError, .keychain(errSecInteractionNotAllowed))
        }
        XCTAssertEqual(b.keychain, stored, "真密钥必须保留")
        XCTAssertEqual(b.keychainDeletes, 0)
        XCTAssertNil(b.file)
    }

    func testReadErrorFallsBackToExistingFile() throws {
        let b = FakeBackend(); b.readError = -34018; b.file = stored
        XCTAssertEqual(try KeyStore.resolve(b, allowFileFallback: true), stored)
        XCTAssertEqual(b.file, stored)
    }

    func testSimulatorWithoutKeychainStoresNewKeyInFile() throws {
        let b = FakeBackend(); b.readError = -34018
        let k = try KeyStore.resolve(b, allowFileFallback: true)
        XCTAssertEqual(k.count, 32)
        XCTAssertEqual(b.file, k)
    }

    func testNotFoundGeneratesAndStoresInKeychain() throws {
        let b = FakeBackend()
        let k = try KeyStore.resolve(b, allowFileFallback: false)
        XCTAssertEqual(b.keychain, k)
        XCTAssertNil(b.file)
    }

    func testRandomFailureThrows() {
        let b = FakeBackend(); b.random = .failure(.random(-1))
        XCTAssertThrowsError(try KeyStore.resolve(b, allowFileFallback: false))
        XCTAssertNil(b.keychain)
    }

    func testUnpersistableKeyIsNeverReturned() {
        let b = FakeBackend(); b.writeStatus = errSecNotAvailable; b.fileWritable = false
        XCTAssertThrowsError(try KeyStore.resolve(b, allowFileFallback: true))
        let d = FakeBackend(); d.writeStatus = errSecNotAvailable
        XCTAssertThrowsError(try KeyStore.resolve(d, allowFileFallback: false), "真机不能退回到文件")
        XCTAssertNil(d.file)
    }

    func testFileKeyMigratesIntoKeychainAndFileIsRemoved() throws {
        let b = FakeBackend(); b.file = stored
        XCTAssertEqual(try KeyStore.resolve(b, allowFileFallback: true), stored)
        XCTAssertEqual(b.keychain, stored)
        XCTAssertNil(b.file, "迁移成功后删除文件副本")
    }

    func testFileKeptWhenKeychainMigrationFails() throws {
        let b = FakeBackend(); b.file = stored; b.writeStatus = errSecNotAvailable
        XCTAssertEqual(try KeyStore.resolve(b, allowFileFallback: true), stored)
        XCTAssertEqual(b.file, stored)
    }

    func testLeftoverFileCopyRemovedWhenKeychainHasSameKey() throws {
        let b = FakeBackend(); b.keychain = stored; b.file = stored
        _ = try KeyStore.resolve(b, allowFileFallback: false)
        XCTAssertNil(b.file)
    }
}
