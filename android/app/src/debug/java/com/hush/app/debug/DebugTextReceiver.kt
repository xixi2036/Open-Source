package com.hush.app.debug

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Base64
import com.hush.app.util.DebugInput

/** 自动化测试用：adb 无法直接输入中文，通过广播把文字填入当前输入框。 */
class DebugTextReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val text = intent.getStringExtra("b64")?.let { String(Base64.decode(it, Base64.DEFAULT), Charsets.UTF_8) }
            ?: intent.getStringExtra("text")
            ?: return
        DebugInput.post(text)
    }
}
