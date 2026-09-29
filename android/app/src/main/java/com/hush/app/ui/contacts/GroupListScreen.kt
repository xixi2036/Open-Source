package com.hush.app.ui.contacts

import com.hush.app.util.net

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
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Groups
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.GroupInfo

@Composable
fun GroupListScreen(onBack: () -> Unit, onOpen: (String) -> Unit) {
    var groups by remember { mutableStateOf(emptyList<GroupInfo>()) }
    LaunchedEffect(Unit) {
        suspend fun load() { groups = withContext(Dispatchers.IO) { runCatching { HushCore.client.groups().filter { it.active } }.getOrDefault(emptyList()) } }
        load()
        withContext(Dispatchers.IO) { runCatching { net { HushCore.client.refreshGroups() } } }
        load()
        HushCore.events.collect { if (it is CoreEvent.GroupsChanged) load() }
    }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("群聊", onBack = onBack)
        if (groups.isEmpty()) { EmptyState(Icons.Outlined.Groups, "还没有加入群聊"); return }
        LazyColumn {
            items(groups, key = { it.id }) { g ->
                Row(Modifier.fillMaxWidth().clickable { onOpen(com.hush.app.util.ConvId.group(g.id)) }.padding(horizontal = 16.dp, vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
                    Avatar(g.id, g.name, g.avatarId, 44.dp, group = true)
                    Spacer(Modifier.width(12.dp))
                    Text(g.name, style = HushType.body, color = Hush.colors.textPrimary, modifier = Modifier.weight(1f))
                    Text("${g.members.size} 人", style = HushType.secondary, color = Hush.colors.textSecondary)
                }
                RowDivider(72)
            }
        }
    }
}
