package com.hush.app.ui.chat

import androidx.compose.runtime.saveable.rememberSaveable
import com.hush.app.util.Savers
import android.widget.Toast
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CheckboxDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.ConvId
import com.hush.app.util.DebugTextSink
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.ConversationKind
import uniffi.hushcore.MessageItem
import uniffi.hushcore.MessageStatus

/** 转发目标：最近会话或还没有会话的好友。 */
private data class ForwardTarget(val convId: String, val title: String, val seed: String, val avatarId: String?, val group: Boolean, val recent: Boolean)

/**
 * U2：选择转发目标（最近会话 + 好友，可搜索、可多选），确认后调用 forward_message。
 * 部分目标发送失败时列出失败的会话，可一键重发（失败的那条在目标会话里也显示为失败，可单独重发）。
 */
@Composable
fun ForwardDialog(messages: List<MessageItem>, vm: ChatViewModel, onSent: () -> Unit = {}, onDismiss: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val friends by ContactsStore.friends.collectAsState()
    var convTargets by remember { mutableStateOf(emptyList<ForwardTarget>()) }
    // A4：搜索词与已选目标可跨 Activity 重建保留
    var q by rememberSaveable { mutableStateOf("") }
    var selected by rememberSaveable(stateSaver = Savers.OrderedStringSet) { mutableStateOf(linkedSetOf<String>()) }
    var busy by remember { mutableStateOf(false) }
    var failed by remember { mutableStateOf(emptyList<MessageItem>()) }

    LaunchedEffect(Unit) {
        convTargets = withContext(Dispatchers.IO) {
            runCatching { HushCore.client.conversations(false) }.getOrDefault(emptyList())
                .filter { it.active }
                .map { ForwardTarget(it.id, it.title, it.peerUserId ?: it.groupId ?: it.id, it.avatarId, it.kind == ConversationKind.GROUP, recent = true) }
        }
    }
    val targets = remember(convTargets, friends) {
        val known = convTargets.mapTo(HashSet()) { it.convId }
        convTargets + friends.filter { ConvId.direct(it.id) !in known }
            .map { ForwardTarget(ConvId.direct(it.id), it.name, it.id, it.avatarId, group = false, recent = false) }
    }
    val shown = targets.filter { q.isBlank() || it.title.contains(q.trim(), ignoreCase = true) }
    val titles = targets.associate { it.convId to it.title }

    fun send() {
        if (selected.isEmpty() || busy) return
        busy = true
        scope.launch {
            val out = vm.forwardMany(messages, selected.toList())
            busy = false
            if (out == null) return@launch
            val bad = out.filter { it.status == MessageStatus.FAILED }
            if (bad.isEmpty()) {
                Toast.makeText(context, "已转发", Toast.LENGTH_SHORT).show(); onSent(); onDismiss()
            } else failed = bad
        }
    }

    Dialog(onDismissRequest = onDismiss, properties = DialogProperties(usePlatformDefaultWidth = false, decorFitsSystemWindows = false)) {
        DebugTextSink { q = it }
        Column(Modifier.fillMaxSize().background(Hush.colors.background).statusBarsPadding().imePadding()) {
            HushTopBar(if (messages.size > 1) "转发 ${messages.size} 条消息" else "选择会话", onBack = onDismiss, actions = {
                TextButton(enabled = selected.isNotEmpty() && !busy, onClick = ::send, modifier = Modifier.testTag("forward_send")) {
                    Text(if (selected.isEmpty()) "发送" else "发送(${selected.size})")
                }
            })
            TextField(
                value = q, onValueChange = { q = it }, singleLine = true,
                placeholder = { Text("搜索") },
                leadingIcon = { Icon(Icons.Filled.Search, null) },
                shape = RoundedCornerShape(24.dp),
                colors = TextFieldDefaults.colors(
                    focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent,
                    focusedContainerColor = Hush.colors.surface, unfocusedContainerColor = Hush.colors.surface,
                ),
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp).testTag("forward_search"),
            )
            LazyColumn(Modifier.fillMaxSize()) {
                val recent = shown.filter { it.recent }
                val others = shown.filter { !it.recent }
                if (recent.isNotEmpty()) {
                    item { Section("最近聊天") }
                    items(recent, key = { "c" + it.convId }) { t -> TargetRow(t, t.convId in selected) { selected = toggle(selected, t.convId) } }
                }
                if (others.isNotEmpty()) {
                    item { Section("好友") }
                    items(others, key = { "f" + it.convId }) { t -> TargetRow(t, t.convId in selected) { selected = toggle(selected, t.convId) } }
                }
                if (shown.isEmpty()) item { Text("无结果", color = Hush.colors.textSecondary, modifier = Modifier.padding(24.dp)) }
            }
        }
    }

    if (failed.isNotEmpty()) {
        val names = failed.joinToString("、") { titles[it.convId] ?: "会话" }
        AlertDialog(
            onDismissRequest = { failed = emptyList(); onSent(); onDismiss() },
            title = { Text(if (failed.size >= selected.size) "转发失败" else "部分转发失败") },
            text = { Text("以下会话发送失败：$names\n可以现在重发，或稍后在对应会话中点击失败图标重发。") },
            confirmButton = {
                TextButton(enabled = !busy, modifier = Modifier.testTag("forward_retry"), onClick = {
                    busy = true
                    scope.launch {
                        val again = vm.resendAll(failed).filter { it.status == MessageStatus.FAILED }
                        busy = false
                        if (again.isEmpty()) {
                            Toast.makeText(context, "已转发", Toast.LENGTH_SHORT).show(); failed = emptyList(); onSent(); onDismiss()
                        } else failed = again
                    }
                }) { Text("重发", color = Hush.colors.accent) }
            },
            dismissButton = { TextButton(onClick = { failed = emptyList(); onSent(); onDismiss() }) { Text("关闭") } },
        )
    }
}

private fun toggle(set: LinkedHashSet<String>, id: String): LinkedHashSet<String> =
    LinkedHashSet(set).apply { if (!remove(id)) add(id) }

@Composable
private fun Section(t: String) {
    Text(t, style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.padding(start = 16.dp, top = 12.dp, bottom = 4.dp))
}

@Composable
private fun TargetRow(t: ForwardTarget, checked: Boolean, onToggle: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onToggle).padding(horizontal = 12.dp, vertical = 8.dp).testTag("fwd_${t.title}"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Checkbox(checked = checked, onCheckedChange = { onToggle() }, colors = CheckboxDefaults.colors(checkedColor = Hush.colors.accent))
        Spacer(Modifier.width(4.dp))
        Avatar(t.seed, t.title, t.avatarId, 44.dp, group = t.group)
        Spacer(Modifier.width(12.dp))
        Text(t.title, style = HushType.body, color = Hush.colors.textPrimary, maxLines = 1)
    }
    RowDivider(72)
}
