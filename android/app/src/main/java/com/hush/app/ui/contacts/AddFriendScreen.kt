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
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.QrCode
import androidx.compose.material.icons.filled.QrCodeScanner
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.components.SettingsGroup
import com.hush.app.ui.components.SettingsRow
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.Profile

/** 添加好友：邮箱精确搜索 / 扫一扫 / 我的二维码。 */
@Composable
fun AddFriendScreen(onBack: () -> Unit, onProfile: (String) -> Unit, onScan: () -> Unit, onMyQr: () -> Unit) {
    var email by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var result by remember { mutableStateOf<Profile?>(null) }
    var message by remember { mutableStateOf<String?>(null) }
    val scope = rememberCoroutineScope()
    fun search() {
        val q = email.trim().lowercase()
        if (q.isEmpty()) return
        busy = true; message = null; result = null
        scope.launch {
            try {
                val p = withContext(Dispatchers.IO) { net { HushCore.client.searchUserByEmail(q) } }
                if (p == null) message = "未找到该用户" else { result = p; ContactsStore.put(p) }
            } catch (e: Exception) {
                message = e.userMessage()
            }
            busy = false
        }
    }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("添加好友", onBack = onBack)
        TextField(
            value = email, onValueChange = { email = it }, singleLine = true,
            placeholder = { Text("输入对方邮箱（精确匹配）") },
            leadingIcon = { Icon(Icons.Filled.Search, null) },
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, imeAction = ImeAction.Search),
            keyboardActions = KeyboardActions(onSearch = { search() }),
            shape = RoundedCornerShape(24.dp),
            colors = TextFieldDefaults.colors(
                focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent,
                focusedContainerColor = Hush.colors.surface, unfocusedContainerColor = Hush.colors.surface,
            ),
            modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp).testTag("email_search"),
        )
        if (busy) CircularProgressIndicator(Modifier.padding(16.dp).align(Alignment.CenterHorizontally))
        message?.let { Text(it, color = Hush.colors.textSecondary, modifier = Modifier.padding(16.dp)) }
        result?.let { p ->
            val isFriend = ContactsStore.isFriend(p.id)
            Row(
                Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp).clip(RoundedCornerShape(12.dp)).background(Hush.colors.surface)
                    .clickable { onProfile(p.id) }.padding(12.dp).testTag("search_result"),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Avatar(p.id, p.name, p.avatarId, 52.dp)
                Spacer(Modifier.width(12.dp))
                Column(Modifier.weight(1f)) {
                    Text(p.name, style = HushType.bodyMedium, color = Hush.colors.textPrimary)
                    if (p.about.isNotBlank()) Text(p.about, style = HushType.secondary, color = Hush.colors.textSecondary, maxLines = 1)
                }
                Text(if (p.id == HushCore.myId) "自己" else if (isFriend) "已是好友" else "添加到通讯录", color = Hush.colors.accent, style = HushType.secondary)
            }
        }
        SettingsGroup {
            SettingsRow("扫一扫", icon = Icons.Filled.QrCodeScanner, subtitle = "扫描好友的二维码名片", onClick = onScan)
            RowDivider(52)
            SettingsRow("我的二维码", icon = Icons.Filled.QrCode, subtitle = "让好友扫一扫添加你", onClick = onMyQr)
        }
    }
}
