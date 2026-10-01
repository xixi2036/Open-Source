import Foundation
import Security

/// Keychain 读取结果：区分「确实没有」与「暂时读不到」，后者绝不能当作没有而重新生成密钥
enum KeychainRead: Equatable {
    case found(Data)
    case notFound
    case error(OSStatus)
}

enum KeyStoreError: LocalizedError, Equatable {
    case keychain(OSStatus)
    case random(OSStatus)
    case persist(OSStatus)

    var errorDescription: String? {
        switch self {
        case let .keychain(st): return "无法读取钥匙串中的数据库密钥（\(st)），请解锁设备后重试"
        case let .random(st): return "无法生成数据库密钥（\(st)）"
        case let .persist(st): return "无法保存数据库密钥（\(st)）"
        }
    }
}

/// 密钥存储的底层操作（测试中替换为内存实现）
protocol DBKeyBackend {
    func readKeychain() -> KeychainRead
    /// 新增或更新（不先删除）
    func writeKeychain(_ data: Data) -> OSStatus
    func deleteKeychain()
    func readFile() -> Data?
    func writeFile(_ data: Data) -> Bool
    func deleteFile()
    func randomKey() -> Result<Data, KeyStoreError>
}

/// 32 字节数据库密钥：存 Keychain（kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly）。
/// 未签名的模拟器构建没有 Keychain 权限（-34018），仅模拟器退回到应用私有文件（不进备份）。
enum KeyStore {
    static let keyLength = 32

    static var allowFileFallback: Bool {
        #if targetEnvironment(simulator)
        return true
        #else
        return false
        #endif
    }

    static func loadOrCreateDBKey() throws -> Data {
        try resolve(SystemDBKeyBackend(), allowFileFallback: allowFileFallback, databaseExists: AppPaths.databaseExists)
    }

    /// 读取或创建密钥。只有 Keychain 明确返回「不存在」时才会生成新密钥；
    /// 其他错误（设备首次解锁前、偶发的 -34018 等）直接报错，不写入、不删除任何东西。
    /// databaseExists：本地库文件已经存在。此时钥匙串「读取出错」且没有文件副本，说明密钥只是暂时读不到，
    /// 即使在模拟器上也不能生成新密钥（新密钥打不开旧库，还会把真正的密钥顶掉）。
    static func resolve(_ b: DBKeyBackend, allowFileFallback: Bool, databaseExists: Bool = false) throws -> Data {
        let file = b.readFile().flatMap { $0.count == keyLength ? $0 : nil }
        switch b.readKeychain() {
        case let .found(k) where k.count == keyLength:
            if file == k { b.deleteFile() }   // 迁移后遗留的文件副本
            return k
        case .found:
            throw KeyStoreError.keychain(errSecDecode)
        case .notFound:
            if let f = file {
                // 旧版本 / 模拟器保存在文件里：迁移进 Keychain，确认能读回后删除文件
                if b.writeKeychain(f) == errSecSuccess, b.readKeychain() == .found(f) { b.deleteFile() }
                return f
            }
            let key = try b.randomKey().get()
            let st = b.writeKeychain(key)
            if st == errSecSuccess { return key }
            if allowFileFallback, b.writeFile(key) { return key }
            throw KeyStoreError.persist(st)
        case let .error(st):
            if let f = file { return f }
            guard allowFileFallback, !databaseExists else { throw KeyStoreError.keychain(st) }
            // 模拟器未签名构建、首次启动：Keychain 不可用，密钥只存文件
            let key = try b.randomKey().get()
            guard b.writeFile(key) else { throw KeyStoreError.persist(st) }
            return key
        }
    }

    /// 用户确认「重置本地数据」后删除密钥
    static func deleteDBKey() {
        let b = SystemDBKeyBackend()
        b.deleteKeychain()
        b.deleteFile()
    }
}

struct SystemDBKeyBackend: DBKeyBackend {
    private let service = "com.hush.app.dbkey"
    private let account = "db_key"

