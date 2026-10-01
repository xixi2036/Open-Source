package com.hush.app.ui.chat

import androidx.activity.compose.BackHandler
import androidx.core.net.toUri
import androidx.compose.foundation.interaction.DragInteraction
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.automirrored.filled.Forward
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.shadow
import com.hush.app.util.Savers
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
    onOpenImage: (path: String, convId: String) -> Unit,
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
    // A4：只保存待转发消息的 id，转发对话框可跨 Activity 重建保留
    var forwardIds by rememberSaveable(stateSaver = Savers.StringList) { mutableStateOf(emptyList<String>()) }
    var confirmDeleteSelected by rememberSaveable { mutableStateOf(false) }
    /** 待确认的非 https 链接。 */
    var confirmLink by rememberSaveable { mutableStateOf<String?>(null) }
    val downloads by vm.downloads.collectAsState()
    // 多选模式下返回键先退出多选
    BackHandler(enabled = s.selecting) { vm.clearSelection() }

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
    // 是否停在底部（reverseLayout：下标 0 是最新一条）
    val atBottom by remember { derivedStateOf { listState.firstVisibleItemIndex <= 1 } }
    /** 不在底部期间新到的消息数，用于“N 条新消息”悬浮按钮。 */
    var unseenNew by rememberSaveable { mutableIntStateOf(0) }
    var lastNewestTs by remember { mutableStateOf<Long?>(null) }
    // “跟随最新消息”：只有用户自己把列表拖离底部才取消；连续到达多条消息（或带大图的消息）导致
    // 列表暂时不在底部时仍然跟随，不误计为“新消息”
    var follow by remember { mutableStateOf(true) }
    LaunchedEffect(listState) {
        var userDragged = false
        launch { listState.interactionSource.interactions.collect { if (it is DragInteraction.Start) userDragged = true } }
        snapshotFlow { listState.isScrollInProgress }.collect { scrolling ->
            if (!scrolling && userDragged) {
                userDragged = false
                follow = listState.firstVisibleItemIndex <= 1
            }
        }
    }
    // 新消息到达且在跟随时保持在底部；否则不打断阅读，只累加计数
    val newest = s.messages.lastOrNull()
    val newestId = newest?.id
    LaunchedEffect(newestId) {
        val prevTs = lastNewestTs
        lastNewestTs = newest?.sortTs
        // 首次加载后若要定位到某条消息，不抢先滚到底部
        if (focusMessageId != null && !jumpDone) return@LaunchedEffect
        val newestOutgoing = newest?.isOutgoing == true
        if (newestOutgoing) follow = true
        if (follow) listState.animateScrollToItem(0)
        // 只有真的来了更新的消息才计数（末尾消息被删除 / 到期时 newestId 也会变）
        else if (prevTs != null && newest != null && newest.sortTs > prevTs) unseenNew++
    }
    LaunchedEffect(atBottom) { if (atBottom) unseenNew = 0 }
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
        // 定位到较早的消息后不再自动跟随最新消息
        follow = idx <= 1
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
            onImageClick = { m, i -> scope.launch { vm.download(m, i)?.let { onOpenImage(it, convId) } } },
            onVideoClick = { m, i ->
                scope.launch {
                    val path = vm.download(m, i) ?: return@launch
                    val body = m.body as? MessageBody.Attachments
                    playVideo(context, File(path), body?.items?.getOrNull(i)?.mime ?: "video/*")
                }
            },
            onRetryDownload = { m, i -> vm.retryDownload(m, i) },
            onLinkClick = { url -> if (Links.needsConfirm(url)) confirmLink = url else openLink(context, url) },
            onToggleSelect = { vm.toggleSelected(it) },
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
        if (s.selecting) SelectionTopBar(s.selected.size, onClose = { vm.clearSelection() })
        else ChatTopBar(s, vm, onBack, onSettings, onCall)
        HorizontalDivider(color = Hush.colors.divider, thickness = 0.5.dp)
        // 聊天页内的连接状态提示：没有网络 / 连接中 / 未连接时，发出的消息会停在“发送中”
        com.hush.app.ui.chats.ConnectionBanner()
        Box(Modifier.weight(1f)) {
            LazyColumn(state = listState, reverseLayout = true, modifier = Modifier.fillMaxSize().testTag("messages")) {
                items(rows.size, key = { rows[it].key }) { i ->
                    when (val r = rows[i]) {
                        is ChatRow.Day -> CenterNote(r.label, pill = true)
                        is ChatRow.Msg -> {
                            // 只为进入可见区域的消息自动下载图片/语音
                            LaunchedEffect(r.m.id) { vm.ensureMedia(r.m) }
                            // A6：移除（阅后即焚到期、删除）时不做淡出——淡出期间旧行还留在原位，会和补位上移的相邻行
                            // （例如“已设置阅后即焚”的系统提示）叠在一起；只保留位移动画
                            Box(Modifier.animateItem(fadeInSpec = null, fadeOutSpec = null)) {
                                MessageRow(
                                    r.m, s.isGroup, r.header, actions, highlight = r.m.id == highlightId,
                                    ctx = RowContext(downloads, s.selecting, r.m.id in s.selected),
                                )
                            }
                        }
                    }
                }
                if (s.loadingMore) item(key = "loading") {
                    Box(Modifier.fillMaxWidth().padding(8.dp), contentAlignment = Alignment.Center) { CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp) }
                }
            }
            // 回到底部 / N 条新消息
            if (!atBottom) {
                ScrollToBottomButton(unseenNew, Modifier.align(Alignment.BottomEnd).padding(end = 12.dp, bottom = 12.dp)) {
                    follow = true
                    scope.launch {
                        // 离底部很远时先跳到附近再平滑滚动，避免长距离动画
                        if (listState.firstVisibleItemIndex > 30) listState.scrollToItem(8)
                        listState.animateScrollToItem(0)
                    }
                }
            }
        }
        when {
            s.selecting -> SelectionBar(
                count = s.selected.size,
                onForward = {
                    val items = vm.selectedMessages()
                    val ok = items.filter(Selection::forwardable)
                    if (ok.isEmpty()) Toast.makeText(context, "所选消息不能转发", Toast.LENGTH_SHORT).show()
                    else {
                        if (ok.size < items.size) Toast.makeText(context, "${items.size - ok.size} 条消息不能转发，已忽略", Toast.LENGTH_SHORT).show()
                        forwardIds = ok.map { it.id }
                    }
                },
                onDelete = { confirmDeleteSelected = true },
            )
            s.summary?.active == false -> Box(Modifier.fillMaxWidth().background(Hush.colors.surface).padding(16.dp), contentAlignment = Alignment.Center) {
                Text("你已不在此群聊中", color = Hush.colors.textSecondary)
            }
            else -> Composer(vm, s.replyTo)
        }
    }

    sheetFor?.let { m ->
        MessageActionSheet(
            m, vm, onForward = { forwardIds = listOf(it.id) },
            onSelect = { vm.startSelection(it) }, onDismiss = { sheetFor = null },
        )
    }
    if (forwardIds.isNotEmpty()) {
        val ids = forwardIds
        val items = s.messages.filter { it.id in ids }
        when {
            // 取消转发时保留多选状态；转发完成后退出多选
            items.isNotEmpty() -> ForwardDialog(items, vm, onSent = { vm.clearSelection() }, onDismiss = { forwardIds = emptyList() })
            // 消息已不在（被撤回 / 到期）：关闭对话框
            s.loaded -> LaunchedEffect(ids) { forwardIds = emptyList() }
        }
    }
    if (confirmDeleteSelected) {
        com.hush.app.ui.components.ConfirmDialog(
            "删除 ${s.selected.size} 条消息？", "只从本机删除，对方的聊天记录不受影响", "删除", danger = true,
            onConfirm = { vm.deleteSelected() }, onDismiss = { confirmDeleteSelected = false },
        )
    }
    confirmLink?.let { url ->
        com.hush.app.ui.components.ConfirmDialog(
            "打开不安全的链接？", "该链接没有使用 https 加密，访问内容可能被窃听或篡改：\n$url", "仍然打开", danger = true,
            onConfirm = { openLink(context, url) }, onDismiss = { confirmLink = null },
        )
    }
}

