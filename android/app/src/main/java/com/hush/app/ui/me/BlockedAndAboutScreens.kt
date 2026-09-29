package com.hush.app.ui.me

import com.hush.app.util.net

import android.widget.Toast
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Block
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.hush.app.R
import com.hush.app.core.HushCore
import com.hush.app.ui.components.EmptyState
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.components.SettingsGroup
import com.hush.app.ui.components.SettingsRow
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.components.rememberName
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/** 黑名单。 */
@Composable
fun BlockedScreen(onBack: () -> Unit) {
    val context = LocalContext.current
    var ids by remember { mutableStateOf(emptyList<String>()) }
    var reload by remember { mutableIntStateOf(0) }
    val scope = rememberCoroutineScope()
    LaunchedEffect(reload) { ids = withContext(Dispatchers.IO) { runCatching { HushCore.client.blocked() }.getOrDefault(emptyList()) } }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("黑名单", onBack = onBack)
        if (ids.isEmpty()) { EmptyState(Icons.Outlined.Block, "黑名单为空"); return }
        LazyColumn {
            items(ids, key = { it }) { id ->
                Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
                    UserAvatar(id, 44.dp)
                    Spacer(Modifier.width(12.dp))
                    Text(rememberName(id, "用户"), style = HushType.body, color = Hush.colors.textPrimary, modifier = Modifier.weight(1f))
                    OutlinedButton(onClick = {
                        scope.launch {
                            runCatching { withContext(Dispatchers.IO) { net { HushCore.client.unblock(id) } } }
                                .onFailure { Toast.makeText(context, it.userMessage(), Toast.LENGTH_SHORT).show() }
                            reload++
                        }
                    }) { Text("移除") }
                }
                RowDivider(72)
            }
        }
    }
}

/** 关于。 */
@Composable
fun AboutScreen(onBack: () -> Unit) {
    val context = LocalContext.current
    Column(Modifier.fillMaxSize().background(Hush.colors.background).verticalScroll(rememberScrollState())) {
        HushTopBar("关于 Hush", onBack = onBack)
        Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally) {
            Image(painterResource(R.drawable.hush_logo), "Hush", modifier = Modifier.size(88.dp).clip(RoundedCornerShape(20.dp)))
            Spacer(Modifier.height(12.dp))
            Text("Hush 密语", style = HushType.title, color = Hush.colors.textPrimary)
            Text("版本 " + appVersion(context), style = HushType.secondary, color = Hush.colors.textSecondary)
        }
        SettingsGroup {
            SettingsRow("端到端加密", subtitle = "端到端加密由 vodozemac（Olm/Megolm）提供", showChevron = false)
            RowDivider()
            SettingsRow("本地存储", subtitle = "消息正文以 AES-256-GCM 加密存储，数据库密钥由 Android Keystore 保护", showChevron = false)
            RowDivider()
            SettingsRow(
                "开源许可", showChevron = false,
                subtitle = "本应用使用了 vodozemac、UniFFI、WebRTC、Jetpack Compose、Coil、ZXing、ML Kit 等开源组件，遵循其各自的开源许可证（Apache-2.0 / BSD / MIT 等）。",
            )
        }
        Text(
            "Hush 为自研客户端，所有图标来自 Material Icons。", style = HushType.secondary, color = Hush.colors.textSecondary,
            textAlign = TextAlign.Center, modifier = Modifier.fillMaxWidth().padding(24.dp),
        )
    }
}
