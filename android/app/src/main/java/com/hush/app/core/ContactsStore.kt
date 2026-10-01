package com.hush.app.core

import com.hush.app.util.net

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import uniffi.hushcore.FriendRequests
import uniffi.hushcore.Profile
import java.util.concurrent.ConcurrentHashMap

/** 好友、好友申请与资料缓存。 */
object ContactsStore {
    private val core get() = HushCore.client

    private val _friends = MutableStateFlow<List<Profile>>(emptyList())
    val friends: StateFlow<List<Profile>> = _friends

    private val _requests = MutableStateFlow(FriendRequests(emptyList(), emptyList()))
    val requests: StateFlow<FriendRequests> = _requests

    /** user_id → Profile（好友、群成员、陌生人资料）。 */
    private val store = com.hush.app.util.BatchedMapFlow<String, Profile>(HushCore.scope)
    /** C2：写入按 50ms 时间窗合并后一次发射，不再每写一项就复制整张表。 */
    val profiles: StateFlow<Map<String, Profile>> = store.flow

    private val inFlight = ConcurrentHashMap.newKeySet<String>()

    fun pendingIncomingCount(r: FriendRequests = _requests.value): Int = r.incoming.count { it.status == "pending" }

    suspend fun refreshRequests() {
        _requests.value = net { core.listFriendRequests() }
        store.putAll((_requests.value.incoming.map { it.from } + _requests.value.outgoing.map { it.to }).associateBy { it.id })
    }

    /** 本机刚处理（接受/拒绝）的申请：立即更新状态，Tab 与“新的朋友”红点随之消失。 */
    fun markRequestHandled(id: String, status: String) {
        _requests.update { r -> r.copy(incoming = r.incoming.map { if (it.id == id) it.copy(status = status) else it }) }
    }

    suspend fun refreshFriends() {
        val list = net { core.refreshFriends() }
        setFriends(list)
    }

    fun reloadLocalFriends() {
        setFriends(core.friends())
    }

    private fun setFriends(list: List<Profile>) {
        // 与通讯录一致：按拼音/字母分组顺序（选择好友列表等处直接使用该顺序）
        _friends.value = com.hush.app.util.ContactIndex.group(list) { it.name }.flatMap { it.second }
        store.putAll(list.associateBy { it.id })
    }

    fun isFriend(userId: String) = _friends.value.any { it.id == userId }

    fun put(p: Profile) {
        store.put(p.id, p)
    }

    /** 已缓存的资料（不触发拉取）。 */
    fun cached(userId: String): Profile? = store[userId]

    /** 同步取名字；本地没有时异步拉取。 */
    fun nameOf(userId: String): String {
        if (userId.isEmpty()) return ""
        HushCore.account.value?.let { if (it.userId == userId) return it.profile.name }
        store[userId]?.let { return it.name }
        ensure(userId)
        return "用户"
    }

    fun profileOf(userId: String): Profile? {
        HushCore.account.value?.let { if (it.userId == userId) return it.profile }
        return store[userId] ?: run { ensure(userId); null }
    }

    /** 确保资料已加载：先查本地缓存，再请求服务端。 */
    fun ensure(userId: String, force: Boolean = false) {
        if (userId.isEmpty()) return
        if (!force && store.containsKey(userId)) return
        if (!inFlight.add(userId)) return
        HushCore.scope.launch(Dispatchers.IO) {
            try {
                if (!force) runCatching { core.cachedProfile(userId) }.getOrNull()?.let { put(it) }
                runCatching { net { core.getUser(userId) } }.getOrNull()?.let { put(it) }
            } finally {
                inFlight.remove(userId)
            }
        }
    }

    fun clear() {
        _friends.value = emptyList()
        _requests.value = FriendRequests(emptyList(), emptyList())
        store.clear()
    }
}
