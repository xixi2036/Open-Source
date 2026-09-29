import Foundation
import Observation

/// 通讯录：好友、好友申请、群、黑名单、用户资料缓存、安全码核验状态。
/// 视图只通过这里读写联系人相关数据，不直接调用 HushClient。
@MainActor @Observable
final class ContactsStore {
    var friends: [Profile] = []
    var groups: [GroupInfo] = []
    var friendRequests = FriendRequests(incoming: [], outgoing: [])
    var blocked: [String] = []
    var profiles: [String: Profile] = [:]
    var verifiedUsers: Set<String> = []

    @ObservationIgnored private var client: HushClient?
    @ObservationIgnored private unowned let core: HushCore
    @ObservationIgnored private var pendingProfiles: Set<String> = []
    /// 拉取失败的资料：30 秒内不再重试
    @ObservationIgnored private var profileFailures: [String: Date] = [:]
    /// 本地库里读到的资料（不参与观察，避免在 body 中修改被观察状态）
    @ObservationIgnored private var storedProfiles: [String: Profile] = [:]

    init(core: HushCore) { self.core = core }

    func attach(_ client: HushClient?) { self.client = client }

    private func api() throws -> HushClient {
        guard let client else { throw HushError.NotLoggedIn }
        return client
    }

    var pendingRequestCount: Int {
        friendRequests.incoming.filter { $0.status == "pending" }.count
    }

    // MARK: - 生命周期

    func loadLocal(me: Profile?) {
        setFriends((try? client?.friends()) ?? [])
        groups = (try? client?.groups()) ?? []
        blocked = (try? client?.blocked()) ?? []
        if let me { profiles[me.id] = me }
    }

    func fullSync() async {
        guard let c = client else { return }
        if let r = try? await c.listFriendRequests() { friendRequests = r }
        if let f = try? await c.refreshFriends() { setFriends(f) }
        if let g = try? await c.refreshGroups() { groups = g }
        blocked = (try? c.blocked()) ?? []
    }

    func reset() {
        friends = []; groups = []; blocked = []
        friendRequests = FriendRequests(incoming: [], outgoing: [])
        profiles = [:]; verifiedUsers = []
        pendingProfiles = []; profileFailures = [:]; storedProfiles = [:]
    }

    func handle(_ event: CoreEvent) {
        switch event {
        case let .friendRequestReceived(request):
            if let i = friendRequests.incoming.firstIndex(where: { $0.id == request.id }) {
                friendRequests.incoming[i] = request
            } else {
                friendRequests.incoming.insert(request, at: 0)
            }
            Task { await refreshFriendRequests() }
        case .friendsChanged:
            setFriends((try? client?.friends()) ?? friends)
            blocked = (try? client?.blocked()) ?? blocked
            Task { await refreshFriendRequests() }
        case .groupsChanged:
            groups = (try? client?.groups()) ?? groups
        case let .keyChanged(userId):
            verifiedUsers.remove(userId)
        default:
            break
        }
    }

    private func setFriends(_ list: [Profile]) {
        friends = list.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
        for p in list { profiles[p.id] = p }
    }

    // MARK: - 资料

    func profile(_ userId: String) -> Profile? {
        if let p = profiles[userId] { return p }
        if let p = storedProfiles[userId] { return p }
        if let p = client?.cachedProfile(userId: userId) {
            storedProfiles[userId] = p
            return p
        }
        requestProfile(userId)
        return nil
    }

    func displayName(_ userId: String) -> String {
        if userId == core.myId { return core.account?.profile.name ?? "我" }
        if let p = profile(userId), !p.name.isEmpty { return p.name }
        return "用户"
    }

    /// 记住一份最新资料（本人资料更新、搜索结果）
    func remember(_ p: Profile) { profiles[p.id] = p }

    private func requestProfile(_ userId: String) {
        guard !userId.isEmpty, let c = client, !pendingProfiles.contains(userId) else { return }
        if let failed = profileFailures[userId], Date().timeIntervalSince(failed) < 30 { return }
        pendingProfiles.insert(userId)
        Task {
            let p = try? await c.getUser(userId: userId)
            // 期间退出登录（pending 被清空）则丢弃结果
            guard pendingProfiles.remove(userId) != nil else { return }
            if let p { profiles[userId] = p; profileFailures[userId] = nil } else { profileFailures[userId] = Date() }
        }
    }