    private var base: [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: service,
         kSecAttrAccount as String: account]
    }

    func readKeychain() -> KeychainRead {
        var q = base
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: AnyObject?
        let st = SecItemCopyMatching(q as CFDictionary, &out)
        switch st {
        case errSecSuccess:
            guard let d = out as? Data else { return .error(errSecDecode) }
            return .found(d)
        case errSecItemNotFound: return .notFound
        default: return .error(st)
        }
    }

    func writeKeychain(_ data: Data) -> OSStatus {
        var add = base
        add[kSecValueData as String] = data
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let st = SecItemAdd(add as CFDictionary, nil)
        guard st == errSecDuplicateItem else { return st }
        let attrs: [String: Any] = [kSecValueData as String: data,
                                    kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly]
        return SecItemUpdate(base as CFDictionary, attrs as CFDictionary)
    }

    func deleteKeychain() { SecItemDelete(base as CFDictionary) }

    private var fileURL: URL { AppPaths.appSupport.appendingPathComponent(".dbkey") }

    func readFile() -> Data? {
        guard let d = try? Data(contentsOf: fileURL) else { return nil }
        excludeFromBackup()
        return d
    }

    func writeFile(_ data: Data) -> Bool {
        do {
            try data.write(to: fileURL, options: [.completeFileProtectionUntilFirstUserAuthentication, .atomic])
        } catch { return false }
        excludeFromBackup()
        return (try? Data(contentsOf: fileURL)) == data
    }

    func deleteFile() { try? FileManager.default.removeItem(at: fileURL) }

    /// 密钥文件不能和数据库一起进入 iCloud / 电脑备份
    private func excludeFromBackup() {
        var url = fileURL
        var v = URLResourceValues()
        v.isExcludedFromBackup = true
        try? url.setResourceValues(v)
    }

    func randomKey() -> Result<Data, KeyStoreError> {
        var bytes = [UInt8](repeating: 0, count: KeyStore.keyLength)
        let st = SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes)
        return st == errSecSuccess ? .success(Data(bytes)) : .failure(.random(st))
    }
}

enum AppPaths {
    static var appSupport: URL {
        let url = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    static var dataDir: URL {
        let url = appSupport.appendingPathComponent("hush", isDirectory: true)
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    /// 本地数据库文件是否已存在（决定钥匙串读不到时能不能生成新密钥）
    static var databaseExists: Bool {
        let dir = appSupport.appendingPathComponent("hush", isDirectory: true)
        return FileManager.default.fileExists(atPath: dir.appendingPathComponent("hush.sqlite").path)
    }

    /// A2：整个数据目录（数据库、WAL、明文附件缓存 files/）不进 iCloud / 电脑备份，并显式设置文件保护级别。
    /// 备份排除标记设在目录上即对其下所有内容生效；保护级别对已存在的文件逐个补设（新文件继承目录的级别）。
    /// 用 completeUntilFirstUserAuthentication 而不是 complete：锁屏后后台仍要能写库（WAL）和收消息。
    /// 会遍历目录，必须在后台线程、打开数据库之前调用。
    static func protectDataDir(_ dir: URL = dataDir) {
        var url = dir
        var v = URLResourceValues()
        v.isExcludedFromBackup = true
        try? url.setResourceValues(v)
        let fm = FileManager.default
        let attrs: [FileAttributeKey: Any] = [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication]
        try? fm.setAttributes(attrs, ofItemAtPath: dir.path)
        guard let walker = fm.enumerator(at: dir, includingPropertiesForKeys: [.fileProtectionKey]) else { return }
        for case let item as URL in walker {
            let current = try? item.resourceValues(forKeys: [.fileProtectionKey]).fileProtection
            if current != .completeUntilFirstUserAuthentication && current != .complete {
                try? fm.setAttributes(attrs, ofItemAtPath: item.path)
            }
        }
    }

    /// 发送前的临时文件（压缩后的图片、录音等）
    static var outbox: URL {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("outbox", isDirectory: true)
        try? FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    static func newOutboxFile(ext: String) -> URL {
        outbox.appendingPathComponent(UUID().uuidString).appendingPathExtension(ext)
    }

    /// 分享用的临时明文副本（tmp/share/<UUID>/原文件名）
    static var shareDir: URL {
        FileManager.default.temporaryDirectory.appendingPathComponent("share", isDirectory: true)
    }

    /// 发送完成后删除 outbox 里的明文临时文件（核心已在自己的目录保存副本）。
    /// 只处理位于 outbox 之内的路径；若文件在 outbox/<UUID>/ 子目录中，连同子目录一起删除。
    static func discardOutbox(_ path: String) {
        let root = outbox.standardizedFileURL.resolvingSymlinksInPath().path
        let file = URL(fileURLWithPath: path).standardizedFileURL.resolvingSymlinksInPath()
        guard file.path.hasPrefix(root + "/") else { return }
        let parent = file.deletingLastPathComponent()
        let target = parent.path == root ? file : parent
        try? FileManager.default.removeItem(at: target)
    }

    /// 退出登录时清理全部临时明文（待发送文件、分享副本）
    static func clearTemporaryPlaintext() {
        try? FileManager.default.removeItem(at: FileManager.default.temporaryDirectory.appendingPathComponent("outbox", isDirectory: true))
        try? FileManager.default.removeItem(at: shareDir)
    }
}
