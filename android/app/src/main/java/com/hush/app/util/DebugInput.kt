package com.hush.app.util

import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.rememberUpdatedState
import com.hush.app.BuildConfig
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.SharedFlow

/**
 * 测试输入通道：debug 构建中的 DebugTextReceiver（src/debug，需 DUMP 权限，仅 adb shell 可发）
 * 把 adb 广播的文字（可含中文）投递到这里。release 构建中没有发送方，[DebugTextSink] 也不订阅。
 */
object DebugInput {
    private val _text = MutableSharedFlow<String>(extraBufferCapacity = 8)
    val text: SharedFlow<String> = _text
    fun post(t: String) = _text.tryEmit(t)
}

/** 当前可见的输入框接收调试文字；release 构建为空操作。各界面只需调用这一处。 */
@Composable
fun DebugTextSink(onText: (String) -> Unit) {
    if (!BuildConfig.DEBUG) return
    val latest = rememberUpdatedState(onText)
    LaunchedEffect(Unit) { DebugInput.text.collect { latest.value(it) } }
}
