package com.hush.app.ui.contacts

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
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
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Groups
import androidx.compose.material.icons.filled.PersonAdd
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.hush.app.core.ContactsStore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.LargeHeader
import com.hush.app.ui.components.CountBadge
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.ContactIndex

/** 通讯录 Tab：新的朋友 / 群聊 / 好友列表（按首字母分组）。 */
@Composable
fun ContactsScreen(onRequests: () -> Unit, onGroups: () -> Unit, onProfile: (String) -> Unit, onAdd: () -> Unit) {
    val friends by ContactsStore.friends.collectAsState()
    val requests by ContactsStore.requests.collectAsState()
    val pending = ContactsStore.pendingIncomingCount(requests)
    val grouped = remember(friends) { ContactIndex.group(friends) { it.name } }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        LargeHeader("通讯录") {
            IconButton(onClick = onAdd, modifier = Modifier.testTag("add_friend")) { Icon(Icons.Filled.PersonAdd, "添加好友") }
        }
        LazyColumn(Modifier.fillMaxSize()) {
            item {
                FixedRow(Icons.Filled.PersonAdd, Hush.colors.newFriends, "新的朋友", pending, onRequests, "new_friends")
                RowDivider(72)
                FixedRow(Icons.Filled.Groups, Hush.colors.success, "群聊", 0, onGroups, "group_list")
            }
            grouped.forEach { (letter, list) ->
                item(key = "h_$letter") {
                    Text(letter, style = HushType.secondary, color = Hush.colors.textSecondary,
                        modifier = Modifier.fillMaxWidth().background(Hush.colors.surface).padding(horizontal = 16.dp, vertical = 4.dp))
                }
                items(list, key = { it.id }) { p ->
                    Row(
                        Modifier.fillMaxWidth().clickable { onProfile(p.id) }.padding(horizontal = 16.dp, vertical = 10.dp).testTag("friend_${p.name}"),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Avatar(p.id, p.name, p.avatarId, 44.dp)
                        Spacer(Modifier.width(12.dp))
                        Text(p.name, style = HushType.body, color = Hush.colors.textPrimary)
                    }
                    RowDivider(72)
                }
            }
            item {
                Text("${friends.size} 位联系人", style = HushType.secondary, color = Hush.colors.textSecondary,
                    modifier = Modifier.fillMaxWidth().padding(20.dp), textAlign = androidx.compose.ui.text.style.TextAlign.Center)
            }
        }
    }
}

@Composable
private fun FixedRow(icon: ImageVector, color: Color, title: String, badge: Int, onClick: () -> Unit, tag: String) {
    Row(Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 16.dp, vertical = 10.dp).testTag(tag), verticalAlignment = Alignment.CenterVertically) {
        Box(Modifier.size(44.dp).clip(RoundedCornerShape(10.dp)).background(color), contentAlignment = Alignment.Center) {
            Icon(icon, null, tint = Color.White)
        }
        Spacer(Modifier.width(12.dp))
        Text(title, style = HushType.body, color = Hush.colors.textPrimary, modifier = Modifier.weight(1f))
        CountBadge(badge, Hush.colors.danger)
    }
}