/** “回到底部”悬浮按钮；不在底部期间有新消息时显示“N 条新消息”。 */
@Composable
private fun ScrollToBottomButton(unseen: Int, modifier: Modifier, onClick: () -> Unit) {
    Row(
        modifier.shadow(4.dp, RoundedCornerShape(22.dp)).clip(RoundedCornerShape(22.dp)).background(Hush.colors.surface)
            .clickable(onClickLabel = "回到底部", onClick = onClick)
            .padding(horizontal = if (unseen > 0) 14.dp else 10.dp, vertical = 10.dp)
            .testTag("scroll_to_bottom"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (unseen > 0) {
            Text("${if (unseen > 99) "99+" else unseen} 条新消息", style = HushType.secondary, color = Hush.colors.accent, modifier = Modifier.testTag("unseen_new"))
            Spacer(Modifier.width(4.dp))
        }
        Icon(Icons.Filled.KeyboardArrowDown, "回到底部", tint = if (unseen > 0) Hush.colors.accent else Hush.colors.textSecondary, modifier = Modifier.size(24.dp))
    }
}

/** 多选模式的顶栏。 */
@Composable
private fun SelectionTopBar(count: Int, onClose: () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        IconButton(onClick = onClose, modifier = Modifier.testTag("select_close")) { Icon(Icons.Filled.Close, "退出多选") }
        Text("已选择 $count 条", style = HushType.bodyMedium, color = Hush.colors.textPrimary, modifier = Modifier.weight(1f).padding(vertical = 13.dp).testTag("select_count"))
    }
}

