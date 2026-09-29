package com.hush.app

import android.app.Application
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import com.hush.app.core.AppLock
import com.hush.app.core.HushCore
import com.hush.app.service.Notifier
import com.hush.app.ui.call.CallManager
import com.hush.app.util.MediaUtils
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch

class HushApp : Application() {
    override fun onCreate() {
        super.onCreate()
        HushCore.init(this)
        // 上次进程遗留的发送临时文件（发送中被杀进程等）
        HushCore.scope.launch(Dispatchers.IO) { MediaUtils.clearTemp(this@HushApp) }
        Notifier.createChannels(this)
        Notifier.observe(this)
        CallManager.init(this)
        AppLock.init(HushCore.prefs.screenLock.value)
        ProcessLifecycleOwner.get().lifecycle.addObserver(object : DefaultLifecycleObserver {
            override fun onStart(owner: LifecycleOwner) {
                HushCore.appInForeground = true
                HushCore.onForeground()
                AppLock.onForeground(HushCore.prefs.screenLock.value)
            }

            override fun onStop(owner: LifecycleOwner) {
                HushCore.appInForeground = false
                AppLock.onBackground()
                CallManager.onAppBackground()
            }
        })
    }
}
