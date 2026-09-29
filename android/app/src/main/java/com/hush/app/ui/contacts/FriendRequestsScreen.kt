package com.hush.app.ui.contacts

import com.hush.app.util.net

import android.widget.Toast
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.PersonAddAlt
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
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
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.FriendRequest

/** 新的朋友：收到的申请（接受/拒绝），以及已处理与我发出的申请。 */
@Composable
fun FriendRequestsScreen(onBack: () -> Unit, onProfile: (String) -> Unit) {
    val context = LocalContext.current
    val requests by ContactsStore.requests.collectAsState()
    val scope = rememberCoroutineScope()
    // 本页处理过的申请：id → 状态文案
    var handled by remember { mutableStateOf(mapOf<String, String>()) }
    LaunchedEffect(Unit) { withContext(Dispatchers.IO) { runCatching { ContactsStore.refreshRequests() } } }

    // 用应用级作用域：离开页面后请求仍会完成，不会因组合销毁而取消
    fun act(r: FriendRequest, accept: Boolean) = HushCore.scope.launch(Dispatchers.Main) {
        try {
            withContext(Dispatchers.IO) {
                if (accept) net { HushCore.client.acceptFriendRequest(r.id) } else net { HushCore.client.rejectFriendRequest(r.id) }
            }
            handled = handled + (r.id to if (accept) "已添加" else "已拒绝")
            ContactsStore.markRequestHandled(r.id, if (accept) "accepted" else "rejected")
            withContext(Dispatchers.IO) { runCatching { ContactsStore.refreshFriends() } }
        } catch (e: Exception) {
            Toast.makeText(context, e.userMessage(), Toast.LENGTH_SHORT).show()
        }
    }

    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("新的朋友", onBack = onBack)
        if (requests.incoming.isEmpty() && requests.outgoing.isEmpty() && handled.isEmpty()) {
            EmptyState(Icons.Outlined.PersonAddAlt, "暂无好友申请"); return
        }
        LazyColumn(Modifier.fillMaxSize()) {
            items(requests.incoming, key = { "in" + it.id }) { r ->
                val status = handled[r.id] ?: statusText(r.status)
                RequestRow(r.from.id, r.from.name, r.from.avatarId, r.message.ifBlank { "请求添加你为好友" }, { onProfile(r.from.id) }) {
                    if (status == null) {
                        OutlinedButton(onClick = { act(r, false) }, contentPadding = PaddingValues(horizontal = 12.dp), modifier = Modifier.height(34.dp).testTag("reject_${r.from.name}")) { Text("拒绝") }
                        Spacer(Modifier.width(8.dp))
                        Button(
                            onClick = { act(r, true) }, contentPadding = PaddingValues(horizontal = 12.dp), modifier = Modifier.height(34.dp).testTag("accept_${r.from.name}"),
                            colors = ButtonDefaults.buttonColors(containerColor = Hush.colors.accent),
                        ) { Text("接受") }
                    } else Text(status, color = Hush.colors.textSecondary, style = HushType.secondary)
                }
                RowDivider(72)
            }
            if (requests.outgoing.isNotEmpty()) {
                item { Text("我发出的申请", style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.padding(start = 16.dp, top = 16.dp, bottom = 4.dp)) }
                items(requests.outgoing, key = { "out" + it.id }) { r ->
                    RequestRow(r.to.id, r.to.name, r.to.avatarId, r.message.ifBlank { "请求添加对方为好友" }, { onProfile(r.to.id) }) {
                        Text(statusText(r.status) ?: "等待验证", color = Hush.colors.textSecondary, style = HushType.secondary)
                    }
                    RowDivider(72)
                }
            }
        }
    }
}

private fun statusText(s: String): String? = when (s) {
    "accepted" -> "已添加"
    "rejected" -> "已拒绝"
    else -> null
}

@Composable
private fun RequestRow(id: String, name: String, avatarId: String?, message: String, onClick: () -> Unit, trailing: @Composable () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 12.dp), verticalAlignment = Alignment.CenterVertically) {
        Avatar(id, name, avatarId, 48.dp)
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(name, style = HushType.bodyMedium, color = Hush.colors.textPrimary)
            Text(message, style = HushType.secondary, color = Hush.colors.textSecondary, maxLines = 2, overflow = TextOverflow.Ellipsis)
        }
        Spacer(Modifier.width(8.dp))
        Row(verticalAlignment = Alignment.CenterVertically) { trailing() }
    }
}