/** 多选模式的底栏：批量转发 / 批量删除。 */
@Composable
private fun SelectionBar(count: Int, onForward: () -> Unit, onDelete: () -> Unit) {
    Column {
        HorizontalDivider(color = Hush.colors.divider, thickness = 0.5.dp)
        Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), horizontalArrangement = Arrangement.SpaceEvenly) {
            SelectionAction(Icons.AutoMirrored.Filled.Forward, "转发", count > 0, Hush.colors.accent, "select_forward", onForward)
            SelectionAction(Icons.Filled.Delete, "删除", count > 0, Hush.colors.danger, "select_delete", onDelete)
        }
    }
}

@Composable
private fun SelectionAction(icon: androidx.compose.ui.graphics.vector.ImageVector, label: String, enabled: Boolean, color: androidx.compose.ui.graphics.Color, tag: String, onClick: () -> Unit) {
    val tint = if (enabled) color else Hush.colors.textSecondary.copy(alpha = 0.5f)
    Column(
        Modifier.clip(RoundedCornerShape(10.dp)).clickable(enabled = enabled, onClick = onClick).padding(horizontal = 28.dp, vertical = 4.dp).testTag(tag),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Icon(icon, null, tint = tint)
        Text(label, style = HushType.secondary, color = tint)
    }
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

/** 视频附件：交给系统播放器（不弹选择器，直接用默认播放器）。 */
fun playVideo(context: android.content.Context, file: File, mime: String) {
    val uri = FileProvider.getUriForFile(context, "${context.packageName}.files", file)
    val intent = Intent(Intent.ACTION_VIEW).setDataAndType(uri, mime.takeIf { it.startsWith("video/") } ?: "video/*").addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
    try {
        context.startActivity(intent)
    } catch (_: ActivityNotFoundException) {
        Toast.makeText(context, "没有可以播放视频的应用", Toast.LENGTH_SHORT).show()
    }
}

/** 在浏览器中打开消息里的链接。 */
fun openLink(context: android.content.Context, url: String) {
    try {
        context.startActivity(Intent(Intent.ACTION_VIEW, url.toUri()).addCategory(Intent.CATEGORY_BROWSABLE))
    } catch (_: ActivityNotFoundException) {
        Toast.makeText(context, "没有可以打开链接的应用", Toast.LENGTH_SHORT).show()
    }
}
