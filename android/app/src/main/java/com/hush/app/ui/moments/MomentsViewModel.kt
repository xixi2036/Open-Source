package com.hush.app.ui.moments

import com.hush.app.util.net

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.Moment

data class MomentsState(
    val items: List<Moment> = emptyList(),
    val refreshing: Boolean = false,
    val loadingMore: Boolean = false,
    val hasMore: Boolean = true,
    val loaded: Boolean = false,
)

/** 朋友圈动态流（userId 非空时为个人相册）。 */
/** [initialReloadKey]：创建时外部“已发表新动态”计数，之前的计数不再触发刷新（init 已加载）。 */
class MomentsViewModel(private val userId: String?, initialReloadKey: Int = 0) : ViewModel() {
    private var handledReloadKey = initialReloadKey

    private val core get() = HushCore.client
    private val _state = MutableStateFlow(MomentsState())
    val state: StateFlow<MomentsState> = _state
    private val _toast = com.hush.app.util.toastFlow()
    val toast: SharedFlow<String> = _toast

    init {
        refresh()
        viewModelScope.launch {
            HushCore.events.collect { ev -> if (ev is CoreEvent.MomentNotification) refreshQuiet() }
        }
    }

    private suspend fun fetch(before: Long?): List<Moment> = withContext(Dispatchers.IO) {
        val list = if (userId == null) net { core.momentsFeed(before) } else net { core.userMoments(userId, before) }
        list.forEach { m -> ContactsStore.put(m.author); m.likes.forEach(ContactsStore::put); m.comments.forEach { ContactsStore.put(it.author) } }
        list
    }

    /** 发表动态后计数加一：只在计数变化时刷新一次（切换 Tab 重新进入组合不再重复刷新）。 */
    fun reloadIfNeeded(key: Int) {
        if (key <= handledReloadKey) return
        handledReloadKey = key
        refresh()
    }

    fun refresh() {
        if (_state.value.refreshing) return
        _state.update { it.copy(refreshing = true) }
        viewModelScope.launch {
            try {
                val list = fetch(null)
                _state.update { it.copy(items = list, hasMore = list.size >= 20, refreshing = false, loaded = true) }
            } catch (e: Exception) {
                _state.update { it.copy(refreshing = false, loaded = true) }
                _toast.tryEmit(e.userMessage())
            }
        }
    }

    /** 静默刷新（新互动到达），保留已加载数量。 */
    private fun refreshQuiet() = viewModelScope.launch {
        runCatching { fetch(null) }.onSuccess { fresh ->
            _state.update { st ->
                val freshIds = fresh.map { it.id }.toSet()
                st.copy(items = fresh + st.items.filterNot { it.id in freshIds }.filter { it.createdAt < (fresh.lastOrNull()?.createdAt ?: Long.MAX_VALUE) })
            }
        }
    }

    fun loadMore() {
        val st = _state.value
        if (st.loadingMore || !st.hasMore || st.items.isEmpty() || st.refreshing) return
        _state.update { it.copy(loadingMore = true) }
        viewModelScope.launch {
            try {
                val more = fetch(st.items.last().createdAt)
                _state.update { s ->
                    val ids = s.items.map { it.id }.toSet()
                    s.copy(items = s.items + more.filterNot { it.id in ids }, hasMore = more.size >= 20, loadingMore = false)
                }
            } catch (e: Exception) {
                _state.update { it.copy(loadingMore = false) }
            }
        }
    }

    private fun replace(id: String, fresh: Moment?) {
        _state.update { st -> st.copy(items = if (fresh == null) st.items.filterNot { it.id == id } else st.items.map { if (it.id == id) fresh else it }) }
    }


    fun toggleLike(m: Moment) = viewModelScope.launch {
        val liked = m.likes.any { it.id == HushCore.myId }
        val me = HushCore.account.value?.profile
        // 乐观更新
        replace(m.id, m.copy(likes = if (liked) m.likes.filterNot { it.id == HushCore.myId } else m.likes + listOfNotNull(me)))
        try {
            withContext(Dispatchers.IO) { net { core.likeMoment(m.id, !liked) } }
        } catch (e: Exception) {
            replace(m.id, m); _toast.tryEmit(e.userMessage())
        }
    }

    fun comment(m: Moment, text: String, replyTo: String?) = viewModelScope.launch {
        try {
            val c = withContext(Dispatchers.IO) { core.commentMoment(m.id, text.trim(), replyTo) }
            val cur = _state.value.items.firstOrNull { it.id == m.id } ?: m
            replace(m.id, cur.copy(comments = cur.comments + c))
        } catch (e: Exception) {
            _toast.tryEmit(e.userMessage())
        }
    }

    fun deleteComment(m: Moment, commentId: String) = viewModelScope.launch {
        try {
            withContext(Dispatchers.IO) { net { core.deleteComment(commentId) } }
            val cur = _state.value.items.firstOrNull { it.id == m.id } ?: m
            replace(m.id, cur.copy(comments = cur.comments.filterNot { it.id == commentId }))
        } catch (e: Exception) {
            _toast.tryEmit(e.userMessage())
        }
    }

    fun delete(m: Moment) = viewModelScope.launch {
        try {
            withContext(Dispatchers.IO) { net { core.deleteMoment(m.id) } }
            replace(m.id, null)
        } catch (e: Exception) {
            _toast.tryEmit(e.userMessage())
        }
    }
}
