package com.hush.app.ui.moments

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.VisibilityOff
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.theme.Hush
import com.hush.app.util.net
import com.hush.app.util.toastFlow
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.Comment
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.HushException
import uniffi.hushcore.Moment

data class MomentDetailState(
    val moment: Moment? = null,
    val loading: Boolean = true,
    /** 动态已删除或对我不可见（核心 moment(id) 返回 NotFound）。 */
    val unavailable: Boolean = false,
    val deleted: Boolean = false,
)

/** 单条动态详情（P-D：朋友圈通知点击进入）。 */
class MomentDetailViewModel(private val id: String) : ViewModel() {
    private val core get() = HushCore.client
    private val _state = MutableStateFlow(MomentDetailState())
    val state: StateFlow<MomentDetailState> = _state
    private val _toast = toastFlow()
    val toast: SharedFlow<String> = _toast

    init {
        load()
        viewModelScope.launch {
            HushCore.events.collect { ev -> if (ev is CoreEvent.MomentNotification && ev.notification.momentId == id) load(quiet = true) }
        }
    }

    fun load(quiet: Boolean = false) = viewModelScope.launch {
        if (!quiet) _state.update { it.copy(loading = true) }
        try {
            val m = withContext(Dispatchers.IO) { net { core.moment(id) } }
            ContactsStore.put(m.author); m.likes.forEach(ContactsStore::put); m.comments.forEach { ContactsStore.put(it.author) }
            _state.update { it.copy(moment = m, loading = false, unavailable = false) }
        } catch (e: HushException.NotFound) {
            _state.update { it.copy(moment = null, loading = false, unavailable = true) }
        } catch (e: Exception) {
            _state.update { it.copy(loading = false) }
            if (!quiet) _toast.tryEmit(e.userMessage())
        }
    }

    private fun set(m: Moment) = _state.update { it.copy(moment = m) }

    fun toggleLike(m: Moment) = viewModelScope.launch {
        val liked = m.likes.any { it.id == HushCore.myId }
        val me = HushCore.account.value?.profile
        set(m.copy(likes = if (liked) m.likes.filterNot { it.id == HushCore.myId } else m.likes + listOfNotNull(me)))
        try {
            withContext(Dispatchers.IO) { net { core.likeMoment(m.id, !liked) } }
        } catch (e: Exception) {
            set(m); _toast.tryEmit(e.userMessage())
        }
    }

    fun comment(m: Moment, text: String, replyTo: String?) = viewModelScope.launch {
        try {
            val c = withContext(Dispatchers.IO) { core.commentMoment(m.id, text.trim(), replyTo) }
            val cur = _state.value.moment ?: m
            set(cur.copy(comments = cur.comments + c))
        } catch (e: Exception) {
            _toast.tryEmit(e.userMessage())
        }
    }

    fun deleteComment(m: Moment, commentId: String) = viewModelScope.launch {
        try {
            withContext(Dispatchers.IO) { net { core.deleteComment(commentId) } }
            val cur = _state.value.moment ?: m
            set(cur.copy(comments = cur.comments.filterNot { it.id == commentId }))
        } catch (e: Exception) {
            _toast.tryEmit(e.userMessage())
        }
    }

    fun delete(m: Moment) = viewModelScope.launch {
        try {
            withContext(Dispatchers.IO) { net { core.deleteMoment(m.id) } }
            _state.update { it.copy(deleted = true) }
        } catch (e: Exception) {
            _toast.tryEmit(e.userMessage())
        }
    }
}

@Composable
fun MomentDetailScreen(momentId: String, onBack: () -> Unit, onImage: (String) -> Unit, onProfile: (String) -> Unit) {
    val vm: MomentDetailViewModel = viewModel(key = "moment_$momentId") { MomentDetailViewModel(momentId) }
    val s by vm.state.collectAsState()
    com.hush.app.util.ToastCollector(vm.toast)
    androidx.compose.runtime.LaunchedEffect(s.deleted) { if (s.deleted) onBack() }
    var commenting by remember { mutableStateOf<Pair<Moment, Comment?>?>(null) }
    val actions = remember(vm) {
        MomentActions(
            onLike = vm::toggleLike,
            onComment = { m, c -> commenting = m to c },
            onDeleteComment = { m, c -> vm.deleteComment(m, c.id) },
            onDelete = vm::delete,
            onImage = onImage,
            onProfile = onProfile,
        )
    }
    Box(Modifier.fillMaxSize().background(Hush.colors.background).imePadding()) {
        Column(Modifier.fillMaxSize()) {
            HushTopBar("详情", onBack = onBack)
            val m = s.moment
            when {
                m != null -> Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).testTag("moment_detail")) { MomentCard(m, actions) }
                s.unavailable -> EmptyState(Icons.Outlined.VisibilityOff, "该动态已不可见")
                s.loading -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { CircularProgressIndicator() }
            }
        }
        commenting?.let { (m, c) ->
            CommentBar(
                hint = if (c != null) "回复 ${c.author.name}" else "评论",
                modifier = Modifier.align(Alignment.BottomCenter).fillMaxWidth(),
                onSend = { text -> vm.comment(m, text, c?.author?.id); commenting = null },
                onDismiss = { commenting = null },
            )
        }
    }
}
