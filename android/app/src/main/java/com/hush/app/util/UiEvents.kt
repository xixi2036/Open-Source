package com.hush.app.util

import android.widget.Toast
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.platform.LocalContext
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableSharedFlow

/** ViewModel 一次性提示消息：`private val _toast = toastFlow()`，界面用 [ToastCollector] 显示。 */
fun toastFlow(): MutableSharedFlow<String> = MutableSharedFlow(extraBufferCapacity = 4)

/** 收集一次性提示并以 Toast 显示。 */
@Composable
fun ToastCollector(messages: Flow<String>) {
    val context = LocalContext.current
    LaunchedEffect(messages) { messages.collect { Toast.makeText(context, it, Toast.LENGTH_SHORT).show() } }
}
