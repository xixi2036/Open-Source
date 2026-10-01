package com.hush.app.ui.nav

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.navigation.NavBackStackEntry
import androidx.navigation.NavType
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import androidx.navigation.navArgument
import com.hush.app.ui.call.rememberCallStarter
import com.hush.app.ui.chat.AddMembersScreen
import com.hush.app.ui.chat.ChatScreen
import com.hush.app.ui.chat.ConversationSettingsScreen
import com.hush.app.ui.chat.ImageViewerScreen
import com.hush.app.ui.chat.SafetyNumberScreen
import com.hush.app.ui.chats.ArchivedScreen
import com.hush.app.ui.chats.NewChatScreen
import com.hush.app.ui.chats.NewGroupScreen
import com.hush.app.ui.chats.SearchScreen
import com.hush.app.ui.contacts.AddFriendScreen
import com.hush.app.ui.contacts.FriendRequestsScreen
import com.hush.app.ui.contacts.GroupListScreen
import com.hush.app.ui.contacts.MyQrScreen
import com.hush.app.ui.contacts.ProfileScreen
import com.hush.app.ui.contacts.ScanScreen
import com.hush.app.ui.main.MainNav
import com.hush.app.ui.main.MainScreen
import com.hush.app.ui.me.AboutScreen
import com.hush.app.ui.me.BlockedScreen
import com.hush.app.ui.me.EditProfileScreen
import com.hush.app.ui.moments.ComposeMomentScreen
import com.hush.app.ui.moments.MomentDetailScreen
import com.hush.app.ui.moments.MomentNotificationsScreen
import com.hush.app.ui.moments.MomentsScreen
import com.hush.app.ui.theme.Hush

private fun NavBackStackEntry.arg(name: String): String = arguments?.getString(name) ?: ""