    func fetchProfile(_ userId: String) async -> Profile? {
        if let p = try? await client?.getUser(userId: userId) {
            profiles[userId] = p
            return p
        }
        return profile(userId)
    }

    func isFriend(_ userId: String) -> Bool { friends.contains { $0.id == userId } }
    func isBlocked(_ userId: String) -> Bool { blocked.contains(userId) }

    func group(_ groupId: String) -> GroupInfo? {
        groups.first { $0.id == groupId } ?? (try? client?.groupInfo(groupId: groupId)) ?? nil
    }

    var myQrPayload: String { client?.myQrPayload() ?? "" }

    // MARK: - 好友

    func refreshFriendRequests() async {
        if let r = try? await client?.listFriendRequests() { friendRequests = r }
    }

    func refreshFriends() async {
        if let f = try? await client?.refreshFriends() { setFriends(f) }
        blocked = (try? client?.blocked()) ?? blocked
    }

    func refreshGroups() async {
        if let g = try? await client?.refreshGroups() { groups = g }
    }

    func searchUser(email: String) async throws -> Profile? {
        let p = try await api().searchUserByEmail(email: email)
        if let p { profiles[p.id] = p }
        return p
    }

    func sendFriendRequest(to userId: String, message: String) async throws {
        try await api().sendFriendRequest(userId: userId, message: message)
        await refreshFriendRequests()
        // 对方通过后核心会发 FriendsChanged 事件，由 handle 刷新好友列表
    }

    func respond(to request: FriendRequest, accept: Bool) async throws {
        if accept { try await api().acceptFriendRequest(id: request.id) } else { try await api().rejectFriendRequest(id: request.id) }
        await refreshFriendRequests()
        await refreshFriends()
    }

    func setBlocked(_ userId: String, _ block: Bool) async throws {
        if block { try await api().block(userId: userId) } else { try await api().unblock(userId: userId) }
        await refreshFriends()
    }

    /// 删除好友并清空与其的单聊
    func removeFriend(_ userId: String) async throws {
        let c = try api()
        try await c.removeFriend(userId: userId)
        try? c.clearConversation(convId: c.directConversationId(userId: userId))
        await refreshFriends()
    }

    // MARK: - 群

    func createGroup(name: String, memberIds: [String]) async throws -> GroupInfo {
        let g = try await api().createGroup(name: name, memberIds: memberIds)
        await refreshGroups()
        return g
    }

    func addMembers(_ groupId: String, _ ids: [String]) async throws {
        _ = try await api().addMembers(groupId: groupId, ids: ids)
        await refreshGroups()
    }

    func setAdmin(_ groupId: String, _ userId: String, admin: Bool) async throws {
        _ = try await api().setAdmin(groupId: groupId, userId: userId, admin: admin)
        await refreshGroups()
    }

    func removeMember(_ groupId: String, _ userId: String) async throws {
        try await api().removeMember(groupId: groupId, userId: userId)
        await refreshGroups()
    }

    func leaveGroup(_ groupId: String) async throws {
        try await api().leaveGroup(groupId: groupId)
        await refreshGroups()
    }

    func updateGroup(_ groupId: String, name: String? = nil, announcement: String? = nil, avatarPath: String? = nil) async throws {
        _ = try await api().updateGroup(groupId: groupId, name: name, announcement: announcement, avatarPath: avatarPath)
        await refreshGroups()
    }

    // MARK: - 安全码

    func safetyNumber(_ userId: String) async throws -> SafetyNumber {
        let s = try await api().safetyNumber(userId: userId)
        if s.verified { verifiedUsers.insert(userId) } else { verifiedUsers.remove(userId) }
        return s
    }

    func refreshVerified(_ userId: String) async {
        _ = try? await safetyNumber(userId)
    }

    func setVerified(_ userId: String, _ verified: Bool) throws {
        try api().setVerified(userId: userId, verified: verified)
        if verified { verifiedUsers.insert(userId) } else { verifiedUsers.remove(userId) }
    }

    func verifySafetyQr(_ userId: String, payload: String) throws -> Bool {
        try api().verifySafetyQr(userId: userId, payload: payload)
    }
}
