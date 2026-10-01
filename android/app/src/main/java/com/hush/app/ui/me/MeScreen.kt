package com.hush.app.ui.me

import androidx.fragment.app.FragmentActivity
import com.hush.app.core.KeepAlive
import androidx.compose.material.icons.filled.NotificationsOff
import androidx.compose.material.icons.filled.BatteryChargingFull
import android.os.Build
import android.widget.Toast
import androidx.biometric.BiometricManager
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Logout
import androidx.compose.material.icons.filled.Block
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.DoneAll
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Keyboard
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.QrCode
import androidx.compose.material.icons.filled.Timer
import androidx.compose.material.icons.filled.Dns
import androidx.compose.material.icons.filled.Wifi
import androidx.compose.material.icons.filled.VisibilityOff
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
import com.hush.app.R
import com.hush.app.service.Notifier
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import androidx.compose.ui.unit.dp
import com.hush.app.core.HushCore
import com.hush.app.ui.chat.DisappearingDialog
import com.hush.app.ui.components.ConfirmDialog
import com.hush.app.ui.components.LargeHeader
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.components.SettingsGroup
import com.hush.app.ui.components.SettingsRow
import com.hush.app.ui.components.SwitchRow
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.TimeFormat
import kotlinx.coroutines.launch
import uniffi.hushcore.ConnectionState

