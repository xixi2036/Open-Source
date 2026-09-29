package com.hush.app.ui.chats

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.hush.app.core.HushCore
import com.hush.app.util.Coalescer
import com.hush.app.util.TypingTracker
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.ConversationSummary
import uniffi.hushcore.CoreEvent

data class ConvListState(
    val items: List<ConversationSummary> = emptyList(),
    val archivedCount: Int = 0,
    val typing: Set<String> = emptySet(),
    val loaded: Boolean = false,
)

/** 会话列表（archived=true 时为归档列表）。 */
class ConversationListViewModel(private val archived: Boolean) : ViewModel() {
    private val _state = MutableStateFlow(ConvListState())
    val state: StateFlow<ConvListState> = _state
    private val reloader = Coalescer(viewModelScope) { load() }
    /** 按 (会话, 用户) 记录并 15 秒过期，与聊天页一致；丢失 typing=false 时不会一直显示“正在输入…”。 */
    private val typingTracker = TypingTracker()
    private var typingRefreshJob: Job? = null

    init {
        reload()
        viewModelScope.launch {
            HushCore.events.collect { ev ->
                when (ev) {
                    is CoreEvent.MessageAdded -> {
                        if (!ev.message.isOutgoing) { typingTracker.clear(ev.convId, ev.message.senderId); publishTyping() }
                        reload()
                    }
                    is CoreEvent.ConversationsChanged, is CoreEvent.MessageUpdated,
                    is CoreEvent.MessageRemoved, is CoreEvent.GroupsChanged, is CoreEvent.FriendsChanged -> reload()
                    is CoreEvent.Typing -> if (ev.userId != HushCore.myId) {
                        typingTracker.onTyping(ev.convId, ev.userId, ev.typing); publishTyping()
                    }
                    else -> Unit
                }
            }
        }
    }

    private fun publishTyping() {
        _state.update { it.copy(typing = typingTracker.conversations()) }
        typingRefreshJob?.cancel()
        val wait = typingTracker.millisUntilNextExpiry() ?: return
        typingRefreshJob = viewModelScope.launch { delay(wait + 50); publishTyping() }
    }

    fun reload() = reloader.trigger()

    private suspend fun load() {
        val (list, archivedCount) = withContext(Dispatchers.IO) {
            runCatching {
                val l = HushCore.client.conversations(archived)
                val a = if (archived) 0 else HushCore.client.conversations(true).size
                l to a
            }.getOrDefault(emptyList<ConversationSummary>() to 0)
        }
        val sorted = list.sortedWith(compareByDescending<ConversationSummary> { it.pinned }.thenByDescending { it.lastActivityTs })
        _state.update { it.copy(items = sorted, archivedCount = archivedCount, loaded = true) }
    }

    private fun act(block: () -> Unit) {
        viewModelScope.launch(Dispatchers.IO) { runCatching(block); reload() }
    }

    fun setPinned(id: String, v: Boolean) = act { HushCore.client.setPinned(id, v) }
    fun setMuted(id: String, v: Boolean) = act { HushCore.client.setMuted(id, v) }
    fun setArchived(id: String, v: Boolean) = act { HushCore.client.setArchived(id, v) }
    /** U1：核心删除会话（本地消息、附件缓存、草稿与会话行）；群会话不退群，有新消息时重新出现。 */
    fun delete(id: String) = act { HushCore.client.deleteConversation(id) }
}
