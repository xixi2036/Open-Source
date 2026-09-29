package com.hush.app.service

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import androidx.core.net.toUri
import com.hush.app.MainActivity
import com.hush.app.R
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.MessageBody
import java.util.concurrent.ConcurrentHashMap

/** 本地通知：消息渠道、来电渠道、通话中渠道、常驻连接渠道。 */
object Notifier {
    const val CH_MESSAGES = "messages"
    const val CH_CALLS = "calls"
    const val CH_ONGOING_CALL = "ongoing_call"
    const val CH_CONNECTION = "connection"
    const val EXTRA_CONV = "conv_id"
    const val EXTRA_ACCEPT_CALL = "accept_call"
    const val EXTRA_CALL_ID = "call_id"
    private const val ID_CALL = 2001
    const val ID_ONGOING_CALL = 2002
    const val ID_CONNECTION = 1001

    /** 同一会话在该时间窗内的多条消息（例如重连后补拉的积压）只发一次通知。 */
    private const val MESSAGE_DEBOUNCE_MS = 500L
    private val pendingMessages = ConcurrentHashMap<String, Job>()

    fun createChannels(context: Context) {
        val nm = context.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(CH_MESSAGES, context.getString(R.string.ch_messages), NotificationManager.IMPORTANCE_HIGH)
                .apply { description = context.getString(R.string.ch_messages_desc) },
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_CALLS, context.getString(R.string.ch_calls), NotificationManager.IMPORTANCE_HIGH)
                .apply { description = context.getString(R.string.ch_calls_desc) },
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_ONGOING_CALL, context.getString(R.string.ch_ongoing_call), NotificationManager.IMPORTANCE_LOW)
                .apply { description = context.getString(R.string.ch_ongoing_call_desc) },
        )
        nm.createNotificationChannel(
            NotificationChannel(CH_CONNECTION, context.getString(R.string.ch_connection), NotificationManager.IMPORTANCE_MIN)
                .apply { description = context.getString(R.string.ch_connection_desc) },
        )
    }

    fun openConversationIntent(context: Context, convId: String?): PendingIntent {
        val i = Intent(context, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP
            if (convId != null) putExtra(EXTRA_CONV, convId)
        }
        return PendingIntent.getActivity(context, convId?.hashCode() ?: 0, i, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
    }

    /** 监听核心事件，在会话不在前台时发消息通知（按会话 [MESSAGE_DEBOUNCE_MS] 合并）。 */
    fun observe(context: Context) {
        val app = context.applicationContext
        HushCore.scope.launch {
            HushCore.events.collect { ev ->
                if (ev is CoreEvent.MessageAdded && !ev.message.isOutgoing) {
                    val body = ev.message.body
                    if (body !is MessageBody.Text && body !is MessageBody.Attachments && !(body is MessageBody.CallLog && body.missed)) return@collect
                    if (HushCore.appInForeground && HushCore.activeConversationId == ev.convId) return@collect
                    // P-F：积压消息（重连后补拉、启动时处理的残留）不弹横幅、不响铃，只静默更新通知栏
                    scheduleMessage(app, ev.convId, alert = !ev.backlog, mention = ev.mentionsMe)
                }
            }
        }
    }

    /** 合并窗口内是否有需要提醒（非积压）的消息。 */
    private val pendingAlert = ConcurrentHashMap<String, Boolean>()
    /** 合并窗口内是否有提及我的消息（U4：免打扰会话里被 @ 仍通知）。 */
    private val pendingMention = ConcurrentHashMap<String, Boolean>()

    private fun scheduleMessage(context: Context, convId: String, alert: Boolean, mention: Boolean) {
        pendingMessages.remove(convId)?.cancel()
        if (alert) pendingAlert[convId] = true
        if (mention) pendingMention[convId] = true
        pendingMessages[convId] = HushCore.scope.launch(Dispatchers.IO) {
            delay(MESSAGE_DEBOUNCE_MS)
            pendingMessages.remove(convId)
            val shouldAlert = pendingAlert.remove(convId) == true
            val mentioned = pendingMention.remove(convId) == true
            // 合并窗口内用户可能已打开该会话
            if (HushCore.appInForeground && HushCore.activeConversationId == convId) return@launch
            notifyMessage(context, convId, shouldAlert, mentioned)
        }
    }

    private fun notifyMessage(context: Context, convId: String, alert: Boolean, mentioned: Boolean) {
        val settings = runCatching { HushCore.client.settings() }.getOrNull() ?: return
        if (!settings.notifications) return
        val conv = runCatching { HushCore.client.conversation(convId) }.getOrNull() ?: return
        if (conv.unreadCount == 0u) return
        // 免打扰会话只在这次有人 @我 时通知（核心契约：被提及的消息即使免打扰也通知）
        if (conv.muted && !mentioned) return
        val text = if (conv.unreadMentions > 0u) context.getString(R.string.notif_mentioned, conv.title) else context.getString(R.string.notif_new_message, conv.title)
        val n = NotificationCompat.Builder(context, CH_MESSAGES)
            .setSmallIcon(R.drawable.ic_stat_hush)
            .setColor(ContextCompat.getColor(context, R.color.accent))
            .setContentTitle(context.getString(R.string.app_name))
            .setContentText(text)
            .setNumber(conv.unreadCount.toInt())
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            // 未读期间的后续更新只刷新内容，不再次响铃/振动
            .setOnlyAlertOnce(true)
            // 积压消息：只出现在通知栏，不弹横幅、不响铃
            .setSilent(!alert)
            .setAutoCancel(true)
            .setContentIntent(openConversationIntent(context, convId))
            .build()
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return
        runCatching { NotificationManagerCompat.from(context).notify(convId.hashCode(), n) }
    }

    /** 会话已被查看（进入聊天页 / 已读）：撤掉该会话的通知及尚未发出的合并通知。 */
    fun cancelConversation(context: Context, convId: String) {
        pendingMessages.remove(convId)?.cancel()
        pendingAlert.remove(convId)
        pendingMention.remove(convId)
        NotificationManagerCompat.from(context).cancel(convId.hashCode())
    }

    /**
     * Android 14+ 上全屏 Intent 需要用户授权（USE_FULL_SCREEN_INTENT 可能被系统撤销）。
     * 未授权时来电只显示为普通高优先级通知，锁屏状态下不会拉起来电界面。
     */
    fun canUseFullScreenIntent(context: Context): Boolean =
        Build.VERSION.SDK_INT < 34 || context.getSystemService(NotificationManager::class.java).canUseFullScreenIntent()

    /** 打开“全屏通知”授权页（Android 14+），不支持时打开应用通知设置。 */
    fun fullScreenIntentSettings(context: Context): Intent =
        if (Build.VERSION.SDK_INT >= 34) {
            Intent(Settings.ACTION_MANAGE_APP_USE_FULL_SCREEN_INTENT, "package:${context.packageName}".toUri())
        } else {
            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, context.packageName)
        }

    /** 来电通知（全屏 Intent）；App 在前台时由界面直接弹出，不发通知。 */
    fun showIncomingCall(context: Context, peerId: String, video: Boolean, callId: String) {
        if (HushCore.appInForeground) return
        val name = ContactsStore.nameOf(peerId)
        val i = Intent(context, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        val pi = PendingIntent.getActivity(context, ID_CALL, i, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        // “接听”打开来电页并按页面上的流程接听（需要时申请麦克风/摄像头权限）；“拒绝”在后台直接拒接
        val acceptIntent = Intent(context, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP
            putExtra(EXTRA_ACCEPT_CALL, callId)
        }
        val acceptPi = PendingIntent.getActivity(context, ID_CALL + 1, acceptIntent, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        val rejectIntent = Intent(context, CallActionReceiver::class.java).setAction(CallActionReceiver.ACTION_REJECT).putExtra(EXTRA_CALL_ID, callId)
        val rejectPi = PendingIntent.getBroadcast(context, ID_CALL + 2, rejectIntent, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        val b = NotificationCompat.Builder(context, CH_CALLS)
            .setSmallIcon(R.drawable.ic_stat_hush)
            .setContentTitle(name)
            .setContentText(context.getString(if (video) R.string.notif_incoming_video else R.string.notif_incoming_voice))
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setOngoing(true)
            .setAutoCancel(true)
            .setContentIntent(pi)
            .addAction(0, context.getString(R.string.notif_call_reject), rejectPi)
            .addAction(0, context.getString(R.string.notif_call_accept), acceptPi)
            .setTimeoutAfter(45_000)
        // 未获授权时 setFullScreenIntent 会被系统降级为普通通知；显式区分便于日志排查
        if (canUseFullScreenIntent(context)) b.setFullScreenIntent(pi, true)
        else com.hush.app.util.HushLog.w("Notifier", "full-screen intent not permitted; incoming call shown as heads-up only")
        val n = b.build()
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return
        runCatching { NotificationManagerCompat.from(context).notify(ID_CALL, n) }
    }

    fun cancelIncomingCall(context: Context) {
        NotificationManagerCompat.from(context).cancel(ID_CALL)
    }

    /** 通话进行中的常驻通知（CallService 前台通知），点击回到通话界面。 */
    fun ongoingCallNotification(context: Context, video: Boolean): Notification =
        NotificationCompat.Builder(context, CH_ONGOING_CALL)
            .setSmallIcon(R.drawable.ic_stat_hush)
            .setColor(ContextCompat.getColor(context, R.color.accent))
            .setContentTitle(context.getString(if (video) R.string.notif_ongoing_video else R.string.notif_ongoing_voice))
            .setContentText(context.getString(R.string.notif_ongoing_desc))
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setOngoing(true)
            .setContentIntent(openConversationIntent(context, null))
            .build()

    fun connectionNotification(context: Context): Notification =
        NotificationCompat.Builder(context, CH_CONNECTION)
            .setSmallIcon(R.drawable.ic_stat_hush)
            .setContentTitle(context.getString(R.string.notif_connection_title))
            .setContentText(context.getString(R.string.notif_connection_desc))
            .setPriority(NotificationCompat.PRIORITY_MIN)
            .setOngoing(true)
            .setContentIntent(openConversationIntent(context, null))
            .build()
}
