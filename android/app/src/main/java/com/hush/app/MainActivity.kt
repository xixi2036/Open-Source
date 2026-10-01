package com.hush.app

import android.content.Intent
import android.os.Build
import android.os.Bundle
import android.view.WindowManager
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import com.hush.app.core.HushCore
import com.hush.app.service.Notifier
import com.hush.app.ui.HushRoot
import com.hush.app.ui.call.CallLogic
import com.hush.app.ui.call.CallManager
import com.hush.app.ui.nav.NavRequests
import com.hush.app.ui.theme.HushTheme
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch
import com.hush.app.util.WindowPrivacy

/** 单 Activity：Compose 根 + 通知跳转 + 防截屏 + 来电亮屏。 */
class MainActivity : FragmentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        // C9：进程被回收后从最近任务恢复时，系统会把当初启动 Activity 的那个 Intent（带通知的会话参数）原样再给一次。
        // 只在全新启动时处理；恢复（savedInstanceState != null）或“从历史启动”时忽略，不再跳进那个会话。
        if (savedInstanceState == null) handleIntent(intent)
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.CREATED) {
                combine(HushCore.prefs.screenSecurity, HushCore.prefs.screenLock) { secure, lock -> secure to lock }.collect { (secure, lock) ->
                    val (flagSecure, hideRecents) = WindowPrivacy.decide(secure, lock, Build.VERSION.SDK_INT)
                    if (flagSecure) window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
                    else window.clearFlags(WindowManager.LayoutParams.FLAG_SECURE)
                    // B12：开启屏幕锁时最近任务里不得露出聊天内容（Android 13+ 只隐藏缩略图，不影响截屏）
                    if (Build.VERSION.SDK_INT >= 33) setRecentsScreenshotEnabled(!hideRecents)
                }
            }
        }
        lifecycleScope.launch {
            CallManager.state.collect { s ->
                // 整个通话期间都显示在锁屏之上（从锁屏接听后不能被锁屏盖住）；只有来电响铃时点亮屏幕
                if (Build.VERSION.SDK_INT >= 27) {
                    setShowWhenLocked(CallLogic.showWhenLocked(s.phase))
                    setTurnScreenOn(CallLogic.turnScreenOn(s.phase))
                }
            }
        }
        setContent { HushTheme { HushRoot() } }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleIntent(intent)
    }

    private fun handleIntent(intent: Intent?) {
        if (intent == null || intent.flags and Intent.FLAG_ACTIVITY_LAUNCHED_FROM_HISTORY != 0) return
        if (intent.getBooleanExtra(Notifier.EXTRA_SHOW_CALL, false)) {
            intent.removeExtra(Notifier.EXTRA_SHOW_CALL)
            CallManager.maximize()
        }
        intent.getStringExtra(Notifier.EXTRA_ACCEPT_CALL)?.let { callId ->
            intent.removeExtra(Notifier.EXTRA_ACCEPT_CALL)
            Notifier.cancelIncomingCall(this)
            NavRequests.acceptCall.value = callId
        }
        val conv = intent.getStringExtra(Notifier.EXTRA_CONV) ?: return
        intent.removeExtra(Notifier.EXTRA_CONV)
        Notifier.cancelConversation(this, conv)
        NavRequests.conversation.value = conv
    }
}
