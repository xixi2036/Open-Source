package com.hush.app.ui.moments

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Favorite
import androidx.compose.material.icons.outlined.NotificationsNone
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.hush.app.core.MomentNotifStore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.TimeFormat
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import uniffi.hushcore.MomentNotificationKind

/** 朋友圈互动通知列表。 */
@Composable
fun MomentNotificationsScreen(onBack: () -> Unit, onProfile: (String) -> Unit, onMoment: (String) -> Unit) {
    val items by MomentNotifStore.items.collectAsState()
    LaunchedEffect(Unit) { withContext(Dispatchers.IO) { runCatching { MomentNotifStore.refresh() } } }
    DisposableEffect(Unit) { onDispose { MomentNotifStore.markAllSeen() } }
    LaunchedEffect(items.size) { MomentNotifStore.markAllSeen() }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("消息", onBack = onBack)
        if (items.isEmpty()) { EmptyState(Icons.Outlined.NotificationsNone, "暂无互动消息"); return }
        LazyColumn {
            items(items, key = { it.id }) { n ->
                // 点击整行进入该条动态详情（P-D）；点头像看对方资料
                Row(
                    Modifier.fillMaxWidth().clickable { onMoment(n.momentId) }.padding(horizontal = 16.dp, vertical = 12.dp).testTag("moment_notif_${n.id}"),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Avatar(n.from.id, n.from.name, n.from.avatarId, 44.dp, modifier = Modifier.clickable { onProfile(n.from.id) })
                    Spacer(Modifier.width(12.dp))
                    Column(Modifier.weight(1f)) {
                        Text(n.from.name, style = HushType.bodyMedium, color = Hush.colors.accent)
                        if (n.kind == MomentNotificationKind.LIKE) {
                            Icon(Icons.Filled.Favorite, "赞了你", tint = Hush.colors.danger, modifier = Modifier.size(16.dp))
                        } else {
                            Text(n.text ?: "评论了你", style = HushType.body, color = Hush.colors.textPrimary)
                        }
                        Text(TimeFormat.relative(n.createdAt), style = HushType.secondary, color = Hush.colors.textSecondary)
                    }
                }
                RowDivider(72)
            }
        }
    }
}
