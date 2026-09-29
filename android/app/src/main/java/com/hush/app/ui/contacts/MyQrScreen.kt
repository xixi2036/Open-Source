package com.hush.app.ui.contacts

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.hush.app.core.HushCore
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.QrImage
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** 我的二维码：my_qr_payload + 头像昵称。 */
@Composable
fun MyQrScreen(onBack: () -> Unit) {
    val acct by HushCore.account.collectAsState()
    var payload by remember { mutableStateOf("") }
    LaunchedEffect(acct?.userId) {
        payload = withContext(Dispatchers.IO) { runCatching { HushCore.client.myQrPayload() }.getOrDefault("") }
    }
    Column(Modifier.fillMaxSize().background(Hush.colors.surface)) {
        HushTopBar("我的二维码", onBack = onBack)
        Column(
            Modifier.padding(24.dp).fillMaxWidth().clip(RoundedCornerShape(16.dp)).background(Hush.colors.background).padding(24.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
                acct?.let { UserAvatar(it.userId, 56.dp) }
                Spacer(Modifier.width(12.dp))
                Column {
                    Text(acct?.profile?.name ?: "", style = HushType.title, color = Hush.colors.textPrimary)
                    Text(acct?.email ?: "", style = HushType.secondary, color = Hush.colors.textSecondary)
                }
            }
            Spacer(Modifier.height(24.dp))
            Box(Modifier.clip(RoundedCornerShape(12.dp)).background(Color.White).padding(12.dp).testTag("my_qr")) { QrImage(payload, 240.dp) }
            Spacer(Modifier.height(16.dp))
            Text("扫一扫上面的二维码，加我为好友", style = HushType.secondary, color = Hush.colors.textSecondary)
        }
    }
}
