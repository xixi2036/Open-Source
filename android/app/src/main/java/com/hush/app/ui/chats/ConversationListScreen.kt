package com.hush.app.ui.chats

import androidx.compose.material.icons.filled.NotificationsOff
import androidx.compose.material.icons.filled.Close
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.foundation.clickable
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Chat
import androidx.compose.material.icons.filled.AddCircleOutline
import androidx.compose.material.icons.filled.Archive
import androidx.compose.material.icons.filled.GroupAdd
import androidx.compose.material.icons.filled.NotificationsOff
import androidx.compose.material.icons.filled.PersonAdd
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Timer
import androidx.compose.material.icons.outlined.ChatBubbleOutline
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import androidx.lifecycle.viewmodel.compose.viewModel
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.ConfirmDialog
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.LargeHeader
import com.hush.app.ui.components.UnreadBadge
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.ConversationPreview
import com.hush.app.util.MessageText
import com.hush.app.util.TimeFormat
import uniffi.hushcore.ConnectionState
import uniffi.hushcore.ConversationKind
import uniffi.hushcore.ConversationSummary

@Composable
fun ConversationListScreen(
    onOpen: (String) -> Unit,
    onSearch: () -> Unit,
    onNewChat: () -> Unit,
    onNewGroup: () -> Unit,
    onAddFriend: () -> Unit,
    onArchived: () -> Unit,
) {
    val vm: ConversationListViewModel = viewModel(key = "convs") { ConversationListViewModel(false) }
    val s by vm.state.collectAsState()
    var menu by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        LargeHeader("聊天") {
            IconButton(onClick = onSearch, modifier = Modifier.testTag("search")) { Icon(Icons.Filled.Search, "搜索") }
            Box {
                IconButton(onClick = { menu = true }, modifier = Modifier.testTag("new")) { Icon(Icons.Filled.AddCircleOutline, "新建") }
                DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                    DropdownMenuItem(text = { Text("发起单聊") }, leadingIcon = { Icon(Icons.AutoMirrored.Filled.Chat, null) }, onClick = { menu = false; onNewChat() })
                    DropdownMenuItem(text = { Text("新建群聊") }, leadingIcon = { Icon(Icons.Filled.GroupAdd, null) }, onClick = { menu = false; onNewGroup() })
                    DropdownMenuItem(text = { Text("添加好友") }, leadingIcon = { Icon(Icons.Filled.PersonAdd, null) }, onClick = { menu = false; onAddFriend() })
                }
            }
        }
        ConnectionBanner()
        NotificationPermissionBanner()
        ConversationList(s, vm, onOpen, footer = {
            if (s.archivedCount > 0) {
                Row(
                    Modifier.fillMaxWidth().combinedClickable(onClick = onArchived).padding(16.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(Icons.Filled.Archive, null, tint = Hush.colors.textSecondary)
                    Spacer(Modifier.width(12.dp))
                    Text("已归档的会话 (${s.archivedCount})", color = Hush.colors.textSecondary, style = HushType.body)
                }
            }
        }, empty = "还没有会话，去通讯录找朋友聊天吧")
    }
}

@Composable
fun ArchivedScreen(onBack: () -> Unit, onOpen: (String) -> Unit) {
    val vm: ConversationListViewModel = viewModel(key = "archived") { ConversationListViewModel(true) }
    val s by vm.state.collectAsState()
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("已归档的会话", onBack = onBack)
        ConversationList(s, vm, onOpen, footer = {}, empty = "没有已归档的会话")
    }
}

/**
 * 连接状态提示条（会话列表与聊天页共用）：没有网络 / 连接中 / 未连接。已连接时不显示。
 * 状态变化后稍等一下再显示，避免正常重连的一瞬间闪一下。
 */
