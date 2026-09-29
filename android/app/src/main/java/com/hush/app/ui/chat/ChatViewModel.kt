package com.hush.app.ui.chat

import com.hush.app.util.net

import android.app.Application
import android.net.Uri
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.service.Notifier
import com.hush.app.util.Coalescer
import com.hush.app.util.ConvId
import com.hush.app.util.toastFlow
import com.hush.app.util.MediaUtils
import com.hush.app.util.TypingTracker
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.AttachmentKind
import uniffi.hushcore.AttachmentMeta
import uniffi.hushcore.ConnectionState
import uniffi.hushcore.ConversationKind
import uniffi.hushcore.ConversationSummary
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.GroupInfo
import uniffi.hushcore.MessageBody
import uniffi.hushcore.MessageItem
import java.io.File
import androidx.compose.ui.text.TextRange
import androidx.compose.ui.text.input.TextFieldValue
import com.hush.app.util.MentionEditor
import com.hush.app.util.MentionSpan
import java.util.concurrent.ConcurrentHashMap

/** 输入框：文字、光标与 @提及区间（U4）；[mentionAt] 非空时显示成员选择（刚输入的 `@` 的位置）。 */
data class ComposerState(
    val value: TextFieldValue = TextFieldValue(""),
    val spans: List<MentionSpan> = emptyList(),
    val mentionAt: Int? = null,
)

data class ChatState(
    val summary: ConversationSummary? = null,
    val group: GroupInfo? = null,
    /** 按时间升序（与核心一致）。 */
    val messages: List<MessageItem> = emptyList(),
    val hasMore: Boolean = true,
    val loadingMore: Boolean = false,
    val typingUsers: Set<String> = emptySet(),
    val replyTo: MessageItem? = null,
    val verified: Boolean = false,
    val loaded: Boolean = false,
) {
    val isGroup: Boolean get() = summary?.kind == ConversationKind.GROUP
}

class ChatViewModel(app: Application, val convId: String) : AndroidViewModel(app) {
    private val core get() = HushCore.client
    private val _state = MutableStateFlow(ChatState())
    val state: StateFlow<ChatState> = _state
    private val _toast = toastFlow()
    val toast: SharedFlow<String> = _toast
    private val _composer = MutableStateFlow(ComposerState())
    val composer: StateFlow<ComposerState> = _composer
    /** 已从核心恢复过草稿（只在首次加载时恢复一次，之后不覆盖正在输入的内容）。 */
    private var draftRestored = false

    val peerId: String? = ConvId.peerOf(convId)
    val groupId: String? = ConvId.groupOf(convId)

    private var visible = false
    private val markReader = Coalescer(viewModelScope, 400) { runCatching { withContext(Dispatchers.IO) { core.markRead(convId) } } }
    private val summaryReloader = Coalescer(viewModelScope, 200) { loadSummary() }
    private val latestReloader = Coalescer(viewModelScope, 1000) { refreshLatest() }
    private val downloading = ConcurrentHashMap.newKeySet<String>()
    private var typingSent = false
    private var typingStopJob: Job? = null
    private val typing = TypingTracker()
    private var typingRefreshJob: Job? = null

    init {
        viewModelScope.launch {
            loadSummary()
            restoreDraft()
            val page = withContext(Dispatchers.IO) { runCatching { core.messages(convId, null, PAGE.toUInt()) }.getOrDefault(emptyList()) }
            _state.update { it.copy(messages = page, hasMore = page.size >= PAGE, loaded = true) }
            // 附件不在这里批量下载：列表项进入可见区域时由 ChatScreen 调用 ensureMedia
        }
        viewModelScope.launch { HushCore.events.collect(::onEvent) }
    }

    /** 安全码核验状态（进入页面与从安全码页返回时刷新：核验/取消核验在另一页面完成，没有对应事件）。 */
    private fun refreshVerified() {
        val uid = peerId ?: return
        viewModelScope.launch {
            val sn = withContext(Dispatchers.IO) { runCatching { net { core.safetyNumber(uid) } }.getOrNull() } ?: return@launch
            _state.update { it.copy(verified = sn.verified) }
        }
    }

