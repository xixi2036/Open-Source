package com.hush.app.ui.chats

import androidx.compose.runtime.saveable.rememberSaveable
import com.hush.app.util.Savers
import com.hush.app.util.net

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.PersonSearch
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.OutlinedTextFieldDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.FriendPickerList
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.theme.Hush
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** 发起单聊：选择一个好友。 */
@Composable
fun NewChatScreen(onBack: () -> Unit, onOpen: (String) -> Unit) {
    val friends by ContactsStore.friends.collectAsState()
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("发起单聊", onBack = onBack)
        if (friends.isEmpty()) EmptyState(Icons.Outlined.PersonSearch, "还没有好友，先去添加好友吧")
        else FriendPickerList(friends, emptySet(), multi = false) { p -> onOpen(HushCore.client.directConversationId(p.id)) }
    }
}

/** 新建群聊：多选好友 + 群名。 */
@Composable
fun NewGroupScreen(onBack: () -> Unit, onCreated: (String) -> Unit) {
    val friends by ContactsStore.friends.collectAsState()
    var selected by rememberSaveable(stateSaver = Savers.StringSet) { mutableStateOf(setOf<String>()) }
    var name by rememberSaveable { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    com.hush.app.util.DebugTextSink { name = it }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("新建群聊", onBack = onBack, actions = {
            TextButton(
                enabled = selected.isNotEmpty() && !busy,
                modifier = Modifier.testTag("create_group"),
                onClick = {
                    busy = true
                    scope.launch {
                        try {
                            val groupName = name.trim().ifEmpty {
                                friends.filter { it.id in selected }.take(3).joinToString("、") { it.name }
                            }
                            val g = withContext(Dispatchers.IO) { HushCore.client.createGroup(groupName, selected.toList()) }
                            onCreated(com.hush.app.util.ConvId.group(g.id))
                        } catch (e: Exception) {
                            error = e.userMessage(); busy = false
                        }
                    }
                },
            ) { Text(if (selected.isEmpty()) "完成" else "完成(${selected.size})") }
        })
        OutlinedTextField(
            value = name, onValueChange = { name = it }, singleLine = true, label = { Text("群名称（可选）") },
            shape = RoundedCornerShape(12.dp),
            colors = OutlinedTextFieldDefaults.colors(focusedBorderColor = Hush.colors.accent, unfocusedBorderColor = Hush.colors.divider),
            modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp).testTag("group_name"),
        )
        if (error != null) Text(error!!, color = Hush.colors.danger, modifier = Modifier.padding(horizontal = 16.dp))
        FriendPickerList(friends, selected, multi = true) { p ->
            selected = if (p.id in selected) selected - p.id else selected + p.id
        }
    }
}
