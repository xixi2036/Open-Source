package com.hush.app.ui.contacts

import androidx.compose.runtime.saveable.rememberSaveable
import com.hush.app.util.net

import android.widget.Toast
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Chat
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.PhotoLibrary
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
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
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.auth.PrimaryButton
import com.hush.app.ui.chat.CircleAction
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.SettingsGroup
import com.hush.app.ui.components.SettingsRow
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.components.rememberProfile
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.HushException

/** 个人资料页：发消息 / 语音 / 视频 / 朋友圈；非好友显示“添加到通讯录”。 */
@Composable
fun ProfileScreen(
    userId: String,
    onBack: () -> Unit,
    onMessage: (String) -> Unit,
    onCall: (String, Boolean) -> Unit,
    onMoments: (String) -> Unit,
) {
    val context = LocalContext.current
    val p = rememberProfile(userId)
    val friends by ContactsStore.friends.collectAsState()
    val isFriend = friends.any { it.id == userId }
    val isMe = userId == HushCore.myId
    var addDialog by rememberSaveable { mutableStateOf(false) }
    var sent by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    // 扫到的二维码可能来自其他服务器或已注销的账号：资料拉取返回 404 时提示“用户不存在”，不显示“添加到通讯录”
    var notFound by remember { mutableStateOf(false) }
    LaunchedEffect(userId) {
        ContactsStore.ensure(userId, force = true)
        if (userId != HushCore.myId && ContactsStore.cached(userId) == null) {
            val err = withContext(Dispatchers.IO) { runCatching { net { HushCore.client.getUser(userId) } }.exceptionOrNull() }
            notFound = err is HushException.NotFound || (err is HushException.Server && err.code == "not_found")
        }
    }

    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("个人资料", onBack = onBack)
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
            Column(Modifier.fillMaxWidth().padding(vertical = 20.dp), horizontalAlignment = Alignment.CenterHorizontally) {
                UserAvatar(userId, 96.dp)
                Spacer(Modifier.height(12.dp))
                Text(p?.name ?: "", style = HushType.title, color = Hush.colors.textPrimary, modifier = Modifier.testTag("profile_name"))
                p?.email?.takeIf { it.isNotBlank() }?.let { email ->
                    Spacer(Modifier.height(2.dp))
                    Text(email, style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.testTag("profile_email"))
                }
                if (!p?.about.isNullOrBlank()) {
                    Spacer(Modifier.height(6.dp))
                    Text(p!!.about, style = HushType.secondary, color = Hush.colors.textSecondary, textAlign = TextAlign.Center, modifier = Modifier.padding(horizontal = 32.dp))
                }
            }
            if (isFriend || isMe) {
                if (!isMe) {
                    Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.Center) {
                        CircleAction(Icons.AutoMirrored.Filled.Chat, "发消息") { onMessage(HushCore.client.directConversationId(userId)) }
                        CircleAction(Icons.Filled.Call, "语音通话") { onCall(userId, false) }
                        CircleAction(Icons.Filled.Videocam, "视频通话") { onCall(userId, true) }
                    }
                }
                SettingsGroup {
                    SettingsRow("朋友圈", icon = Icons.Filled.PhotoLibrary, onClick = { onMoments(userId) })
                }
            } else if (notFound && p == null) {
                Text(
                    "用户不存在", style = HushType.body, color = Hush.colors.textSecondary, textAlign = TextAlign.Center,
                    modifier = Modifier.fillMaxWidth().padding(24.dp).testTag("profile_not_found"),
                )
            } else {
                Column(Modifier.padding(24.dp)) {
                    PrimaryButton(if (sent) "已发送申请" else "添加到通讯录", onClick = { addDialog = true }, enabled = !sent, modifier = Modifier.testTag("add_to_contacts"))
                }
            }
        }
    }
    if (addDialog) {
        val myName = HushCore.account.collectAsState().value?.profile?.name ?: ""
        var msg by rememberSaveable { mutableStateOf("我是$myName") }
        AlertDialog(
            onDismissRequest = { addDialog = false },
            title = { Text("发送好友申请") },
            text = { OutlinedTextField(msg, { msg = it }, label = { Text("验证消息") }, modifier = Modifier.fillMaxWidth().testTag("request_msg")) },
            confirmButton = {
                TextButton(modifier = Modifier.testTag("send_request"), onClick = {
                    addDialog = false
                    scope.launch {
                        try {
                            withContext(Dispatchers.IO) { net { HushCore.client.sendFriendRequest(userId, msg) } }
                            sent = true
                            Toast.makeText(context, "好友申请已发送", Toast.LENGTH_SHORT).show()
                            withContext(Dispatchers.IO) {
                                runCatching { ContactsStore.refreshRequests() }
                                runCatching { ContactsStore.refreshFriends() }
                            }
                        } catch (e: Exception) {
                            Toast.makeText(context, e.userMessage(), Toast.LENGTH_SHORT).show()
                        }
                    }
                }) { Text("发送") }
            },
            dismissButton = { TextButton(onClick = { addDialog = false }) { Text("取消") } },
        )
    }
}
