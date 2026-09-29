package com.hush.app.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.ui.Alignment
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.hush.app.R
import com.hush.app.ui.theme.HushType
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.semantics
import androidx.core.content.ContextCompat
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.compose.ui.semantics.testTagsAsResourceId
import com.hush.app.core.AppLock
import com.hush.app.core.HushCore
import com.hush.app.service.ConnectionService
import com.hush.app.ui.auth.AuthScreen
import com.hush.app.ui.call.CallManager
import com.hush.app.ui.call.CallPhase
import com.hush.app.ui.call.CallScreen
import com.hush.app.ui.me.LockScreen
import com.hush.app.ui.nav.HushNavHost
import com.hush.app.ui.theme.Hush

/**
 * 根：未登录 → 登录流程；已登录 → 主导航，屏幕锁（若开启）作为覆盖层盖在主导航之上
 * （主导航始终留在组合中，解锁后导航栈、当前聊天与草稿不丢失）；通话页覆盖在最上层。
 */
@Composable
fun HushRoot() {
    val ready by HushCore.ready.collectAsState()
    // 本地数据库在后台线程打开：打开完成前显示启动页（品牌图标），不阻塞主线程
    if (!ready) {
        SplashScreen()
        return
    }
    ReadyRoot()
}

@OptIn(ExperimentalComposeUiApi::class)
@Composable
private fun ReadyRoot() {
    val context = LocalContext.current
    val account by HushCore.account.collectAsState()
    val locked by AppLock.locked.collectAsState()
    val call by CallManager.state.collectAsState()
    var kicked by remember { mutableStateOf<String?>(null) }
    // 每次登录成功后换一个登录流程 ViewModel：退出登录（含登录过期）回到登录页时从欢迎页重新开始，
    // 而不是停在上次登录流程的最后一步（例如新用户的资料设置页）
    var authGen by rememberSaveable { mutableIntStateOf(0) }
    LaunchedEffect(account != null) { if (account != null) authGen++ }
    LaunchedEffect(Unit) { HushCore.loggedOut.collect { kicked = it } }
    LaunchedEffect(account?.userId) {
        if (account != null) {
            HushCore.startSession()
            CallManager.prefetchTurn()
            ConnectionService.start(context)
        } else {
            ConnectionService.stop(context)
        }
    }

    // 运行时申请通知权限（Android 13+）：登录后只申请一次，已授权则不申请
    val notifPerm = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {}
    var notifAsked by rememberSaveable { mutableStateOf(false) }
    LaunchedEffect(account != null, locked) {
        if (account == null || locked || notifAsked || Build.VERSION.SDK_INT < 33) return@LaunchedEffect
        notifAsked = true
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            notifPerm.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    Box(Modifier.fillMaxSize().background(Hush.colors.background).semantics { testTagsAsResourceId = true }) {
        if (account == null) {
            AuthScreen(viewModel(key = "auth_$authGen"))
        } else {
            // 上锁时对无障碍服务隐藏下层内容
            Box(if (locked) Modifier.fillMaxSize().clearAndSetSemantics {} else Modifier.fillMaxSize()) { HushNavHost() }
            if (locked) LockScreen()
        }
        if (call.phase != CallPhase.IDLE && account != null) CallScreen(call)
    }
    kicked?.let { reason ->
        AlertDialog(
            onDismissRequest = { kicked = null },
            title = { Text("已退出登录") },
            text = { Text(com.hush.app.util.loggedOutMessage(reason)) },
            confirmButton = { TextButton(onClick = { kicked = null }) { Text("好") } },
        )
    }
}

/** 启动页：与系统启动窗口同底色，居中品牌图标；打开本地数据失败时显示原因与重试。 */
@Composable
private fun SplashScreen() {
    val error by HushCore.openError.collectAsState()
    Box(Modifier.fillMaxSize().background(Hush.colors.background).testTag("splash"), contentAlignment = Alignment.Center) {
        Column(horizontalAlignment = Alignment.CenterHorizontally) {
            Image(
                painterResource(R.drawable.hush_logo), "Hush",
                modifier = Modifier.size(120.dp).clip(RoundedCornerShape(28.dp)),
            )
            val e = error
            if (e != null) {
                Spacer(Modifier.height(24.dp))
                Text("无法打开本地数据", style = HushType.title, color = Hush.colors.textPrimary)
                Spacer(Modifier.height(8.dp))
                Text(
                    e.message ?: e.javaClass.simpleName, style = HushType.secondary, color = Hush.colors.textSecondary,
                    textAlign = TextAlign.Center, modifier = Modifier.padding(horizontal = 32.dp),
                )
                Spacer(Modifier.height(16.dp))
                TextButton(onClick = { HushCore.openInBackground() }) { Text("重试") }
            }
        }
    }
}
