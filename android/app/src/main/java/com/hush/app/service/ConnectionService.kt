package com.hush.app.service

import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import com.hush.app.util.HushLog as Log
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import com.hush.app.core.HushCore
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch

/**
 * 前台服务（remoteMessaging）：进程常驻，使核心库的 WebSocket 在后台保持连接。
 * 服务本身不创建 HushClient，与 Activity 共用 [HushCore] 单例。
 */
class ConnectionService : Service() {
    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val n = Notifier.connectionNotification(this)
        val type = if (Build.VERSION.SDK_INT >= 34) ServiceInfo.FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING else 0
        try {
            ServiceCompat.startForeground(this, Notifier.ID_CONNECTION, n, type)
        } catch (e: Exception) {
            Log.w("ConnectionService", "startForeground", e)
            stopSelf(); return START_NOT_STICKY
        }
        // 进程被回收后系统重启本服务（START_STICKY）时，数据库可能尚未打开：等打开完成再判断登录态
        job?.cancel()
        job = HushCore.scope.launch(Dispatchers.Main) {
            HushCore.awaitReady()
            if (!HushCore.isLoggedIn) stopSelf() else HushCore.startSession()
        }
        return START_STICKY
    }

    private var job: Job? = null

    override fun onDestroy() {
        job?.cancel()
        super.onDestroy()
    }

    companion object {
        fun start(context: Context) {
            runCatching { ContextCompat.startForegroundService(context, Intent(context, ConnectionService::class.java)) }
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, ConnectionService::class.java))
        }
    }
}
