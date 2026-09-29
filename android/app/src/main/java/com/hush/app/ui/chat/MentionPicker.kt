package com.hush.app.ui.chat

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType

/** U4：群聊输入 `@` 后弹出的成员选择（可搜索；不含自己，不提供“所有人”）。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MentionPicker(vm: ChatViewModel) {
    val st by vm.state.collectAsState()
    val profiles by ContactsStore.profiles.collectAsState()
    var q by remember { mutableStateOf("") }
    val members = st.group?.members.orEmpty().filter { it.userId != HushCore.myId }
        .map { m -> m.userId to (profiles[m.userId]?.name ?: "成员") }
        .filter { q.isBlank() || it.second.contains(q.trim(), ignoreCase = true) }
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    ModalBottomSheet(onDismissRequest = vm::dismissMentionPicker, sheetState = sheet, containerColor = Hush.colors.background) {
        Column(Modifier.fillMaxWidth().navigationBarsPadding().padding(bottom = 8.dp)) {
            Text("选择提醒的人", style = HushType.bodyMedium, color = Hush.colors.textPrimary, modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp))
            TextField(
                value = q, onValueChange = { q = it }, singleLine = true,
                placeholder = { Text("搜索成员") },
                leadingIcon = { Icon(Icons.Filled.Search, null) },
                shape = RoundedCornerShape(24.dp),
                colors = TextFieldDefaults.colors(
                    focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent,
                    focusedContainerColor = Hush.colors.surface, unfocusedContainerColor = Hush.colors.surface,
                ),
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp).testTag("mention_search"),
            )
            LazyColumn(Modifier.heightIn(max = 420.dp)) {
                items(members, key = { it.first }) { (id, name) ->
                    Row(
                        Modifier.fillMaxWidth().clickable { vm.pickMention(id, name) }
                            .padding(horizontal = 16.dp, vertical = 10.dp).testTag("mention_$name"),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Avatar(id, name, profiles[id]?.avatarId, 40.dp)
                        Spacer(Modifier.width(12.dp))
                        Text(name, style = HushType.body, color = Hush.colors.textPrimary)
                    }
                    RowDivider(68)
                }
                if (members.isEmpty()) item {
                    Text("没有匹配的成员", color = Hush.colors.textSecondary, modifier = Modifier.padding(24.dp).background(Color.Transparent))
                }
            }
        }
    }
}
