package com.hush.app.service

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Handler
import android.os.Looper
import android.widget.Toast
import androidx.core.app.RemoteInput
import com.hush.app.R
import com.hush.app.core.HushCore
import com.hush.app.util.HushLog
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** 消息通知上的操作：标为已读、直接回复（不打开界面）。 */
class NotificationActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val convId = intent.getStringExtra(Notifier.EXTRA_CONV) ?: return
        val action = intent.action ?: return
        val reply = RemoteInput.getResultsFromIntent(intent)?.getCharSequence(KEY_REPLY)?.toString()?.trim()
        val app = context.applicationContext
        val pending = goAsync()
        HushCore.scope.launch(Dispatchers.IO) {
            try {
                if (withTimeoutOrNull(8_000) { HushCore.awaitReady() } == null || !HushCore.isLoggedIn) return@launch
                when (action) {
                    ACTION_MARK_READ -> runCatching { HushCore.client.markRead(convId) }
                    ACTION_REPLY -> {
                        // 发通知之后用户才开启屏幕锁：旧通知上的回复入口不再生效
                        if (reply.isNullOrEmpty() || HushCore.prefs.screenLock.value) return@launch
                        val sent = withTimeoutOrNull(8_000) { runCatching { HushCore.client.sendText(convId, reply, null) } }
                        if (sent?.getOrNull()?.let { it.status != uniffi.hushcore.MessageStatus.FAILED } == true) runCatching { HushCore.client.markRead(convId) }
                        else {
                            HushLog.w("NotifAction", "reply failed", sent?.exceptionOrNull())
                            Handler(Looper.getMainLooper()).post { Toast.makeText(app, R.string.notif_reply_failed, Toast.LENGTH_LONG).show() }
                        }
                    }
                }
            } finally {
                // 撤掉通知（直接回复后不撤的话，通知会一直停在“正在发送”的转圈状态）
                Notifier.cancelConversation(app, convId)
                pending.finish()
            }
        }
    }

    companion object {
        const val ACTION_MARK_READ = "com.hush.app.action.MARK_READ"
        const val ACTION_REPLY = "com.hush.app.action.REPLY"
        const val KEY_REPLY = "reply_text"
    }
}