/** 我 Tab：个人卡片 + 设置分组。 */
@Composable
fun MeScreen(onEditProfile: () -> Unit, onMyQr: () -> Unit, onBlocked: () -> Unit, onAbout: () -> Unit) {
    val context = LocalContext.current
    val acct by HushCore.account.collectAsState()
    val conn by HushCore.connection.collectAsState()
    val screenLock by HushCore.prefs.screenLock.collectAsState()
    val screenSecure by HushCore.prefs.screenSecurity.collectAsState()
    // 核心调用会读写数据库，放到 IO 线程
    var settings by remember { mutableStateOf<uniffi.hushcore.Settings?>(null) }
    LaunchedEffect(Unit) { settings = withContext(Dispatchers.IO) { runCatching { HushCore.client.settings() }.getOrNull() } }
    // Android 14+ 来电全屏提醒授权；从系统设置页返回时重新检查
    var fullScreenOk by remember { mutableStateOf(Notifier.canUseFullScreenIntent(context)) }
    var notifOk by remember { mutableStateOf(Notifier.notificationsEnabled(context)) }
    var batteryOk by remember { mutableStateOf(KeepAlive.ignoringBatteryOptimizations(context)) }
    LifecycleEventEffect(Lifecycle.Event.ON_RESUME) {
        fullScreenOk = Notifier.canUseFullScreenIntent(context)
        notifOk = Notifier.notificationsEnabled(context)
        batteryOk = KeepAlive.ignoringBatteryOptimizations(context)
    }
    var disDialog by remember { mutableStateOf(false) }
    var logoutConfirm by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    fun update(transform: (uniffi.hushcore.Settings) -> uniffi.hushcore.Settings) {
        val cur = settings ?: return
        val next = transform(cur)
        settings = next
        scope.launch {
            val ok = withContext(Dispatchers.IO) { runCatching { HushCore.client.setSettings(next) }.isSuccess }
            if (!ok) { settings = cur; Toast.makeText(context, "设置保存失败", Toast.LENGTH_SHORT).show() }
        }
    }

    Column(Modifier.fillMaxSize().background(Hush.colors.background).verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
        LargeHeader("我")
        // 个人卡片
        Row(
            Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp).clip(RoundedCornerShape(12.dp)).background(Hush.colors.surface)
                .clickable(onClick = onEditProfile).padding(16.dp).testTag("me_card"),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            acct?.let { UserAvatar(it.userId, 64.dp) }
            Spacer(Modifier.width(14.dp))
            Column(Modifier.weight(1f)) {
                Text(acct?.profile?.name ?: "", style = HushType.title, color = Hush.colors.textPrimary)
                Text(acct?.email ?: "", style = HushType.secondary, color = Hush.colors.textSecondary)
                val about = acct?.profile?.about
                if (!about.isNullOrBlank()) Text(about, style = HushType.secondary, color = Hush.colors.textSecondary, maxLines = 1)
            }
            IconButton(onClick = onMyQr, modifier = Modifier.testTag("me_qr")) { Icon(Icons.Filled.QrCode, "我的二维码", tint = Hush.colors.textSecondary) }
        }

        SettingsGroup(title = "隐私") {
            SwitchRow("已读回执", settings?.readReceipts == true, icon = Icons.Filled.DoneAll, subtitle = "关闭后对方看不到你是否已读") { v -> update { it.copy(readReceipts = v) } }
            RowDivider(52)
            SwitchRow("正在输入提示", settings?.typingIndicators == true, icon = Icons.Filled.Keyboard) { v -> update { it.copy(typingIndicators = v) } }
            RowDivider(52)
            SettingsRow("默认阅后即焚", icon = Icons.Filled.Timer, value = TimeFormat.disappearing(settings?.defaultDisappearingSecs?.toInt() ?: 0), onClick = { disDialog = true })
            RowDivider(52)
            SwitchRow("屏幕锁", screenLock, icon = Icons.Filled.Lock, subtitle = "打开应用时需验证生物识别或设备密码；开启后最近任务中隐藏内容，通知不提供直接回复") { v ->
                if (v) {
                    val can = BiometricManager.from(context).canAuthenticate(
                        BiometricManager.Authenticators.BIOMETRIC_WEAK or BiometricManager.Authenticators.DEVICE_CREDENTIAL,
                    )
                    if (can == BiometricManager.BIOMETRIC_SUCCESS) HushCore.prefs.setScreenLock(true)
                    else Toast.makeText(context, "请先在系统设置中设置锁屏密码或生物识别", Toast.LENGTH_LONG).show()
                } else {
                    // B12：关闭屏幕锁同样需要验证身份
                    val activity = context as? FragmentActivity
                    if (activity == null) HushCore.prefs.setScreenLock(false)
                    else authenticate(
                        activity, "关闭屏幕锁", "验证身份以关闭屏幕锁",
                        onSuccess = { HushCore.prefs.setScreenLock(false) },
                        onError = { Toast.makeText(context, "未通过验证，屏幕锁保持开启", Toast.LENGTH_SHORT).show() },
                        // 设备已没有任何锁屏凭据：屏幕锁本来也无法使用，直接关闭
                        onUnavailable = { action, reason ->
                            if (action == LockPolicy.Action.DISABLE) HushCore.prefs.setScreenLock(false)
                            else Toast.makeText(context, reason, Toast.LENGTH_SHORT).show()
                        },
                    )
                }
            }
            RowDivider(52)
            SwitchRow("屏幕安全", screenSecure, icon = Icons.Filled.VisibilityOff, subtitle = "禁止截屏与录屏，最近任务中隐藏内容") { HushCore.prefs.setScreenSecurity(it) }
            RowDivider(52)
            SettingsRow("黑名单", icon = Icons.Filled.Block, onClick = onBlocked)
        }
        SettingsGroup(title = "通知", footer = "为保护隐私，通知只显示会话名称与新消息数量，不显示消息内容。") {
            SwitchRow("消息通知", settings?.notifications == true, icon = Icons.Filled.Notifications) { v -> update { it.copy(notifications = v) } }
            if (!notifOk) {
                // B4：系统通知权限被拒 / 被关闭时，后台收不到任何消息与来电提醒
                RowDivider(52)
                SettingsRow(
                    "系统通知权限", icon = Icons.Filled.NotificationsOff, value = "未开启",
                    subtitle = "未开启时，应用在后台收不到新消息和来电提醒",
                    onClick = { runCatching { context.startActivity(Notifier.appNotificationSettings(context)) } },
                    modifier = Modifier.testTag("me_notif_permission"),
                )
            }
            RowDivider(52)
            // B3：电池优化白名单。未豁免时系统在息屏一段时间后会限制后台网络，长连接可能中断
            SettingsRow(
                "后台保持连接", icon = Icons.Filled.BatteryChargingFull, value = if (batteryOk) "已允许" else "受限",
                subtitle = if (batteryOk) "已允许后台运行，息屏后仍可及时收到消息与来电" else "系统电池优化可能在息屏后中断连接，点按允许后台运行",
                showChevron = !batteryOk,
                onClick = { if (!batteryOk) KeepAlive.openBatteryOptimization(context) },
                modifier = Modifier.testTag("me_battery"),
            )
            if (!fullScreenOk) {
                RowDivider(52)
                SettingsRow(
                    stringResource(R.string.full_screen_intent_title), icon = Icons.Filled.Call, value = "未授权",
                    subtitle = stringResource(R.string.full_screen_intent_desc),
                    onClick = { runCatching { context.startActivity(Notifier.fullScreenIntentSettings(context)) } },
                )
            }
        }
        SettingsGroup(title = "连接") {
            SettingsRow("服务器地址", icon = Icons.Filled.Dns, value = acct?.serverUrl ?: "", showChevron = false)
            RowDivider(52)
            SettingsRow(
                "连接状态", icon = Icons.Filled.Wifi, showChevron = false,
                value = when (conn) { ConnectionState.CONNECTED -> "已连接"; ConnectionState.CONNECTING -> "连接中…"; ConnectionState.DISCONNECTED -> "未连接" },
            )
        }
        SettingsGroup(title = "关于") {
            SettingsRow("关于 Hush", icon = Icons.Filled.Info, value = "v" + appVersion(context), onClick = onAbout)
        }
        SettingsGroup {
            SettingsRow("退出登录", icon = Icons.AutoMirrored.Filled.Logout, titleColor = Hush.colors.danger, showChevron = false, onClick = { logoutConfirm = true })
        }
    }
    if (disDialog) {
        DisappearingDialog(settings?.defaultDisappearingSecs?.toInt() ?: 0, onPick = { secs -> update { it.copy(defaultDisappearingSecs = secs.toUInt()) }; disDialog = false }, onDismiss = { disDialog = false })
    }
    if (logoutConfirm) {
        ConfirmDialog("退出登录", "退出后本机聊天记录将被清除", "退出", true, {
            // 退出后 HushCore.account 变为 null，HushRoot 据此回到登录页
            scope.launch { HushCore.logout() }
        }, { logoutConfirm = false })
    }
}

fun appVersion(context: android.content.Context): String = runCatching {
    val pi = if (Build.VERSION.SDK_INT >= 33) context.packageManager.getPackageInfo(context.packageName, android.content.pm.PackageManager.PackageInfoFlags.of(0))
    else @Suppress("DEPRECATION") context.packageManager.getPackageInfo(context.packageName, 0)
    pi.versionName ?: ""
}.getOrDefault("")
