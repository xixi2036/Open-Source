package com.hush.app.ui.chat

import com.hush.app.util.net

import android.app.Application
import android.net.Uri
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.util.Coalescer
import com.hush.app.util.ConvId
import com.hush.app.util.toastFlow
import com.hush.app.util.MediaUtils
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.ConversationSummary
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.GroupInfo
import uniffi.hushcore.GroupRole

data class ConvSettingsState(
    val summary: ConversationSummary? = null,
    val group: GroupInfo? = null,
    val blocked: Boolean = false,
    val busy: Boolean = false,
) {
    val myRole: GroupRole? get() = group?.members?.firstOrNull { it.userId == HushCore.myId }?.role
    val isAdmin: Boolean get() = myRole == GroupRole.OWNER || myRole == GroupRole.ADMIN
    val isOwner: Boolean get() = myRole == GroupRole.OWNER
}

/** 会话设置（单聊/群聊共用）。 */
class ConversationSettingsViewModel(app: Application, val convId: String) : AndroidViewModel(app) {
    private val core get() = HushCore.client
    val peerId: String? = ConvId.peerOf(convId)
    val groupId: String? = ConvId.groupOf(convId)

    private val _state = MutableStateFlow(ConvSettingsState())
    val state: StateFlow<ConvSettingsState> = _state
    private val _toast = toastFlow()
    val toast: SharedFlow<String> = _toast
    /** 需要离开页面（退群、删好友）。 */
    val finished = MutableStateFlow<String?>(null)

    private val reloader = Coalescer(viewModelScope) { load() }

    init {
        reloader.trigger()
        peerId?.let { ContactsStore.ensure(it, force = true) }
        groupId?.let { gid -> viewModelScope.launch(Dispatchers.IO) { runCatching { net { core.refreshGroups() } }; reloader.trigger() } }
        viewModelScope.launch {
            HushCore.events.collect { ev ->
                if (ev is CoreEvent.ConversationsChanged || ev is CoreEvent.GroupsChanged || ev is CoreEvent.FriendsChanged) reloader.trigger()
            }
        }
    }

    private suspend fun load() {
        val st = withContext(Dispatchers.IO) {
            val s = runCatching { core.conversation(convId) }.getOrNull()
            val g = groupId?.let { runCatching { core.groupInfo(it) }.getOrNull() }
            val b = peerId?.let { p -> runCatching { core.blocked().contains(p) }.getOrDefault(false) } ?: false
            ConvSettingsState(s, g, b)
        }
        st.group?.members?.forEach { ContactsStore.ensure(it.userId) }
        _state.update { st.copy(busy = it.busy) }
    }

    private fun run(okMsg: String? = null, block: suspend () -> Unit) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
            try {
                withContext(Dispatchers.IO) { block() }
                okMsg?.let { _toast.tryEmit(it) }
            } catch (e: Exception) {
                _toast.tryEmit(e.userMessage())
            }
            _state.update { it.copy(busy = false) }
            reloader.trigger()
        }
    }

    fun setMuted(v: Boolean) = run { core.setMuted(convId, v) }
    fun setPinned(v: Boolean) = run { core.setPinned(convId, v) }
    fun setDisappearing(secs: Int) = run { net { core.setDisappearing(convId, secs.toUInt()) } }
    fun clear() = run("已清空聊天记录") { core.clearConversation(convId) }

    fun toggleBlock() = run {
        val p = peerId ?: return@run
        if (_state.value.blocked) net { core.unblock(p) } else net { core.block(p) }
    }

    fun removeFriend() = run {
        net { core.removeFriend(peerId ?: return@run) }
        ContactsStore.reloadLocalFriends()
        finished.value = "deleted"
    }

    fun rename(name: String) = run { net { core.updateGroup(groupId!!, name.trim(), null, null) } }
    fun setAnnouncement(text: String) = run { net { core.updateGroup(groupId!!, null, text.trim(), null) } }
    fun setGroupAvatar(uri: Uri) = run {
        val small = MediaUtils.prepareAvatar(getApplication(), uri)
        try {
            net { core.updateGroup(groupId!!, null, null, small.path) }
        } finally {
            MediaUtils.deleteTemp(small)
        }
    }
    fun removeMember(uid: String) = run("已移出群聊") { net { core.removeMember(groupId!!, uid) } }
    fun setAdmin(uid: String, admin: Boolean) = run(if (admin) "已设为管理员" else "已取消管理员") { net { core.setAdmin(groupId!!, uid, admin) } }
    fun leave() = run {
        net { core.leaveGroup(groupId!!) }
        finished.value = "left"
    }
}