    private suspend fun loadSummary() {
        val (s, g) = withContext(Dispatchers.IO) {
            val s = runCatching { core.conversation(convId) }.getOrNull()
            val g = groupId?.let { gid -> runCatching { core.groupInfo(gid) }.getOrNull() }
            s to g
        }
        g?.members?.forEach { ContactsStore.ensure(it.userId) }
        _state.update { it.copy(summary = s, group = g) }
    }

    /** U3：进入聊天页时恢复核心保存的草稿（群聊中按成员昵称重建 @ 区间，草稿本身只保存文字）。 */
    private fun restoreDraft() {
        if (draftRestored) return
        draftRestored = true
        val draft = _state.value.summary?.draft?.takeIf { it.isNotBlank() } ?: return
        if (_composer.value.value.text.isNotEmpty()) return
        val spans = if (_state.value.isGroup) MentionEditor.rehydrate(draft, mentionCandidates().map { it.first to it.second }) else emptyList()
        _composer.value = ComposerState(TextFieldValue(draft, TextRange(draft.length)), spans)
    }

    /** 离开聊天页（切到后台、返回）时保存草稿；空白时清除。内容未变时核心不刷新时间、不发事件。 */
    private fun saveDraft() {
        if (!draftRestored) return
        val text = _composer.value.value.text.takeIf { it.isNotBlank() }
        HushCore.scope.launch(Dispatchers.IO) { runCatching { core.setDraft(convId, text) } }
    }

    private fun onEvent(ev: CoreEvent) {
        when (ev) {
            is CoreEvent.MessageAdded -> if (ev.convId == convId) {
                _state.update { st ->
                    if (st.messages.any { it.id == ev.message.id }) st.copy(messages = st.messages.map { if (it.id == ev.message.id) ev.message else it })
                    else st.copy(messages = st.messages + ev.message)
                }
                if (!ev.message.isOutgoing) {
                    typing.clear(convId, ev.message.senderId); publishTyping()
                    if (visible) markReader.trigger()
                }
                ensureMedia(ev.message)
            }
            is CoreEvent.MessageUpdated -> if (ev.convId == convId) {
                _state.update { st -> st.copy(messages = st.messages.map { if (it.id == ev.message.id) ev.message else it }) }
            }
            is CoreEvent.MessageRemoved -> if (ev.convId == convId) {
                _state.update { st -> st.copy(messages = st.messages.filterNot { it.id == ev.messageId }) }
            }
            is CoreEvent.Typing -> if (ev.convId == convId && ev.userId != HushCore.myId) {
                typing.onTyping(convId, ev.userId, ev.typing); publishTyping()
            }
            is CoreEvent.ConversationsChanged, is CoreEvent.GroupsChanged -> summaryReloader.trigger()
            is CoreEvent.KeyChanged -> if (ev.userId == peerId) _state.update { it.copy(verified = false) }
            // 重连后用最新一页校正（事件总线溢出或断线期间可能漏掉增删改）
            is CoreEvent.ConnectionChanged -> if (ev.state == ConnectionState.CONNECTED) latestReloader.trigger()
            else -> Unit
        }
    }

    private suspend fun refreshLatest() {
        if (!_state.value.loaded) return
        val page = withContext(Dispatchers.IO) { runCatching { core.messages(convId, null, PAGE.toUInt()) }.getOrNull() } ?: return
        _state.update { st ->
            val merged = ChatPaging.mergeLatest(st.messages, page) { it.id }
            if (merged != null) st.copy(messages = merged) else st.copy(messages = page, hasMore = page.size >= PAGE)
        }
    }

    /** 发布当前输入状态，并在最近一条过期时再刷新一次。 */
    private fun publishTyping() {
        _state.update { it.copy(typingUsers = typing.users(convId)) }
        typingRefreshJob?.cancel()
        val wait = typing.millisUntilNextExpiry() ?: return
        typingRefreshJob = viewModelScope.launch { delay(wait + 50); publishTyping() }
    }

