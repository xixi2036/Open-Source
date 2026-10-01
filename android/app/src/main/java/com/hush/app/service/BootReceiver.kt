package com.hush.app.service

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import com.hush.app.core.HushCore
import com.hush.app.util.HushLog
import kotlinx.coroutines.launch

/**
 * 开机 / 应用升级后自启（B3）：已登录时拉起后台连接前台服务，否则重启手机后要等用户手动打开 App 才能收到消息与来电。
 *
 * - BOOT_COMPLETED 在用户首次解锁之后才发出，此时 Keystore 与应用私有目录已可用。
 * - BOOT_COMPLETED / MY_PACKAGE_REPLACED 属于允许从后台启动前台服务的豁免场景；
 *   Android 15 起禁止从开机广播启动的前台服务类型不包括 remoteMessaging。
 * - 是否登录要等数据库在后台线程打开后才知道：未登录时不启动服务，避免无意义的常驻通知。
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val action = intent.action
        if (action != Intent.ACTION_BOOT_COMPLETED && action != Intent.ACTION_MY_PACKAGE_REPLACED) return
        val app = context.applicationContext
        val pending = goAsync()
        HushCore.scope.launch {
            try {
                // Application.onCreate 已经开始打开数据库；广播接收器最多等约 10 秒
                val ready = kotlinx.coroutines.withTimeoutOrNull(8_000) { HushCore.awaitReady() } != null
                if (ready && HushCore.isLoggedIn) {
                    HushLog.i("BootReceiver", "start connection service after $action")
                    ConnectionService.start(app)
                }
            } finally {
                pending.finish()
            }
        }
    }
}
