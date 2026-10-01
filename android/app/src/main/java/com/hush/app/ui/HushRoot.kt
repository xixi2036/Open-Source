package com.hush.app.ui

import androidx.compose.runtime.CompositionLocalProvider
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.ViewModelStore
import androidx.lifecycle.ViewModelStoreOwner
import androidx.lifecycle.viewmodel.compose.LocalViewModelStoreOwner
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
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.ui.focus.focusProperties
import androidx.compose.foundation.focusGroup
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LifecycleEventEffect
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
import com.hush.app.ui.call.CallMiniBar
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
    val callMinimized by CallManager.minimized.collectAsState()
    var kicked by remember { mutableStateOf<String?>(null) }
    // 每次登录成功后换一个登录流程 ViewModel：退出登录（含登录过期）回到登录页时从欢迎页重新开始，
    // 而不是停在上次登录流程的最后一步（例如新用户的资料设置页）
    var authGen by rememberSaveable { mutableIntStateOf(0) }
    val rootOwner = LocalViewModelStoreOwner.current
    LaunchedEffect(account != null) {
        if (account != null) authGen++
        // 退出登录（含被动登出）：清理上一次登录的全部页面 ViewModel（此时主导航已离开组合）
        else rootOwner?.let { o -> runCatching { ViewModelProvider(o)[SESSION_KEY, SessionViewModel::class.java].clearAll() } }
    }
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
            // A1：上锁时下层的输入框不得继续持有焦点，否则键盘输入仍会写进聊天输入框与草稿。
            // 上锁（以及上锁期间每次回到前台）时强制清除焦点并收起键盘；下层整体不可获得焦点，
            // 焦点由锁屏页自己持有（见 LockScreen）。
            val focusManager = LocalFocusManager.current
            val keyboard = LocalSoftwareKeyboardController.current
            LaunchedEffect(locked) {
                if (locked) { focusManager.clearFocus(force = true); keyboard?.hide() }
            }
            LifecycleEventEffect(Lifecycle.Event.ON_RESUME) {
                if (AppLock.locked.value) { focusManager.clearFocus(force = true); keyboard?.hide() }
            }
            // 上锁时对无障碍服务隐藏下层内容
            Box(
                if (locked) Modifier.fillMaxSize().clearAndSetSemantics {}.focusProperties { canFocus = false }.focusGroup()
                else Modifier.fillMaxSize(),
            ) {
                // B15：主导航里的 ViewModel（聊天页、会话列表、朋友圈……）归属于本次登录的会话级 store，
                // 退出登录时整体清理，不再留在 Activity 的 store 里继续订阅事件
                val session: SessionViewModel = viewModel(key = SESSION_KEY)
                CompositionLocalProvider(LocalViewModelStoreOwner provides session) { HushNavHost() }
            }
            if (locked) LockScreen()
        }
        if (call.phase != CallPhase.IDLE && account != null) {
            // 通话页可最小化为悬浮条（返回键 / 左上角按钮），最小化期间其他页面照常使用
            if (callMinimized) CallMiniBar(call, Modifier.align(Alignment.TopEnd)) else CallScreen(call)
        }
    }
    com.hush.app.util.ToastCollector(CallManager.notice)
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

private const val SESSION_KEY = "session"

/**
 * 登录会话级的 ViewModelStore（B15）：自身存放在 Activity 的 store 里（配置变更时保留），
 * 作为主导航的 ViewModelStoreOwner；退出登录时 [clearAll]，其中的 ViewModel 全部 onCleared。
 */
class SessionViewModel : ViewModel(), ViewModelStoreOwner {
    override val viewModelStore = ViewModelStore()

    fun clearAll() = viewModelStore.clear()

    override fun onCleared() = viewModelStore.clear()
}