    /**
     * 图片/语音自动下载（机器人发的图片无缩略图）：只对新收到的消息和进入可见区域的消息调用，
     * 不对整页历史批量下载。
     */
    fun ensureMedia(m: MessageItem) {
        if (m.deleted) return
        val body = m.body as? MessageBody.Attachments ?: return
        body.items.forEachIndexed { i, a ->
            val need = a.localPath == null || !File(a.localPath!!).exists()
            if (need && (a.kind == AttachmentKind.IMAGE || a.kind == AttachmentKind.VOICE) && a.size < 20_000_000u) {
                val key = "${m.id}#$i"
                if (downloading.add(key)) {
                    viewModelScope.launch(Dispatchers.IO) {
                        runCatching { net { core.downloadAttachment(m.id, i.toUInt()) } }
                        delay(2000); downloading.remove(key)
                    }
                }
            }
        }
    }

    fun setVisible(v: Boolean) {
        visible = v
        HushCore.activeConversationId = if (v) convId else HushCore.activeConversationId.takeIf { it != convId }
        if (v) {
            refreshVerified()
            markReader.trigger()
            Notifier.cancelConversation(getApplication(), convId)
        } else {
            stopTyping()
            saveDraft()
        }
    }

    fun loadMore() {
        viewModelScope.launch { loadOlderPage() }
    }

    /** 加载更早的一页；正在加载或已无更多时直接返回 false。 */
    private suspend fun loadOlderPage(): Boolean {
        val st = _state.value
        if (!st.loaded || st.loadingMore || !st.hasMore || st.messages.isEmpty()) return false
        val oldest = st.messages.first()
        val before = oldest.sortTs
        _state.update { it.copy(loadingMore = true) }
        val page = withContext(Dispatchers.IO) { runCatching { core.messages(convId, before, PAGE.toUInt()) }.getOrDefault(emptyList()) }
        _state.update { s ->
            val known = s.messages.mapTo(HashSet()) { it.id }
            s.copy(messages = page.filterNot { it.id in known } + s.messages, hasMore = page.size >= PAGE, loadingMore = false)
        }
        return true
    }

    /**
     * 确保某条消息已加载到列表中（点击引用、从搜索结果进入时定位用）：
     * 不在已加载范围内时向前逐页加载，最多 [MAX_JUMP_PAGES] 页。
     */
    suspend fun ensureLoaded(messageId: String): Boolean {
        repeat(MAX_JUMP_PAGES) {
            val st = _state.value
            if (st.messages.any { it.id == messageId }) return true
            if (st.loaded && !st.hasMore) return false
            if (!st.loaded || st.loadingMore) { delay(100); return@repeat }
            loadOlderPage()
        }
        return _state.value.messages.any { it.id == messageId }
    }


    // ---------- 输入框 ----------
    /** 输入框变化：维护 @ 区间（删除时整体删除 @ 片段），群聊中刚输入 `@` 时弹出成员选择。 */
    fun onComposerChange(v: TextFieldValue) {
        val cur = _composer.value
        if (v.text == cur.value.text) { _composer.value = cur.copy(value = v); return }
        val e = MentionEditor.onEdit(cur.value.text, cur.spans, v.text, v.selection.min)
        val value = if (e.text == v.text) v else TextFieldValue(e.text, TextRange(e.cursor))
        val at = e.atTrigger?.takeIf { _state.value.isGroup }
        _composer.value = ComposerState(value, e.spans, at ?: cur.mentionAt?.takeIf { it < e.text.length && e.text[it] == '@' })
        onTextChanged(e.text)
    }

    /** 调试广播 / 外部整体替换文字（区间全部失效）。 */
    fun setComposerText(text: String) {
        _composer.value = ComposerState(TextFieldValue(text, TextRange(text.length)))
        onTextChanged(text)
    }

