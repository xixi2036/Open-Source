package com.hush.app.ui.chat

import android.content.ActivityNotFoundException
import android.content.Intent
import android.widget.Toast
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.VerifiedUser
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.FileProvider
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.viewmodel.compose.viewModel
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.TimeFormat
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withTimeoutOrNull
import kotlinx.coroutines.launch
import uniffi.hushcore.MessageBody
import uniffi.hushcore.MessageItem
import java.io.File

private sealed interface ChatRow {
    val key: String
    data class Msg(val m: MessageItem, val header: Boolean) : ChatRow { override val key get() = m.id }
    data class Day(val label: String, val ts: Long) : ChatRow { override val key get() = "day_$ts" }
}

@Composable
fun ChatScreen(
    convId: String,
    onBack: () -> Unit,
    onSettings: () -> Unit,
    onOpenImage: (String) -> Unit,
    onCall: (peerId: String, video: Boolean) -> Unit,
    focusMessageId: String? = null,
) {
    val context = LocalContext.current
    val app = context.applicationContext as android.app.Application
    val vm: ChatViewModel = viewModel(key = "chat_$convId") { ChatViewModel(app, convId) }
    val s by vm.state.collectAsState()
    val scope = rememberCoroutineScope()
    val listState = rememberLazyListState()
    var sheetFor by remember { mutableStateOf<MessageItem?>(null) }
    var forwardFor by remember { mutableStateOf<MessageItem?>(null) }

    // 页面可见性 → mark_read / 当前会话
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    DisposableEffect(lifecycle) {
        val obs = LifecycleEventObserver { _, e ->
            if (e == Lifecycle.Event.ON_RESUME) vm.setVisible(true)
            if (e == Lifecycle.Event.ON_PAUSE) vm.setVisible(false)
        }
        lifecycle.addObserver(obs)
        if (lifecycle.currentState.isAtLeast(Lifecycle.State.RESUMED)) vm.setVisible(true)
        onDispose { lifecycle.removeObserver(obs); vm.setVisible(false) }
    }
    com.hush.app.util.ToastCollector(vm.toast)

    val rows = remember(s.messages, s.isGroup) { buildRows(s.messages, s.isGroup) }
    /** 从搜索结果进入时的一次性定位是否已完成（旋转/重建后不重复定位）。 */
    var jumpDone by rememberSaveable { mutableStateOf(false) }
    // 新消息到达且在底部时保持在底部
    val newestId = s.messages.lastOrNull()?.id
    LaunchedEffect(newestId) {
        // 首次加载后若要定位到某条消息，不抢先滚到底部
        if (focusMessageId != null && !jumpDone) return@LaunchedEffect
        val newestOutgoing = s.messages.lastOrNull()?.isOutgoing == true
        if (newestOutgoing || listState.firstVisibleItemIndex <= 2) listState.animateScrollToItem(0)
    }
    // 上滑到顶加载更早：可见项与总项数取自同一次布局（不闭包组合期的 rows），
    // 总数变化后仍在顶部才继续加载下一页
    LaunchedEffect(listState, vm) {
        snapshotFlow {
            val info = listState.layoutInfo
            ChatPaging.nearTop(info.visibleItemsInfo.lastOrNull()?.index, info.totalItemsCount) to info.totalItemsCount
        }
            .distinctUntilChanged()
            .collect { (near, _) -> if (near) vm.loadMore() }
    }

    // 定位到某条消息（引用、搜索结果）：必要时向前加载，滚动到可见区域偏上位置并短暂高亮
    var highlightId by remember { mutableStateOf<String?>(null) }
    suspend fun jumpTo(id: String) {
        if (!vm.ensureLoaded(id)) {
            Toast.makeText(context, "原消息已不存在", Toast.LENGTH_SHORT).show(); return
        }
        val idx = buildRows(vm.state.value.messages, vm.state.value.isGroup).indexOfFirst { it is ChatRow.Msg && it.m.id == id }
        if (idx < 0) return
        // 刚加载的更早消息要等列表重组后才有对应的项，否则 scrollToItem 会被截断到旧的末项
        withTimeoutOrNull(2000) { snapshotFlow { listState.layoutInfo.totalItemsCount }.first { it > idx } }
        val viewport = listState.layoutInfo.viewportSize.height
        listState.scrollToItem(idx, -(viewport / 3))
        highlightId = id
        delay(1600)
        if (highlightId == id) highlightId = null
    }
    LaunchedEffect(focusMessageId, s.loaded) {
        if (focusMessageId == null || jumpDone || !s.loaded) return@LaunchedEffect
        jumpDone = true
        delay(50)
        jumpTo(focusMessageId)
    }

    val actions = remember(vm) {
        BubbleActions(
            onLongPress = { sheetFor = it },
            onQuoteClick = { id -> scope.launch { jumpTo(id) } },
            onImageClick = { m, i -> scope.launch { vm.download(m, i)?.let(onOpenImage) } },
            onFileClick = { m, i ->
                scope.launch {
                    val path = vm.download(m, i) ?: return@launch
                    val body = m.body as? MessageBody.Attachments
                    openFile(context, File(path), body?.items?.getOrNull(i)?.mime ?: "*/*")
                }
            },
            onResend = { vm.resend(it) },
            onReactionClick = { m, e -> vm.react(m, e) },
        )
    }

    Column(Modifier.fillMaxSize().background(Hush.colors.background).statusBarsPadding().navigationBarsPadding().imePadding()) {
        ChatTopBar(s, vm, onBack, onSettings, onCall)
        HorizontalDivider(color = Hush.colors.divider, thickness = 0.5.dp)
        Box(Modifier.weight(1f)) {
            LazyColumn(state = listState, reverseLayout = true, modifier = Modifier.fillMaxSize().testTag("messages")) {
                items(rows.size, key = { rows[it].key }) { i ->
                    when (val r = rows[i]) {
                        is ChatRow.Day -> CenterNote(r.label, pill = true)
                        is ChatRow.Msg -> {
                            // 只为进入可见区域的消息自动下载图片/语音
                            LaunchedEffect(r.m.id) { vm.ensureMedia(r.m) }
                            Box(Modifier.animateItem()) { MessageRow(r.m, s.isGroup, r.header, actions, highlight = r.m.id == highlightId) }
                        }
                    }
                }
                if (s.loadingMore) item(key = "loading") {
                    Box(Modifier.fillMaxWidth().padding(8.dp), contentAlignment = Alignment.Center) { CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp) }
                }
            }
        }
        if (s.summary?.active == false) {
            Box(Modifier.fillMaxWidth().background(Hush.colors.surface).padding(16.dp), contentAlignment = Alignment.Center) {
                Text("你已不在此群聊中", color = Hush.colors.textSecondary)
            }
        } else {
            Composer(vm, s.replyTo)
        }
    }

    sheetFor?.let { m -> MessageActionSheet(m, vm, onForward = { forwardFor = it }, onDismiss = { sheetFor = null }) }
    forwardFor?.let { m -> ForwardDialog(m, vm, onDismiss = { forwardFor = null }) }
}

