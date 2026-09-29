package com.hush.app.ui.chat

import com.hush.app.util.net

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.QrCodeScanner
import androidx.compose.material.icons.filled.VerifiedUser
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.hush.app.core.HushCore
import com.hush.app.ui.auth.PrimaryButton
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.QrImage
import com.hush.app.ui.components.rememberName
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.userMessage
import androidx.compose.runtime.rememberCoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.SafetyNumber

/** 安全码：60 位数字（12 组 × 5 位，3 列）、二维码、扫码核验、标记已核验。 */
@Composable
fun SafetyNumberScreen(userId: String, onBack: () -> Unit, onScan: (String) -> Unit) {
    val name = rememberName(userId, "对方")
    var sn by remember { mutableStateOf<SafetyNumber?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableIntStateOf(0) }
    val scope = rememberCoroutineScope()
    LaunchedEffect(reload) {
        runCatching { withContext(Dispatchers.IO) { net { HushCore.client.safetyNumber(userId) } } }
            .onSuccess { sn = it }.onFailure { error = it.userMessage() }
    }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("安全码", onBack = onBack)
        val s = sn
        if (s == null) {
            Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                if (error != null) Text(error!!, color = Hush.colors.danger) else CircularProgressIndicator()
            }
            return
        }
        Column(
            Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 24.dp, vertical = 8.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            if (s.verified) {
                Row(
                    Modifier.clip(RoundedCornerShape(16.dp)).background(Hush.colors.success.copy(alpha = 0.14f)).padding(horizontal = 12.dp, vertical = 6.dp).testTag("verified_badge"),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(Icons.Filled.VerifiedUser, null, tint = Hush.colors.success, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(6.dp))
                    Text("已核验", color = Hush.colors.success, style = HushType.bodyMedium)
                }
                Spacer(Modifier.height(12.dp))
            }
            Box(Modifier.clip(RoundedCornerShape(12.dp)).background(Color.White).padding(12.dp)) { QrImage(s.qrPayload, 200.dp) }
            Spacer(Modifier.height(20.dp))
            val groups = s.digits.chunked(5)
            Column(verticalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.testTag("safety_digits")) {
                groups.chunked(3).forEach { row ->
                    Row(horizontalArrangement = Arrangement.spacedBy(20.dp)) {
                        row.forEach { g -> Text(g, fontFamily = FontFamily.Monospace, fontSize = 20.sp, color = Hush.colors.textPrimary) }
                    }
                }
            }
            Spacer(Modifier.height(16.dp))
            Text(
                "如果你和 $name 两台设备上看到的安全码一致，说明你们之间的端到端加密未被中间人篡改。可以当面扫描对方的二维码进行核验。",
                style = HushType.secondary, color = Hush.colors.textSecondary, textAlign = TextAlign.Center,
            )
            Spacer(Modifier.height(24.dp))
            OutlinedButton(onClick = { onScan(userId) }, modifier = Modifier.fillMaxWidth().height(48.dp).testTag("scan_verify"), shape = RoundedCornerShape(24.dp)) {
                Icon(Icons.Filled.QrCodeScanner, null)
                Spacer(Modifier.width(8.dp))
                Text("扫码核验")
            }
            Spacer(Modifier.height(12.dp))
            PrimaryButton(
                if (s.verified) "取消核验" else "标记为已核验",
                onClick = {
                    scope.launch {
                        withContext(Dispatchers.IO) { runCatching { HushCore.client.setVerified(userId, !s.verified) } }
                        reload++
                    }
                },
                modifier = Modifier.testTag("toggle_verified"),
            )
        }
    }
}
