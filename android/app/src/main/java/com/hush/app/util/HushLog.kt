package com.hush.app.util

import android.util.Log
import com.hush.app.BuildConfig

/**
 * 日志封装：只在 debug 构建输出。release 构建不向 logcat 写入错误文本、信令动作等
 * （核心错误信息可能包含用户输入或服务端返回的内容）。
 */
object HushLog {
    fun i(tag: String, msg: String) {
        if (BuildConfig.DEBUG) Log.i(tag, msg)
    }

    fun w(tag: String, msg: String, t: Throwable? = null) {
        if (BuildConfig.DEBUG) Log.w(tag, msg, t)
    }
}
