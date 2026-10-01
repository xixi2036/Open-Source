package com.hush.app.core

import android.annotation.SuppressLint
import android.content.Context
import android.content.Intent
import androidx.core.net.toUri
import android.os.PowerManager
import android.provider.Settings

/**
 * 后台保活相关（B3）：限时 WakeLock 与电池优化白名单。
 *
 * WakeLock 只在“必须跑完的一小段工作”期间持有并且一律带超时：网络恢复后的补拉、来电响铃与接通建立。
 * 不为长连接常驻持有 WakeLock（耗电，且 Doze 下网络本来就被挂起，持锁也无济于事）；
 * 长连接在息屏后的存活主要靠前台服务 + 电池优化白名单，需真机验证。
 */
object KeepAlive {
    private const val TAG_PREFIX = "hush:"

    fun newWakeLock(context: Context, name: String): PowerManager.WakeLock? = runCatching {
        context.getSystemService(PowerManager::class.java)
            ?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, TAG_PREFIX + name)
            ?.apply { setReferenceCounted(false) }
    }.getOrNull()

    /** 持有限时 WakeLock 执行 [block]；最长 [timeoutMs]，结束即释放。 */
    inline fun <T> withWakeLock(context: Context, name: String, timeoutMs: Long, block: () -> T): T {
        val wl = newWakeLock(context, name)
        runCatching { wl?.acquire(timeoutMs) }
        try {
            return block()
        } finally {
            runCatching { if (wl?.isHeld == true) wl.release() }
        }
    }

    /** 是否已加入电池优化白名单（不受 Doze / 应用待机的网络限制）。 */
    fun ignoringBatteryOptimizations(context: Context): Boolean = runCatching {
        context.getSystemService(PowerManager::class.java)?.isIgnoringBatteryOptimizations(context.packageName) == true
    }.getOrDefault(false)

    /**
     * 请求加入电池优化白名单的系统对话框；部分 ROM 不支持直接请求时退回到白名单列表页。
     * 即时通讯属于系统允许申请该豁免的用途（需要保持长连接接收消息与来电）。
     */
    @SuppressLint("BatteryLife")
    fun batteryOptimizationIntents(context: Context): List<Intent> = listOf(
        Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, "package:${context.packageName}".toUri()),
        Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS),
        Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, "package:${context.packageName}".toUri()),
    )

    /** 依次尝试打开，返回是否成功打开了某个页面。 */
    fun openBatteryOptimization(context: Context): Boolean {
        for (i in batteryOptimizationIntents(context)) {
            val ok = runCatching { context.startActivity(i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)) }.isSuccess
            if (ok) return true
        }
        return false
    }
}
