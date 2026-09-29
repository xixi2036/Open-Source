package com.hush.app.ui.moments

import androidx.compose.foundation.background
import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.isImeVisible
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.ui.platform.LocalDensity
import com.hush.app.ui.components.LightStatusBarIcons
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.PhotoCamera
import androidx.compose.material.icons.outlined.PhotoLibrary
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.viewmodel.compose.viewModel
import com.hush.app.core.HushCore
import com.hush.app.core.MomentNotifStore
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.components.rememberProfile
import com.hush.app.ui.theme.AvatarPalette
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.DebugTextSink
import uniffi.hushcore.Comment
import uniffi.hushcore.Moment

/** 朋友圈 Tab（userId 为空）或个人相册页（userId 非空）。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MomentsScreen(
    userId: String?,
    onBack: (() -> Unit)?,
    onCompose: () -> Unit,
    onNotifications: () -> Unit,
    onImage: (String) -> Unit,
    onProfile: (String) -> Unit,
    reloadKey: Int = 0,
) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val vm: MomentsViewModel = viewModel(key = "moments_${userId ?: "feed"}") { MomentsViewModel(userId, reloadKey) }
    val s by vm.state.collectAsState()
    val unseen by MomentNotifStore.unseen.collectAsState()
    val listState = rememberLazyListState()
    var commenting by remember { mutableStateOf<Pair<Moment, Comment?>?>(null) }
    com.hush.app.util.ToastCollector(vm.toast)
    LaunchedEffect(reloadKey) { vm.reloadIfNeeded(reloadKey) }
    val nearEnd by remember { derivedStateOf { (listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0) >= listState.layoutInfo.totalItemsCount - 2 } }
    LaunchedEffect(nearEnd, s.items.size) { if (nearEnd && s.loaded) vm.loadMore() }

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
    // 封面在状态栏下方时用浅色状态栏图标；封面滚出后显示带背景的顶栏，内容不再与状态栏文字重叠
    val density = LocalDensity.current
    val collapsed by remember {
        derivedStateOf {
            val threshold = with(density) { (COVER_HEIGHT - 56).dp.roundToPx() }
            listState.firstVisibleItemIndex > 0 || listState.firstVisibleItemScrollOffset > threshold
        }
    }
    LightStatusBarIcons(!collapsed)
    Box(Modifier.fillMaxSize().background(Hush.colors.background).imePadding()) {
        PullToRefreshBox(isRefreshing = s.refreshing, onRefresh = vm::refresh, modifier = Modifier.fillMaxSize()) {
            LazyColumn(state = listState, modifier = Modifier.fillMaxSize().testTag("moments_list")) {
                item { Cover(userId ?: HushCore.myId, onBack, onCompose.takeIf { userId == null || userId == HushCore.myId }) }
                if (userId == null && unseen > 0) {
                    item {
                        Box(Modifier.fillMaxWidth().padding(top = 8.dp), contentAlignment = Alignment.Center) {
                            Text(
                                "$unseen 条新消息", color = Color.White, style = HushType.secondary,
                                modifier = Modifier.clip(RoundedCornerShape(6.dp)).background(Hush.colors.momentsNoticeBar).clickable(onClick = onNotifications)
                                    .padding(horizontal = 14.dp, vertical = 8.dp).testTag("moment_notif_banner"),
                            )
                        }
                    }
                }
                if (s.loaded && s.items.isEmpty()) {
                    item { Box(Modifier.height(320.dp)) { EmptyState(Icons.Outlined.PhotoLibrary, if (userId == null) "还没有动态，发一条分享生活吧" else "暂无动态") } }
                }
                items(s.items, key = { it.id }) { m ->
                    MomentCard(m, actions)
                    HorizontalDivider(color = Hush.colors.divider, thickness = 0.5.dp)
                }
                if (s.loadingMore) item {
                    Box(Modifier.fillMaxWidth().padding(12.dp), contentAlignment = Alignment.Center) { CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp) }
                }
                if (!s.hasMore && s.items.isNotEmpty()) item {
                    Text("没有更多了", style = HushType.secondary, color = Hush.colors.textSecondary, textAlign = TextAlign.Center, modifier = Modifier.fillMaxWidth().padding(16.dp))
                }
            }
        }
        AnimatedVisibility(collapsed, enter = fadeIn(), exit = fadeOut(), modifier = Modifier.align(Alignment.TopCenter)) {
            Column(Modifier.fillMaxWidth().background(Hush.colors.background).statusBarsPadding()) {
                Row(Modifier.fillMaxWidth().height(48.dp).padding(horizontal = 4.dp), verticalAlignment = Alignment.CenterVertically) {
                    if (onBack != null) IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "返回") }
                    else Spacer(Modifier.width(12.dp))
                    Text(
                        if (userId == null) "朋友圈" else rememberProfile(userId)?.name ?: "", style = HushType.title, color = Hush.colors.textPrimary,
                        maxLines = 1, modifier = Modifier.weight(1f),
                    )
                    if (userId == null || userId == HushCore.myId) {
                        IconButton(onClick = onCompose) { Icon(Icons.Filled.PhotoCamera, "发布动态") }
                    }
                }
                HorizontalDivider(color = Hush.colors.divider, thickness = 0.5.dp)
            }
        }
        commenting?.let { (m, c) ->
            CommentBar(
                hint = if (c != null) "回复 ${c.author.name}" else "评论",
                modifier = Modifier.align(Alignment.BottomCenter),
                onSend = { text -> vm.comment(m, text, c?.author?.id); commenting = null },
                onDismiss = { commenting = null },
            )
        }
    }
}

@Composable
private fun Cover(userId: String, onBack: (() -> Unit)?, onCompose: (() -> Unit)?) {
    val p = rememberProfile(userId)
    Box(Modifier.fillMaxWidth().height(COVER_HEIGHT.dp)) {
        Box(
            Modifier.fillMaxWidth().height((COVER_HEIGHT - 30).dp)
                .background(Brush.linearGradient(listOf(Hush.colors.accent, AvatarPalette[0], AvatarPalette[7]))),
        )
        Row(Modifier.fillMaxWidth().statusBarsPadding().padding(4.dp), verticalAlignment = Alignment.CenterVertically) {
            if (onBack != null) IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "返回", tint = Color.White) }
            Spacer(Modifier.weight(1f))
            if (onCompose != null) IconButton(onClick = onCompose, modifier = Modifier.testTag("compose_moment")) { Icon(Icons.Filled.PhotoCamera, "发布动态", tint = Color.White) }
        }
        Row(Modifier.align(Alignment.BottomEnd).padding(end = 16.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(p?.name ?: "", color = Color.White, style = HushType.title.copy(fontWeight = FontWeight.Bold), modifier = Modifier.offset(y = (-14).dp))
            Spacer(Modifier.width(12.dp))
            Box(Modifier.clip(CircleShape).background(Hush.colors.background).padding(2.dp)) {
                UserAvatar(userId, 64.dp)
            }
        }
    }
}

private const val COVER_HEIGHT = 300

@OptIn(ExperimentalLayoutApi::class)
@Composable
internal fun CommentBar(hint: String, modifier: Modifier, onSend: (String) -> Unit, onDismiss: () -> Unit) {
    var text by remember { mutableStateOf("") }
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    DebugTextSink { text = it }
    // 返回键或收起键盘即取消评论（不留一个悬空的输入栏）
    BackHandler(onBack = onDismiss)
    val imeVisible = WindowInsets.isImeVisible
    var imeShown by remember { mutableStateOf(false) }
    LaunchedEffect(imeVisible) {
        if (imeVisible) imeShown = true else if (imeShown) onDismiss()
    }
    Column(modifier.fillMaxWidth().background(Hush.colors.background).navigationBarsPadding()) {
        HorizontalDivider(color = Hush.colors.divider)
        Row(Modifier.fillMaxWidth().padding(8.dp), verticalAlignment = Alignment.CenterVertically) {
            BasicTextField(
                value = text, onValueChange = { text = it }, maxLines = 4,
                textStyle = HushType.body.copy(color = Hush.colors.textPrimary), cursorBrush = SolidColor(Hush.colors.accent),
                modifier = Modifier.weight(1f).focusRequester(focus).testTag("comment_input"),
                decorationBox = { inner ->
                    Box(Modifier.clip(RoundedCornerShape(18.dp)).background(Hush.colors.surface).padding(horizontal = 14.dp, vertical = 9.dp)) {
                        if (text.isEmpty()) Text(hint, color = Hush.colors.textSecondary, style = HushType.body)
                        inner()
                    }
                },
            )
            TextButton(onClick = onDismiss) { Text("取消", color = Hush.colors.textSecondary) }
            TextButton(onClick = { if (text.isNotBlank()) onSend(text) }, enabled = text.isNotBlank(), modifier = Modifier.testTag("comment_send")) { Text("发送") }
        }
    }
}