@Composable
fun ConnectionBanner() {
    val conn by HushCore.connection.collectAsState()
    val online by com.hush.app.core.NetworkMonitor.online.collectAsState()
    val status = ConnectionStatus.of(online, conn == ConnectionState.CONNECTED, conn == ConnectionState.CONNECTING)
    var shown by remember { mutableStateOf(ConnectionStatus.OK) }
    LaunchedEffect(status) {
        if (status != ConnectionStatus.OK && status != ConnectionStatus.OFFLINE) kotlinx.coroutines.delay(1200)
        shown = status
    }
    val text = shown.text ?: return
    val warn = shown != ConnectionStatus.CONNECTING
    Box(
        Modifier.fillMaxWidth().background(if (warn) Hush.colors.danger.copy(alpha = 0.12f) else Hush.colors.accent.copy(alpha = 0.12f))
            .padding(vertical = 4.dp).testTag("connection_banner"),
        contentAlignment = Alignment.Center,
    ) {
        Text(text, style = HushType.secondary, color = if (warn) Hush.colors.danger else Hush.colors.accent)
    }
}

/** 连接状态文案（JVM 单测见 ConnectionStatusTest）。 */
enum class ConnectionStatus(val text: String?) {
    OK(null), OFFLINE("当前网络不可用"), CONNECTING("连接中…"), DISCONNECTED("未连接，正在重试…");

    companion object {
        fun of(online: Boolean, connected: Boolean, connecting: Boolean): ConnectionStatus = when {
            !online -> OFFLINE
            connected -> OK
            connecting -> CONNECTING
            else -> DISCONNECTED
        }
    }
}

/**
 * B4：系统通知权限被拒 / 被关闭时的应用内引导（会话列表顶部）。
 * 没有通知权限时后台的新消息与来电都不会有任何提示，必须让用户知道并能一键去开启。
 */
@Composable
fun NotificationPermissionBanner() {
    val context = androidx.compose.ui.platform.LocalContext.current
    var enabled by remember { mutableStateOf(com.hush.app.service.Notifier.notificationsEnabled(context)) }
    androidx.lifecycle.compose.LifecycleEventEffect(androidx.lifecycle.Lifecycle.Event.ON_RESUME) {
        enabled = com.hush.app.service.Notifier.notificationsEnabled(context)
        if (enabled) com.hush.app.service.Notifier.missedCallWithoutPermission.value = false
    }
    val missedCall by com.hush.app.service.Notifier.missedCallWithoutPermission.collectAsState()
    var dismissed by androidx.compose.runtime.saveable.rememberSaveable { mutableStateOf(false) }
    if (enabled || (dismissed && !missedCall)) return
    Row(
        Modifier.fillMaxWidth().background(Hush.colors.danger.copy(alpha = 0.10f))
            .clickable { runCatching { context.startActivity(com.hush.app.service.Notifier.appNotificationSettings(context)) } }
            .padding(start = 16.dp, top = 8.dp, bottom = 8.dp).testTag("notif_permission_banner"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(Icons.Filled.NotificationsOff, null, tint = Hush.colors.danger, modifier = Modifier.size(20.dp))
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f)) {
            Text(if (missedCall) "通知未开启，刚才有来电未能提醒" else "通知未开启", style = HushType.body, color = Hush.colors.textPrimary)
            Text("应用在后台时收不到新消息和来电提醒，点按去开启", style = HushType.secondary, color = Hush.colors.textSecondary)
        }
        IconButton(onClick = { dismissed = true; com.hush.app.service.Notifier.missedCallWithoutPermission.value = false }) {
            Icon(Icons.Filled.Close, "暂不开启", tint = Hush.colors.textSecondary)
        }
    }
}