/** 登录后的全部页面。 */
@Composable
fun HushNavHost() {
    val nav = rememberNavController()
    val startCall = rememberCallStarter()
    var momentsReload by androidx.compose.runtime.saveable.rememberSaveable { mutableIntStateOf(0) }
    val pending by NavRequests.conversation.collectAsState()
    LaunchedEffect(pending) {
        pending?.let { conv ->
            NavRequests.conversation.value = null
            nav.popBackStack(Routes.MAIN, inclusive = false)
            nav.navigate(Routes.chat(conv))
        }
    }
    fun openChat(conv: String) = nav.navigate(Routes.chat(conv)) { launchSingleTop = true }
    fun back() { nav.popBackStack() }

    val mainNav = MainNav(
        openChat = ::openChat,
        search = { nav.navigate(Routes.SEARCH) },
        newChat = { nav.navigate(Routes.NEW_CHAT) },
        newGroup = { nav.navigate(Routes.NEW_GROUP) },
        addFriend = { nav.navigate(Routes.ADD_FRIEND) },
        archived = { nav.navigate(Routes.ARCHIVED) },
        requests = { nav.navigate(Routes.FRIEND_REQUESTS) },
        groups = { nav.navigate(Routes.GROUPS) },
        profile = { nav.navigate(Routes.profile(it)) },
        composeMoment = { nav.navigate(Routes.COMPOSE_MOMENT) },
        momentNotifs = { nav.navigate(Routes.MOMENT_NOTIFS) },
        image = { nav.navigate(Routes.image(it)) },
        editProfile = { nav.navigate(Routes.EDIT_PROFILE) },
        myQr = { nav.navigate(Routes.MY_QR) },
        blocked = { nav.navigate(Routes.BLOCKED) },
        about = { nav.navigate(Routes.ABOUT) },
    )

    NavHost(nav, startDestination = Routes.MAIN, modifier = Modifier.fillMaxSize().background(Hush.colors.background)) {
        composable(Routes.MAIN) { MainScreen(mainNav, momentsReload) }
        composable(
            Routes.CHAT,
            arguments = listOf(
                navArgument("convId") { type = NavType.StringType },
                navArgument("msg") { type = NavType.StringType; nullable = true; defaultValue = null },
            ),
        ) { e ->
            val conv = e.arg("convId")
            ChatScreen(
                conv, onBack = ::back, onSettings = { nav.navigate(Routes.chatSettings(conv)) },
                onOpenImage = { path, c -> nav.navigate(Routes.image(path, c)) }, onCall = startCall,
                focusMessageId = e.arguments?.getString("msg"),
            )
        }
        composable(Routes.CHAT_SETTINGS) { e ->
            val conv = e.arg("convId")
            Page {
                ConversationSettingsScreen(
                    conv, onBack = ::back,
                    onSafety = { nav.navigate(Routes.safety(it)) },
                    onProfile = { nav.navigate(Routes.profile(it)) },
                    onAddMembers = { nav.navigate(Routes.addMembers(it)) },
                    onCall = startCall,
                    onLeft = { nav.popBackStack(Routes.MAIN, inclusive = false) },
                )
            }
        }
        composable(Routes.SAFETY) { e -> Page { SafetyNumberScreen(e.arg("userId"), ::back) { nav.navigate(Routes.scan(it)) } } }
        composable(
            Routes.IMAGE,
            arguments = listOf(
                navArgument("path") { type = NavType.StringType; defaultValue = "" },
                navArgument("conv") { type = NavType.StringType; nullable = true; defaultValue = null },
            ),
        ) { e ->
            ImageViewerScreen(e.arg("path"), ::back, convId = e.arguments?.getString("conv"))
        }
        composable(Routes.ARCHIVED) { Page { ArchivedScreen(::back, ::openChat) } }
        composable(Routes.SEARCH) { Page { SearchScreen(::back, ::openChat) { conv, msg -> nav.navigate(Routes.chat(conv, msg)) } } }
        composable(Routes.NEW_CHAT) { Page { NewChatScreen(::back) { conv -> nav.popBackStack(); openChat(conv) } } }
        composable(Routes.NEW_GROUP) { Page { NewGroupScreen(::back) { conv -> nav.popBackStack(); openChat(conv) } } }
        composable(Routes.ADD_MEMBERS) { e -> Page { AddMembersScreen(e.arg("groupId"), ::back) } }
        composable(Routes.FRIEND_REQUESTS) { Page { FriendRequestsScreen(::back) { nav.navigate(Routes.profile(it)) } } }
        composable(Routes.GROUPS) { Page { GroupListScreen(::back, ::openChat) } }
        composable(Routes.PROFILE) { e ->
            Page {
                ProfileScreen(
                    e.arg("userId"), ::back, onMessage = ::openChat, onCall = startCall,
                    onMoments = { nav.navigate(Routes.userMoments(it)) },
                )
            }
        }
        composable(Routes.ADD_FRIEND) {
            Page { AddFriendScreen(::back, onProfile = { nav.navigate(Routes.profile(it)) }, onScan = { nav.navigate(Routes.scan()) }, onMyQr = { nav.navigate(Routes.MY_QR) }) }
        }
        composable(Routes.MY_QR) { Page { MyQrScreen(::back) } }
        composable(Routes.SCAN, arguments = listOf(navArgument("safetyUser") { type = NavType.StringType; nullable = true; defaultValue = null })) { e ->
            Page {
                ScanScreen(e.arguments?.getString("safetyUser"), ::back) { uid ->
                    nav.popBackStack(); nav.navigate(Routes.profile(uid))
                }
            }
        }
        composable(Routes.COMPOSE_MOMENT) { Page { ComposeMomentScreen(::back) { momentsReload++; nav.popBackStack() } } }
        composable(Routes.MOMENT_NOTIFS) {
            Page { MomentNotificationsScreen(::back, onProfile = { nav.navigate(Routes.profile(it)) }, onMoment = { nav.navigate(Routes.moment(it)) }) }
        }
        composable(Routes.MOMENT) { e ->
            Page {
                MomentDetailScreen(
                    e.arg("momentId"), ::back,
                    onImage = { nav.navigate(Routes.image(it)) }, onProfile = { nav.navigate(Routes.profile(it)) },
                )
            }
        }
        composable(Routes.USER_MOMENTS) { e ->
            MomentsScreen(
                e.arg("userId"), ::back, onCompose = { nav.navigate(Routes.COMPOSE_MOMENT) }, onNotifications = {},
                onImage = { nav.navigate(Routes.image(it)) }, onProfile = { nav.navigate(Routes.profile(it)) }, reloadKey = momentsReload,
            )
        }
        composable(Routes.EDIT_PROFILE) { Page { EditProfileScreen(::back) } }
        composable(Routes.BLOCKED) { Page { BlockedScreen(::back) } }
        composable(Routes.ABOUT) { Page { AboutScreen(::back) } }
    }
}

/** 普通页面：背景 + 状态栏留白。 */
@Composable
private fun Page(content: @Composable () -> Unit) {
    Box(Modifier.fillMaxSize().background(Hush.colors.background).statusBarsPadding()) { content() }
}
