package com.hush.app.ui.nav

import kotlinx.coroutines.flow.MutableStateFlow

/** 外部（通知点击）请求打开的页面。 */
object NavRequests {
    val conversation = MutableStateFlow<String?>(null)

    /** 来电通知上点了“接听”：值为通话 id，来电页按同样流程（含权限申请）接听。 */
    val acceptCall = MutableStateFlow<String?>(null)
}