@Composable
private fun ConversationList(
    s: ConvListState,
    vm: ConversationListViewModel,
    onOpen: (String) -> Unit,
    footer: @Composable () -> Unit,
    empty: String,
) {
    var confirmDelete by remember { mutableStateOf<ConversationSummary?>(null) }
    if (s.loaded && s.items.isEmpty()) {
        Column(Modifier.fillMaxSize()) {
            Box(Modifier.weight(1f)) { EmptyState(Icons.Outlined.ChatBubbleOutline, empty) }
            footer()
        }
        return
    }
    LazyColumn(Modifier.fillMaxSize().testTag("conv_list")) {
        items(s.items, key = { it.id }) { c ->
            ConversationRow(c, typing = c.id in s.typing, onOpen = { onOpen(c.id) }, vm = vm, onDelete = { confirmDelete = c })
        }
        item { footer() }
    }
    confirmDelete?.let { c ->
        ConfirmDialog(
            title = "删除「${c.title}」？", text = "删除后本机聊天记录将被清除", confirm = "删除", danger = true,
            onConfirm = { vm.delete(c.id) }, onDismiss = { confirmDelete = null },
        )
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun ConversationRow(c: ConversationSummary, typing: Boolean, onOpen: () -> Unit, vm: ConversationListViewModel, onDelete: () -> Unit) {
    var menu by remember { mutableStateOf(false) }
    val isGroup = c.kind == ConversationKind.GROUP
    // 只订阅最后一条消息发送者的资料：昵称加载后刷新预览（缺失时由 rememberProfile 在 LaunchedEffect 中拉取）
    val last = c.lastMessage
    val senderName = if (last != null && !last.isOutgoing && last.senderId.isNotEmpty()) {
        com.hush.app.ui.components.rememberName(last.senderId, "用户")
    } else ""
    Box {
        Row(
            Modifier.fillMaxWidth()
                .background(if (c.pinned) Hush.colors.surface else Hush.colors.background)
                .combinedClickable(onClick = onOpen, onLongClick = { menu = true })
                .padding(horizontal = 16.dp, vertical = 10.dp)
                .testTag("conv_${c.title}"),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Avatar(seed = c.peerUserId ?: c.groupId ?: c.id, name = c.title, avatarId = c.avatarId, size = 56.dp, group = isGroup)
            Spacer(Modifier.width(12.dp))
            Column(Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Row(Modifier.weight(1f), verticalAlignment = Alignment.CenterVertically) {
                        Text(c.title, style = HushType.bodyMedium, color = Hush.colors.textPrimary, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false))
                        if (c.muted) {
                            Spacer(Modifier.width(4.dp))
                            Icon(Icons.Filled.NotificationsOff, "免打扰", tint = Hush.colors.textSecondary, modifier = Modifier.size(14.dp))
                        }
                        if (c.disappearingSecs > 0u) {
                            Spacer(Modifier.width(4.dp))
                            Icon(Icons.Filled.Timer, "阅后即焚", tint = Hush.colors.textSecondary, modifier = Modifier.size(14.dp))
                        }
                    }
                    Spacer(Modifier.width(8.dp))
                    // 有草稿时核心的 lastActivityTs 是草稿时间（用于排序）；显示取最后一条消息的时间
                    Text(TimeFormat.conversation(ConversationPreview.displayTs(last?.sortTs, c.lastActivityTs)), style = HushType.timestamp.copy(fontSize = HushType.secondary.fontSize * 0.92f), color = Hush.colors.textSecondary)
                }
                Spacer(Modifier.height(4.dp))
                Row(verticalAlignment = Alignment.CenterVertically) {
                    val parts = ConversationPreview.of(
                        typing = typing, draft = c.draft, unreadMentions = c.unreadMentions.toInt(),
                        lastPreview = last?.let { MessageText.preview(it, isGroup) { senderName } }, active = c.active,
                    )
                    val preview = buildAnnotatedString {
                        parts.redPrefix?.let { withStyle(SpanStyle(color = Hush.colors.danger)) { append(it) } }
                        append(parts.body)
                    }
                    Text(
                        preview, style = HushType.secondary.copy(fontSize = HushType.secondary.fontSize * 1.08f),
                        color = if (parts.accent) Hush.colors.accent else Hush.colors.textSecondary,
                        maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f).testTag("conv_preview_${c.title}"),
                    )
                    UnreadBadge(c.unreadCount.toInt(), muted = c.muted)
                }
            }
        }
        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
            DropdownMenuItem(text = { Text(if (c.pinned) "取消置顶" else "置顶") }, onClick = { menu = false; vm.setPinned(c.id, !c.pinned) })
            DropdownMenuItem(text = { Text(if (c.muted) "取消免打扰" else "免打扰") }, onClick = { menu = false; vm.setMuted(c.id, !c.muted) })
            DropdownMenuItem(text = { Text(if (c.archived) "取消归档" else "归档") }, onClick = { menu = false; vm.setArchived(c.id, !c.archived) })
            DropdownMenuItem(text = { Text("删除", color = Hush.colors.danger) }, onClick = { menu = false; onDelete() })
        }
    }
}
