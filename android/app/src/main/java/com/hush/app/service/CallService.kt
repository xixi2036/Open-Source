package com.hush.app.service

import android.Manifest
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import com.hush.app.util.HushLog as Log
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import com.hush.app.ui.call.CallLogic
import com.hush.app.ui.call.CallManager

/**
 * 通话前台服务（microphone / camera 类型）。
 * Android 14+ 上麦克风、摄像头属于“使用中”权限：App 退到后台后只有已启动对应类型前台服务才能继续采集，
 * 否则通话中按 Home 键后对方只能听到静音。由 [CallManager] 在发起 / 接听时（App 在前台）启动，结束时停止。
 */
class CallService : Service() {
    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val video = intent?.getBooleanExtra(EXTRA_VIDEO, false) ?: false
        val types = types(this, video)
        var mask = 0
        if (Build.VERSION.SDK_INT >= 30) {
            if (types.microphone) mask = mask or ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
            if (types.camera) mask = mask or ServiceInfo.FOREGROUND_SERVICE_TYPE_CAMERA
        }
        try {
            // startForegroundService 之后必须先 startForeground，再判断是否需要立即停止
            ServiceCompat.startForeground(this, Notifier.ID_ONGOING_CALL, Notifier.ongoingCallNotification(this, video), mask)
        } catch (e: Exception) {
            Log.w(TAG, "startForeground", e)
            stopSelf(); return START_NOT_STICKY
        }
        inForeground = true
        // 启动请求发出后、服务真正跑起来之前通话已结束：此时 stop() 不会调用 stopService（见下），由这里自行停止
        if (!CallManager.isBusy) stopSelf()
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        inForeground = false
        super.onDestroy()
    }

    companion object {
        private const val TAG = "CallService"
        private const val EXTRA_VIDEO = "video"

        /** 已调用 startForeground。只在主线程读写（onStartCommand 与 CallManager.end 都在主线程）。 */
        private var inForeground = false

        private fun granted(context: Context, p: String) =
            ContextCompat.checkSelfPermission(context, p) == PackageManager.PERMISSION_GRANTED

        private fun types(context: Context, video: Boolean) = CallLogic.serviceTypes(
            video = video,
            micGranted = granted(context, Manifest.permission.RECORD_AUDIO),
            cameraGranted = granted(context, Manifest.permission.CAMERA),
        )

        /** 须在 App 位于前台时调用（发起 / 接听）；没有麦克风权限时无需也无法启动。 */
        fun start(context: Context, video: Boolean) {
            if (!types(context, video).any) return
            runCatching {
                ContextCompat.startForegroundService(context, Intent(context, CallService::class.java).putExtra(EXTRA_VIDEO, video))
            }.onFailure { Log.w(TAG, "start", it) }
        }

        /**
         * 通话结束时调用。startForegroundService 之后、startForeground 之前 stopService 会被系统判定为
         * “未调用 startForeground”而使进程崩溃，所以尚未进入前台时不停止，由 onStartCommand 复核后 stopSelf。
         */
        fun stop(context: Context) {
            if (!inForeground) return
            runCatching { context.stopService(Intent(context, CallService::class.java)) }
        }
    }
}