    /** 成员选择：(user_id, 昵称)，不含自己，不提供“所有人”。 */
    fun mentionCandidates(): List<Pair<String, String>> =
        _state.value.group?.members.orEmpty()
            .filter { it.userId != HushCore.myId }
            .map { it.userId to ContactsStore.nameOf(it.userId) }

    fun pickMention(userId: String, name: String) {
        val cur = _composer.value
        val at = cur.mentionAt ?: return
        val text = cur.value.text
        if (at !in text.indices || text[at] != '@') { _composer.value = cur.copy(mentionAt = null); return }
        val e = MentionEditor.insert(text, cur.spans, at, userId, name)
        _composer.value = ComposerState(TextFieldValue(e.text, TextRange(e.cursor)), e.spans)
    }

    /** 取消选择：保留已输入的 `@`。 */
    fun dismissMentionPicker() = _composer.update { it.copy(mentionAt = null) }

    // ---------- 输入状态 ----------
    fun onTextChanged(text: String) {
        if (text.isEmpty()) { stopTyping(); return }
        if (!typingSent) {
            typingSent = true
            viewModelScope.launch(Dispatchers.IO) { runCatching { core.setTyping(convId, true) } }
        }
        typingStopJob?.cancel()
        typingStopJob = viewModelScope.launch { delay(3000); stopTyping() }
    }

    private fun stopTyping() {
        typingStopJob?.cancel()
        if (typingSent) {
            typingSent = false
            HushCore.scope.launch(Dispatchers.IO) { runCatching { core.setTyping(convId, false) } }
        }
    }

    // ---------- 发送 ----------
    fun setReply(m: MessageItem?) = _state.update { it.copy(replyTo = m) }

    /** 发送输入框中的文字；带 @ 区间时走 sendTextWithMentions（核心在发送前清空草稿）。 */
    fun sendComposer() {
        val cur = _composer.value
        val (t, mentions) = MentionEditor.forSend(cur.value.text, cur.spans)
        if (t.isEmpty()) return
        _composer.value = ComposerState()
        val quote = _state.value.replyTo?.id
        _state.update { it.copy(replyTo = null) }
        stopTyping()
        viewModelScope.launch {
            try {
                val m = withContext(Dispatchers.IO) {
                    if (mentions.isEmpty()) core.sendText(convId, t, quote)
                    else core.sendTextWithMentions(convId, t, quote, mentions)
                }
                upsert(m)
            } catch (e: Exception) {
                _toast.tryEmit("发送失败：${e.userMessage()}")
            }
        }
    }

    private fun upsert(m: MessageItem) {
        _state.update { st ->
            if (st.messages.any { it.id == m.id }) st.copy(messages = st.messages.map { if (it.id == m.id) m else it })
            else st.copy(messages = st.messages + m)
        }
    }

    // 发送后（无论成败）删除 cacheDir 中的临时明文副本：核心在 sendAttachment 时已在 files/ 自存一份（重发也用它）
    fun sendImage(uri: Uri) = viewModelScope.launch {
        try {
            val m = withContext(Dispatchers.IO) {
                val ctx = getApplication<Application>()
                val picked = MediaUtils.copyToCache(ctx, uri)
                var file = picked.file
                try {
                    val isGif = picked.mime == "image/gif"
                    if (!isGif) file = MediaUtils.compressJpeg(ctx, picked.file.path, 2048, 85)
                    val (w, h) = MediaUtils.imageSize(file.path)
                    val meta = AttachmentMeta(if (isGif) picked.name else file.name, w.toUInt(), h.toUInt(), 0u, MediaUtils.thumbnail(file.path))
                    core.sendAttachment(convId, file.path, if (isGif) "image/gif" else "image/jpeg", AttachmentKind.IMAGE, null, meta)
                } finally {
                    MediaUtils.deleteTemp(picked.file, file)
                }
            }
            upsert(m)
        } catch (e: Exception) {
            _toast.tryEmit("图片发送失败：${e.userMessage()}")
        }
    }

