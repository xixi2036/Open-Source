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
import kotlinx.coroutines.launch

/** 单 Activity：Compose 根 + 通知跳转 + 防截屏 + 来电亮屏。 */
class MainActivity : FragmentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)
        handleIntent(intent)
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.CREATED) {
                HushCore.prefs.screenSecurity.collect { secure ->
                    if (secure) window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
                    else window.clearFlags(WindowManager.LayoutParams.FLAG_SECURE)
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
        intent?.getStringExtra(Notifier.EXTRA_ACCEPT_CALL)?.let { callId ->
            intent.removeExtra(Notifier.EXTRA_ACCEPT_CALL)
            Notifier.cancelIncomingCall(this)
            NavRequests.acceptCall.value = callId
        }
        val conv = intent?.getStringExtra(Notifier.EXTRA_CONV) ?: return
        intent.removeExtra(Notifier.EXTRA_CONV)
        Notifier.cancelConversation(this, conv)
        NavRequests.conversation.value = conv
    }
}
