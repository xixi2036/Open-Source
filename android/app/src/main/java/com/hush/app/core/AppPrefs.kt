package com.hush.app.core

import android.content.Context
import android.content.SharedPreferences
import androidx.core.content.edit
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow

/** 平台层本地偏好（核心 Settings 之外的开关）。 */
class AppPrefs(context: Context) {
    private val prefs: SharedPreferences = context.getSharedPreferences("hush_app", Context.MODE_PRIVATE)

    private val _screenLock = MutableStateFlow(prefs.getBoolean(K_LOCK, false))
    val screenLock: StateFlow<Boolean> = _screenLock

    private val _screenSecurity = MutableStateFlow(prefs.getBoolean(K_SECURE, false))
    val screenSecurity: StateFlow<Boolean> = _screenSecurity

    var lastServerUrl: String
        get() = prefs.getString(K_SERVER, DEFAULT_SERVER) ?: DEFAULT_SERVER
        set(v) = prefs.edit().putString(K_SERVER, v).apply()

    /** 朋友圈通知已读到的时间戳（毫秒）。 */
    var momentNotifSeenTs: Long
        get() = prefs.getLong(K_MOMENT_SEEN, 0)
        set(v) = prefs.edit().putLong(K_MOMENT_SEEN, v).apply()

    /**
     * v3 之前 App 用“隐藏列表”模拟删除会话（核心当时没有删除接口）。v3 起改用核心 delete_conversation（U1），
     * 这里只供启动时一次性迁移：取出旧列表并清掉该键。
     */
    fun takeLegacyHiddenConversations(): Set<String> {
        val set = prefs.getStringSet(K_HIDDEN_CONVS, null) ?: return emptySet()
        prefs.edit { remove(K_HIDDEN_CONVS) }
        return set
    }

    fun setScreenLock(on: Boolean) {
        prefs.edit().putBoolean(K_LOCK, on).apply(); _screenLock.value = on
    }

    fun setScreenSecurity(on: Boolean) {
        prefs.edit().putBoolean(K_SECURE, on).apply(); _screenSecurity.value = on
    }

    fun clear() {
        // 保留服务器地址：被动登出（登录过期、在其他设备登录）后回到登录页时仍预填原来的服务器
        val server = lastServerUrl
        prefs.edit { clear(); putString(K_SERVER, server) }
        _screenLock.value = false
        _screenSecurity.value = false
    }

    companion object {
        /** debug：模拟器访问宿主机的开发服务器；release：空，由用户填写（B1）。 */
        val DEFAULT_SERVER: String = com.hush.app.BuildConfig.DEFAULT_SERVER
        private const val K_LOCK = "screen_lock"
        private const val K_SECURE = "screen_security"
        private const val K_SERVER = "server_url"
        private const val K_MOMENT_SEEN = "moment_seen_ts"
        private const val K_HIDDEN_CONVS = "hidden_conversations"
    }
}