    fun sendFile(uri: Uri) = viewModelScope.launch {
        try {
            val m = withContext(Dispatchers.IO) {
                val picked = MediaUtils.copyToCache(getApplication(), uri)
                try {
                    val meta = AttachmentMeta(picked.name, 0u, 0u, 0u, null)
                    core.sendAttachment(convId, picked.file.path, picked.mime, AttachmentKind.FILE, null, meta)
                } finally {
                    MediaUtils.deleteTemp(picked.file)
                }
            }
            upsert(m)
        } catch (e: Exception) {
            _toast.tryEmit("文件发送失败：${e.userMessage()}")
        }
    }

    fun sendVoice(file: File, durationMs: Long) = viewModelScope.launch {
        try {
            val m = withContext(Dispatchers.IO) {
                try {
                    val meta = AttachmentMeta("voice.m4a", 0u, 0u, durationMs.toULong(), null)
                    core.sendAttachment(convId, file.path, "audio/mp4", AttachmentKind.VOICE, null, meta)
                } finally {
                    MediaUtils.deleteTemp(file)
                }
            }
            upsert(m)
        } catch (e: Exception) {
            _toast.tryEmit("语音发送失败：${e.userMessage()}")
        }
    }

    // ---------- 消息操作 ----------
    fun react(m: MessageItem, emoji: String) = viewModelScope.launch(Dispatchers.IO) {
        val mine = m.reactions.firstOrNull { HushCore.myId in it.userIds }?.emoji
        runCatching { net { core.react(m.id, emoji, mine == emoji) } }.onFailure { _toast.tryEmit("回应失败") }
    }

    fun recall(m: MessageItem) = viewModelScope.launch(Dispatchers.IO) {
        runCatching { net { core.deleteForEveryone(m.id) } }.onFailure { _toast.tryEmit("撤回失败：${it.userMessage()}") }
    }

    fun deleteLocal(m: MessageItem) = viewModelScope.launch(Dispatchers.IO) {
        runCatching { core.deleteLocal(m.id) }
        _state.update { st -> st.copy(messages = st.messages.filterNot { it.id == m.id }) }
    }

    fun resend(m: MessageItem) = viewModelScope.launch {
        try {
            upsert(withContext(Dispatchers.IO) { core.resend(m.id) })
        } catch (e: Exception) {
            _toast.tryEmit("重发失败：${e.userMessage()}")
        }
    }

    /**
     * U2：转发到多个会话。返回发送失败的目标会话 id（那一条在目标会话中显示为失败，可重发）；
     * 整体失败（源消息不可转发等）时返回 null 并提示。
     */
    suspend fun forward(m: MessageItem, convIds: List<String>): List<MessageItem>? = try {
        val out = withContext(Dispatchers.IO) { core.forwardMessage(m.id, convIds) }
        out.filter { it.convId == convId }.forEach(::upsert)
        out
    } catch (e: Exception) {
        _toast.tryEmit("转发失败：${e.userMessage()}")
        null
    }

    /** 重发转发失败的消息（不在当前会话，按 id 重发）。 */
    suspend fun resendAll(items: List<MessageItem>): List<MessageItem> = withContext(Dispatchers.IO) {
        items.map { runCatching { core.resend(it.id) }.getOrElse { _ -> it } }
    }

    /** 下载附件并返回本地路径（查看大图、打开文件）。 */
    suspend fun download(m: MessageItem, index: Int): String? = withContext(Dispatchers.IO) {
        runCatching { net { core.downloadAttachment(m.id, index.toUInt()) } }.onFailure { _toast.tryEmit("下载失败：${it.userMessage()}") }.getOrNull()
    }

    override fun onCleared() {
        stopTyping()
        saveDraft()
        if (HushCore.activeConversationId == convId) HushCore.activeConversationId = null
    }

    companion object {
        const val PAGE = 40
        private const val MAX_JUMP_PAGES = 50
    }
}
