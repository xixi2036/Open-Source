import Foundation
import Observation

/// 朋友圈动态流（全部好友 / 某个用户的相册 / 单条动态详情）
@MainActor @Observable
final class MomentsModel {
    enum Source: Equatable {
        case feed
        case user(String)
        case single(String)
    }

    let source: Source
    var moments: [Moment] = []
    var loading = false
    var hasMore = true
    var loaded = false
    /// 单条动态已删除或对我不可见（服务端 404）
    var unavailable = false
    /// 正在评论的目标
    var commentTarget: CommentTarget?

    /// 评论写回到列表时递增（值为动态 id，列表据此把该卡片滚到可视区域内）
    var commentAdded: (momentId: String, tick: Int)?

    struct CommentTarget: Equatable {
        let momentId: String
        let replyTo: Profile?
    }

    @ObservationIgnored private let core = HushCore.shared
    @ObservationIgnored private var store: MomentsStore { core.moments }

    init(source: Source = .feed) { self.source = source }

    convenience init(userId: String?) { self.init(source: userId.map { .user($0) } ?? .feed) }

    var isSingle: Bool { if case .single = source { return true } else { return false } }

    private func fetch(before: Int64?) async throws -> [Moment] {
        switch source {
        case .feed: return try await store.feed(before: before)
        case let .user(uid): return try await store.userMoments(uid, before: before)
        case let .single(id): return before == nil ? [try await store.moment(id)] : []
        }
    }

    func refresh() async {
        guard !loading else { return }
        loading = true
        defer { loading = false; loaded = true }
        do {
            let list = try await fetch(before: nil)
            moments = list
            unavailable = false
            hasMore = !isSingle && !list.isEmpty
        } catch HushError.NotFound where isSingle {
            moments = []
            unavailable = true
            hasMore = false
        } catch { core.toast = describe(error) }
    }

    /// 有新的朋友圈通知（点赞、评论、回复）时拉取第一页并按 id 合并：更新已有动态、插入新动态，不丢弃已加载的后续页。
    /// 通知对应的动态已加载但不在第一页（较早的动态收到新评论）时，单独拉取这一条更新。
    func mergeLatest(touched momentId: String? = nil) async {
        guard loaded, !loading else { return }
        if isSingle {
            if case let .single(id) = source, momentId == nil || momentId == id { await refresh() }
            return
        }
        guard let list = try? await fetch(before: nil) else { return }
        moments = Self.merge(existing: moments, latest: list)
        guard let momentId, moments.contains(where: { $0.id == momentId }),
              !list.contains(where: { $0.id == momentId }) else { return }
        do {
            let m = try await store.moment(momentId)
            update(momentId) { $0 = m }
        } catch HushError.NotFound {
            moments.removeAll { $0.id == momentId }
        } catch {}
    }

    static func merge(existing: [Moment], latest: [Moment]) -> [Moment] {
        var fresh = Dictionary(latest.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
        var out = existing.map { fresh.removeValue(forKey: $0.id) ?? $0 }
        let added = latest.filter { fresh[$0.id] != nil }
        guard !added.isEmpty else { return out }
        out.append(contentsOf: added)
        out.sort { $0.createdAt > $1.createdAt }
        return out
    }

    func loadMore() async {
        guard hasMore, !loading, let last = moments.last else { return }
        loading = true
        defer { loading = false }
        do {
            let list = try await fetch(before: last.createdAt)
            let ids = Set(moments.map(\.id))
            let fresh = list.filter { !ids.contains($0.id) }
            moments.append(contentsOf: fresh)
            hasMore = !fresh.isEmpty
        } catch { core.toast = describe(error) }
    }

    func liked(_ m: Moment) -> Bool { m.likes.contains { $0.id == core.myId } }

    func toggleLike(_ m: Moment) {
        let like = !liked(m)
        let before = m.likes
        update(m.id) { mm in
            if like, let me = core.myProfile { mm.likes.append(me) } else { mm.likes.removeAll { $0.id == core.myId } }
        }
        Task {
            do { try await store.like(m.id, like) } catch {
                core.toast = describe(error)
                // 失败只回滚这一条，不重置分页
                update(m.id) { $0.likes = before }
            }
        }
    }

    func sendComment(_ text: String) {
        guard let t = commentTarget else { return }
        let body = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !body.isEmpty else { return }
        commentTarget = nil
        Task {
            do {
                let c = try await store.comment(t.momentId, text: body, replyTo: t.replyTo?.id)
                // 只改这一条（不替换列表、不改 id），列表保持当前位置
                update(t.momentId) { $0.comments.append(c) }
                commentAdded = (t.momentId, (commentAdded?.tick ?? 0) + 1)
            } catch { core.toast = describe(error) }
        }
    }

    func deleteComment(_ momentId: String, _ c: Comment) {
        update(momentId) { $0.comments.removeAll { $0.id == c.id } }
        let store = store, id = c.id
        core.run { try await store.deleteComment(id) }
    }

    func deleteMoment(_ m: Moment) {
        moments.removeAll { $0.id == m.id }
        let store = store, id = m.id
        core.run { try await store.deleteMoment(id) }
        // 详情页删除后返回上一页
        if isSingle { Router.shared.pop() }
    }

    func inserted(_ m: Moment) {
        moments.insert(m, at: 0)
    }

    private func update(_ id: String, _ f: (inout Moment) -> Void) {
        guard let i = moments.firstIndex(where: { $0.id == id }) else { return }
        f(&moments[i])
    }
}
