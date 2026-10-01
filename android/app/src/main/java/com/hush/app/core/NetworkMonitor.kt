package com.hush.app.core

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/**
 * 系统网络状态（B2）：默认网络是否可用、是否按流量计费。
 * 网络恢复或切换（Wi-Fi ↔ 蜂窝）时回调 [onAvailable]，由 [HushCore] 立即重连并补拉，
 * 不必等核心的重连退避或看门狗。
 */
object NetworkMonitor {
    private val _online = MutableStateFlow(true)
    /** 系统当前有可用的默认网络。注册回调前按“有网络”处理，避免误拦操作。 */
    val online: StateFlow<Boolean> = _online

    private val _metered = MutableStateFlow(false)
    /** 当前默认网络按流量计费（蜂窝、计量 Wi-Fi）。 */
    val metered: StateFlow<Boolean> = _metered

    @Volatile private var registered = false

    fun start(context: Context, onAvailable: () -> Unit) {
        if (registered) return
        val cm = context.getSystemService(ConnectivityManager::class.java) ?: return
        runCatching {
            val active = cm.activeNetwork
            val caps = active?.let { cm.getNetworkCapabilities(it) }
            _online.value = caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) == true
            _metered.value = cm.isActiveNetworkMetered
            cm.registerDefaultNetworkCallback(object : ConnectivityManager.NetworkCallback() {
                override fun onAvailable(network: Network) {
                    _online.value = true
                    onAvailable()
                }

                override fun onLost(network: Network) {
                    // 默认网络丢失；若有其他网络接替，系统随后会再回调 onAvailable
                    _online.value = false
                }

                override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                    _metered.value = !caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
                }
            })
            registered = true
        }
    }
}
