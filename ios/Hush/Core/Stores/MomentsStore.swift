import Foundation
import Observation

/// 朋友圈：互动通知（及已读位置）与动态的读写接口。
/// 各页面的动态列表状态在 MomentsModel 中，它通过这里访问核心。
@MainActor @Observable
final class MomentsStore {
    var notes: [MomentNotification] = []
    /// 已看过的朋友圈通知最大 created_at（按账号保存）
    var notesSeenTs: Int64 = 0

    @ObservationIgnored private var client: HushClient?
    @ObservationIgnored private unowned let core: HushCore

    init(core: HushCore) { self.core = core }

    func attach(_ client: HushClient?) { self.client = client }

    private func api() throws -> HushClient {
        guard let client else { throw HushError.NotLoggedIn }
        return client
    }

    var newNoteCount: Int {
        notes.filter { $0.createdAt > notesSeenTs && $0.from.id != core.myId }.count
    }

    func loadLocal(userId: String) {
        notesSeenTs = Self.notesSeen(userId: userId)
    }

    func reset() {
        notes = []
        notesSeenTs = 0
    }

    func handle(_ event: CoreEvent) {
        if case let .momentNotification(notification) = event, !notes.contains(where: { $0.id == notification.id }) {
            notes.insert(notification, at: 0)
        }
    }

    // MARK: - 通知

    func refreshNotes() async {
        if let n = try? await client?.momentNotifications() { notes = n }
    }

    func markNotesSeen() {
        let maxTs = notes.map(\.createdAt).max() ?? 0
        notesSeenTs = max(notesSeenTs, maxTs)
        guard !core.myId.isEmpty else { return }
        UserDefaults.standard.set(Int(notesSeenTs), forKey: Self.seenKey(core.myId))
    }

    static func seenKey(_ userId: String) -> String { "momentNotesSeenTs.\(userId)" }

    /// 读取某账号的「已看到的朋友圈通知时间」；旧版本保存的全局值迁移给首个登录的账号
    static func notesSeen(userId: String, defaults: UserDefaults = .standard) -> Int64 {
        guard !userId.isEmpty else { return 0 }
        let key = seenKey(userId)
        let legacy = "momentNotesSeenTs"
        if defaults.object(forKey: key) == nil, defaults.object(forKey: legacy) != nil {
            defaults.set(defaults.integer(forKey: legacy), forKey: key)
            defaults.removeObject(forKey: legacy)
        }
        return Int64(defaults.integer(forKey: key))
    }

    // MARK: - 动态

    func feed(before: Int64?) async throws -> [Moment] {
        try await api().momentsFeed(before: before)
    }

    func userMoments(_ userId: String, before: Int64?) async throws -> [Moment] {
        try await api().userMoments(userId: userId, before: before)
    }

    /// 单条动态；已删除或对我不可见时抛 HushError.NotFound
    func moment(_ id: String) async throws -> Moment {
        try await api().moment(id: id)
    }

    func like(_ id: String, _ like: Bool) async throws {
        try await api().likeMoment(id: id, like: like)
    }

    func comment(_ id: String, text: String, replyTo: String?) async throws -> Comment {
        try await api().commentMoment(id: id, text: text, replyTo: replyTo)
    }

    func deleteComment(_ id: String) async throws {
        try await api().deleteComment(id: id)
    }

    func deleteMoment(_ id: String) async throws {
        try await api().deleteMoment(id: id)
    }

    func post(text: String, imagePaths: [String], visibility: Visibility) async throws -> Moment {
        try await api().postMoment(text: text, imagePaths: imagePaths, visibility: visibility)
    }
}