private fun buildRows(messages: List<MessageItem>, isGroup: Boolean): List<ChatRow> {
    val out = ArrayList<ChatRow>(messages.size + 8)
    for (i in messages.indices.reversed()) {
        val m = messages[i]
        val older = messages.getOrNull(i - 1)
        val newDay = older == null || !TimeFormat.sameDay(older.sentAt, m.sentAt)
        val header = isGroup && !m.isOutgoing && (newDay || older!!.senderId != m.senderId || older.body is MessageBody.System || older.deleted)
        out += ChatRow.Msg(m, header)
        if (newDay) out += ChatRow.Day(TimeFormat.daySeparator(m.sentAt), m.sentAt)
    }
    return out
}

@Composable
private fun ChatTopBar(s: ChatState, vm: ChatViewModel, onBack: () -> Unit, onSettings: () -> Unit, onCall: (String, Boolean) -> Unit) {
    val summary = s.summary
    val title = when {
        s.isGroup -> "${s.group?.name ?: summary?.title ?: "群聊"} (${s.group?.members?.size ?: 0})"
        else -> summary?.title ?: ""
    }
    val subtitle = when {
        s.typingUsers.isNotEmpty() -> if (s.isGroup) "${s.typingUsers.joinToString("、") { com.hush.app.core.ContactsStore.nameOf(it) }} 正在输入…" else "正在输入…"
        (summary?.disappearingSecs ?: 0u) > 0u -> "⏱ ${TimeFormat.disappearing(summary!!.disappearingSecs.toInt())}"
        else -> null
    }
    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "返回") }
        Row(Modifier.weight(1f).clickable(onClick = onSettings).padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            Avatar(vm.peerId ?: vm.groupId ?: vm.convId, summary?.title ?: "", summary?.avatarId, 38.dp, group = s.isGroup)
            Spacer(Modifier.width(10.dp))
            Column {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(title, style = HushType.bodyMedium, color = Hush.colors.textPrimary, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f, fill = false).testTag("chat_title"))
                    if (s.verified) {
                        Spacer(Modifier.width(4.dp))
                        Icon(Icons.Filled.VerifiedUser, "已核验", tint = Hush.colors.success, modifier = Modifier.size(16.dp))
                    }
                }
                if (subtitle != null) Text(subtitle, style = HushType.secondary, color = if (s.typingUsers.isNotEmpty()) Hush.colors.accent else Hush.colors.textSecondary, maxLines = 1, modifier = Modifier.testTag("chat_subtitle"))
            }
        }
        if (!s.isGroup && vm.peerId != null) {
            IconButton(onClick = { onCall(vm.peerId, true) }, modifier = Modifier.testTag("video_call")) { Icon(Icons.Filled.Videocam, "视频通话") }
            IconButton(onClick = { onCall(vm.peerId, false) }, modifier = Modifier.testTag("voice_call")) { Icon(Icons.Filled.Call, "语音通话") }
        }
        IconButton(onClick = onSettings, modifier = Modifier.testTag("chat_more")) { Icon(Icons.Filled.MoreVert, "更多") }
    }
}

fun openFile(context: android.content.Context, file: File, mime: String) {
    val uri = FileProvider.getUriForFile(context, "${context.packageName}.files", file)
    val intent = Intent(Intent.ACTION_VIEW).setDataAndType(uri, mime).addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    try {
        context.startActivity(Intent.createChooser(intent, "打开文件"))
    } catch (_: ActivityNotFoundException) {
        Toast.makeText(context, "没有可以打开此文件的应用", Toast.LENGTH_SHORT).show()
    }
}
