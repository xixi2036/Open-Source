package com.hush.app.ui.main

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Chat
import androidx.compose.material.icons.automirrored.outlined.Chat
import androidx.compose.material.icons.filled.Camera
import androidx.compose.material.icons.filled.Contacts
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.outlined.Camera
import androidx.compose.material.icons.outlined.Contacts
import androidx.compose.material.icons.outlined.Person
import androidx.compose.material3.Badge
import androidx.compose.material3.BadgedBox
import androidx.compose.material3.Icon
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.NavigationBarItemDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.saveable.rememberSaveableStateHolder
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.runtime.remember
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.testTag
import androidx.lifecycle.viewmodel.compose.viewModel
import com.hush.app.core.ContactsStore
import com.hush.app.core.MomentNotifStore
import com.hush.app.ui.chats.ConversationListScreen
import com.hush.app.ui.chats.ConversationListViewModel
import com.hush.app.ui.contacts.ContactsScreen
import com.hush.app.ui.me.MeScreen
import com.hush.app.ui.moments.MomentsScreen
import com.hush.app.ui.theme.Hush

/** 主界面导航回调集合。 */
class MainNav(
    val openChat: (String) -> Unit,
    val search: () -> Unit,
    val newChat: () -> Unit,
    val newGroup: () -> Unit,
    val addFriend: () -> Unit,
    val archived: () -> Unit,
    val requests: () -> Unit,
    val groups: () -> Unit,
    val profile: (String) -> Unit,
    val composeMoment: () -> Unit,
    val momentNotifs: () -> Unit,
    val image: (String) -> Unit,
    val editProfile: () -> Unit,
    val myQr: () -> Unit,
    val blocked: () -> Unit,
    val about: () -> Unit,
)

private data class TabDef(val label: String, val selected: ImageVector, val normal: ImageVector)

private val tabs = listOf(
    TabDef("聊天", Icons.AutoMirrored.Filled.Chat, Icons.AutoMirrored.Outlined.Chat),
    TabDef("通讯录", Icons.Filled.Contacts, Icons.Outlined.Contacts),
    TabDef("朋友圈", Icons.Filled.Camera, Icons.Outlined.Camera),
    TabDef("我", Icons.Filled.Person, Icons.Outlined.Person),
)

@Composable
fun MainScreen(nav: MainNav, momentsReload: Int) {
    var tab by rememberSaveable { mutableIntStateOf(0) }
    // 各 Tab 的可保存状态（列表滚动位置等）在切换 Tab 时保留
    val tabStates = rememberSaveableStateHolder()
    val convVm: ConversationListViewModel = viewModel(key = "convs") { ConversationListViewModel(false) }
    val convs by convVm.state.collectAsState()
    val requests by ContactsStore.requests.collectAsState()
    val momentUnseen by MomentNotifStore.unseen.collectAsState()
    val unread = convs.items.filter { !it.muted }.sumOf { it.unreadCount.toInt() }
    val pending = ContactsStore.pendingIncomingCount(requests)

    // 底部导航栏高度（含系统导航栏）：Tab 内容区已在它上方，弹出键盘时 imePadding 只需再让出超出部分，
    // 否则朋友圈评论栏与键盘之间会空出一整条导航栏的高度
    val density = LocalDensity.current
    var bottomBarPx by remember { mutableIntStateOf(0) }
    Column(Modifier.fillMaxSize()) {
        Box(
            Modifier.weight(1f)
                .consumeWindowInsets(PaddingValues(bottom = with(density) { bottomBarPx.toDp() }))
                .then(if (tab == 2) Modifier else Modifier.statusBarsPadding()),
        ) {
            tabStates.SaveableStateProvider(tab) {
                when (tab) {
                    0 -> ConversationListScreen(nav.openChat, nav.search, nav.newChat, nav.newGroup, nav.addFriend, nav.archived)
                    1 -> ContactsScreen(nav.requests, nav.groups, nav.profile, nav.addFriend)
                    2 -> MomentsScreen(null, null, nav.composeMoment, nav.momentNotifs, nav.image, nav.profile, momentsReload)
                    3 -> MeScreen(nav.editProfile, nav.myQr, nav.blocked, nav.about)
                }
            }
        }
        NavigationBar(containerColor = Hush.colors.background, modifier = Modifier.onSizeChanged { bottomBarPx = it.height }) {
            tabs.forEachIndexed { i, t ->
                val badge = when (i) { 0 -> unread; 1 -> pending; 2 -> momentUnseen; else -> 0 }
                NavigationBarItem(
                    selected = tab == i,
                    onClick = { tab = i },
                    icon = {
                        BadgedBox(badge = { if (badge > 0) Badge(containerColor = Hush.colors.danger) { Text(if (badge > 99) "99+" else "$badge") } }) {
                            Icon(if (tab == i) t.selected else t.normal, t.label)
                        }
                    },
                    label = { Text(t.label) },
                    colors = NavigationBarItemDefaults.colors(
                        selectedIconColor = Hush.colors.accent, selectedTextColor = Hush.colors.accent,
                        indicatorColor = Hush.colors.accent.copy(alpha = 0.12f),
                        unselectedIconColor = Hush.colors.textSecondary, unselectedTextColor = Hush.colors.textSecondary,
                    ),
                    modifier = Modifier.testTag("tab_${t.label}"),
                )
            }
        }
    }
}
