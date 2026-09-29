package com.hush.app.service

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Handler
import android.os.Looper
import com.hush.app.ui.call.CallManager

/** 来电通知上的“拒绝”按钮（不打开界面）。CallManager 的状态只在主线程修改。 */
class CallActionReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != ACTION_REJECT) return
        val callId = intent.getStringExtra(Notifier.EXTRA_CALL_ID) ?: return
        Handler(Looper.getMainLooper()).post { CallManager.rejectFromNotification(callId) }
    }

    companion object {
        const val ACTION_REJECT = "com.hush.app.action.REJECT_CALL"
    }
}
