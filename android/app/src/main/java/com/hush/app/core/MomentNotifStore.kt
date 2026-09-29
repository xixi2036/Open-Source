package com.hush.app.core

import com.hush.app.util.net

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import uniffi.hushcore.MomentNotification

/** 朋友圈互动通知（不落本地库，登录后全量拉取 + 实时事件累积）。 */
object MomentNotifStore {
    private val _items = MutableStateFlow<List<MomentNotification>>(emptyList())
    val items: StateFlow<List<MomentNotification>> = _items

    private val _unseen = MutableStateFlow(0)
    val unseen: StateFlow<Int> = _unseen

    suspend fun refresh() {
        val list = net { HushCore.client.momentNotifications() }.sortedByDescending { it.createdAt }
        _items.value = list
        list.forEach { ContactsStore.put(it.from) }
        recount()
    }

    fun onNew(n: MomentNotification) {
        _items.update { old -> (listOf(n) + old.filterNot { it.id == n.id }) }
        ContactsStore.put(n.from)
        recount()
    }

    fun markAllSeen() {
        val newest = _items.value.maxOfOrNull { it.createdAt } ?: return
        HushCore.prefs.momentNotifSeenTs = maxOf(newest, HushCore.prefs.momentNotifSeenTs)
        recount()
    }

    private fun recount() {
        val seen = HushCore.prefs.momentNotifSeenTs
        _unseen.value = _items.value.count { it.createdAt > seen }
    }

    fun clear() {
        _items.value = emptyList()
        _unseen.value = 0
    }
}
