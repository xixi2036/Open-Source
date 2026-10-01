import Foundation
import Observation

/// 头像 / 朋友圈图片的本地文件路径缓存（从 HushCore 拆出，退出登录时整体清空）
@MainActor @Observable
final class MediaCache {
    var avatarPaths: [String: String] = [:]
    var mediaPaths: [String: String] = [:]
    @ObservationIgnored private var pendingAvatars: Set<String> = []
    @ObservationIgnored private var pendingMedia: Set<String> = []
    /// reset() 时递增：退出登录前发出的请求结果不再写回
    @ObservationIgnored private var generation = 0

    func loadAvatar(_ id: String, fetch: @escaping (String) async throws -> String) async {
        guard !isCached(avatarPaths[id]), !pendingAvatars.contains(id) else { return }
        pendingAvatars.insert(id)
        let gen = generation
        let path = try? await fetch(id)
        guard gen == generation else { return }
        pendingAvatars.remove(id)
        if let path { avatarPaths[id] = path }
    }

    func loadMedia(_ id: String, fetch: @escaping (String) async throws -> String) async {
        guard !isCached(mediaPaths[id]), !pendingMedia.contains(id) else { return }
        pendingMedia.insert(id)
        let gen = generation
        let path = try? await fetch(id)
        guard gen == generation else { return }
        pendingMedia.remove(id)
        if let path { mediaPaths[id] = path }
    }

    /// 路径存在且文件仍在（核心退出登录会删除 files/）
    private func isCached(_ path: String?) -> Bool {
        guard let path else { return false }
        return FileManager.default.fileExists(atPath: path)
    }

    func reset() {
        generation += 1
        avatarPaths = [:]
        mediaPaths = [:]
        pendingAvatars = []
        pendingMedia = []
        ImageCache.removeAll()
        AttachmentFiles.reset()
    }
}
