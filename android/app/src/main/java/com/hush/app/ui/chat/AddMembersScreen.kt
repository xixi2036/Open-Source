package com.hush.app.ui.chat

import com.hush.app.util.net

import android.widget.Toast
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
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
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.FriendPickerList
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.theme.Hush
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** 群聊添加成员。 */
@Composable
fun AddMembersScreen(groupId: String, onDone: () -> Unit) {
    val context = LocalContext.current
    val friends by ContactsStore.friends.collectAsState()
    var existing by remember { mutableStateOf(setOf<String>()) }
    var selected by remember { mutableStateOf(setOf<String>()) }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    LaunchedEffect(groupId) {
        existing = withContext(Dispatchers.IO) { runCatching { HushCore.client.groupInfo(groupId)?.members?.map { it.userId }?.toSet() }.getOrNull() ?: emptySet() }
    }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("添加成员", onBack = onDone, actions = {
            TextButton(enabled = selected.isNotEmpty() && !busy, onClick = {
                busy = true
                scope.launch {
                    try {
                        withContext(Dispatchers.IO) { net { HushCore.client.addMembers(groupId, selected.toList()) } }
                        onDone()
                    } catch (e: Exception) {
                        Toast.makeText(context, e.userMessage(), Toast.LENGTH_SHORT).show(); busy = false
                    }
                }
            }) { Text(if (selected.isEmpty()) "完成" else "完成(${selected.size})") }
        })
        FriendPickerList(friends, selected, multi = true, disabled = existing) { p ->
            selected = if (p.id in selected) selected - p.id else selected + p.id
        }
    }
}
