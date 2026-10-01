package com.hush.app.ui.chats

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.MessageText
import com.hush.app.util.TimeFormat
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import uniffi.hushcore.ConversationKind
import uniffi.hushcore.ConversationSummary
import uniffi.hushcore.MessageItem

/** 搜索会话名与消息内容（search_messages）。 */
@Composable
fun SearchScreen(onBack: () -> Unit, onOpen: (String) -> Unit, onOpenMessage: (convId: String, messageId: String) -> Unit) {
    // rememberSaveable：从结果返回后保留搜索词与结果
    var q by rememberSaveable { mutableStateOf("") }
    var convs by remember { mutableStateOf(emptyList<ConversationSummary>()) }
    var msgs by remember { mutableStateOf(emptyList<MessageItem>()) }
    var allConvs by remember { mutableStateOf(emptyList<ConversationSummary>()) }
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) {
        if (q.isEmpty()) focus.requestFocus()
        allConvs = withContext(Dispatchers.IO) {
            runCatching { HushCore.client.conversations(false) + HushCore.client.conversations(true) }.getOrDefault(emptyList())
        }
    }
    com.hush.app.util.DebugTextSink { q = it }
    LaunchedEffect(q, allConvs) {
        val query = q.trim()
        if (query.isEmpty()) { convs = emptyList(); msgs = emptyList(); return@LaunchedEffect }
        delay(250)
        convs = allConvs.filter { it.title.contains(query, ignoreCase = true) }
        // 核心的异步搜索：输入变化时 LaunchedEffect 取消协程，正在进行的搜索随之取消
        msgs = runCatching { HushCore.client.searchMessagesAsync(query) }
            .onFailure { if (it is kotlinx.coroutines.CancellationException) throw it }
            .getOrDefault(emptyList())
    }
    val titles = remember(allConvs) { allConvs.associateBy { it.id } }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        Row(Modifier.fillMaxWidth().padding(top = 8.dp, end = 12.dp), verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "返回") }
            TextField(
                value = q, onValueChange = { q = it }, singleLine = true,
                placeholder = { Text("搜索会话与消息") },
                leadingIcon = { Icon(Icons.Filled.Search, null) },
                shape = RoundedCornerShape(24.dp),
                colors = TextFieldDefaults.colors(
                    focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent,
                    focusedContainerColor = Hush.colors.surface, unfocusedContainerColor = Hush.colors.surface,
                ),
                modifier = Modifier.weight(1f).focusRequester(focus),
            )
        }
        LazyColumn(Modifier.fillMaxSize()) {
            if (convs.isNotEmpty()) {
                item { SectionTitle("会话") }
                items(convs, key = { "c" + it.id }) { c ->
                    ResultRow(c.peerUserId ?: c.groupId ?: c.id, c.title, c.avatarId, c.kind == ConversationKind.GROUP, null, null) { onOpen(c.id) }
                }
            }
            if (msgs.isNotEmpty()) {
                item { SectionTitle("聊天记录") }
                items(msgs, key = { "m" + it.id }) { m ->
                    val c = titles[m.convId]
                    ResultRow(
                        c?.peerUserId ?: c?.groupId ?: m.convId, c?.title ?: "", c?.avatarId, c?.kind == ConversationKind.GROUP,
                        MessageText.preview(m, c?.kind == ConversationKind.GROUP), TimeFormat.conversation(m.sentAt),
                    ) { onOpenMessage(m.convId, m.id) }
                }
            }
            if (q.isNotBlank() && convs.isEmpty() && msgs.isEmpty()) {
                item { Text("无结果", color = Hush.colors.textSecondary, modifier = Modifier.padding(24.dp)) }
            }
        }
    }
}

@Composable
private fun SectionTitle(t: String) {
    Text(t, style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.padding(start = 16.dp, top = 16.dp, bottom = 4.dp))
}

@Composable
private fun ResultRow(seed: String, title: String, avatarId: String?, group: Boolean, sub: String?, time: String?, onClick: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
        Avatar(seed, title, avatarId, 44.dp, group = group)
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(title, style = HushType.bodyMedium, color = Hush.colors.textPrimary, maxLines = 1)
            if (sub != null) Text(sub, style = HushType.secondary, color = Hush.colors.textSecondary, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        if (time != null) Text(time, style = HushType.timestamp, color = Hush.colors.textSecondary)
    }
}
