package com.hush.app.ui.me

import android.app.Activity
import android.widget.Toast
import androidx.activity.compose.BackHandler
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricPrompt
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.foundation.focusable
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import com.hush.app.R
import com.hush.app.core.AppLock
import com.hush.app.core.HushCore
import com.hush.app.ui.auth.PrimaryButton
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType

/** 屏幕锁在 canAuthenticate 各种返回值下的处理（JVM 单测见 LockPolicyTest）。 */
object LockPolicy {
    enum class Action {
        /** 可以验证：弹出 BiometricPrompt。 */
        PROMPT,
        /** 设备上已没有任何锁屏凭据：无法验证，关闭屏幕锁并提示，避免把用户永久锁在外面。 */
        DISABLE,
        /** 暂时无法验证（硬件不可用、状态未知、需要安全更新等）：保持锁定，显示原因并允许重试。 */
        RETRY,
    }

    fun decide(status: Int): Action = when (status) {
        BiometricManager.BIOMETRIC_SUCCESS -> Action.PROMPT
        BiometricManager.BIOMETRIC_ERROR_NONE_ENROLLED -> Action.DISABLE
        else -> Action.RETRY
    }

    fun reason(status: Int): String = when (status) {
        BiometricManager.BIOMETRIC_ERROR_HW_UNAVAILABLE -> "生物识别硬件暂时不可用"
        BiometricManager.BIOMETRIC_ERROR_SECURITY_UPDATE_REQUIRED -> "需要安装系统安全更新后才能验证"
        BiometricManager.BIOMETRIC_ERROR_NO_HARDWARE -> "设备不支持所需的验证方式"
        else -> "暂时无法验证身份"
    }
}

/**
 * 弹出系统身份验证（生物识别或设备凭据）。返回 false 表示当前无法验证（[onUnavailable] 已带上原因）。
 * 上锁页解锁、以及在设置里关闭屏幕锁（B12：关闭也必须验证，否则拿到已解锁手机的人可以直接关掉它）共用。
 */
fun authenticate(
    activity: FragmentActivity,
    title: String,
    subtitle: String,
    onSuccess: () -> Unit,
    onError: (String) -> Unit,
    onUnavailable: (LockPolicy.Action, String) -> Unit,
) {
    val auth = BiometricManager.Authenticators.BIOMETRIC_WEAK or BiometricManager.Authenticators.DEVICE_CREDENTIAL
    val status = BiometricManager.from(activity).canAuthenticate(auth)
    when (val action = LockPolicy.decide(status)) {
        LockPolicy.Action.PROMPT -> Unit
        else -> { onUnavailable(action, LockPolicy.reason(status)); return }
    }
    val bp = BiometricPrompt(activity, ContextCompat.getMainExecutor(activity), object : BiometricPrompt.AuthenticationCallback() {
        override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) { onSuccess() }
        override fun onAuthenticationError(code: Int, msg: CharSequence) { onError(msg.toString()) }
    })
    bp.authenticate(BiometricPrompt.PromptInfo.Builder().setTitle(title).setSubtitle(subtitle).setAllowedAuthenticators(auth).build())
}

/** 屏幕锁：BiometricPrompt（允许设备凭据）。作为覆盖层盖在主导航之上，拦截触摸与返回键。 */
@Composable
fun LockScreen() {
    val context = LocalContext.current
    var error by remember { mutableStateOf<String?>(null) }
    fun prompt() {
        val activity = context as? FragmentActivity ?: return
        authenticate(
            activity, "解锁 Hush", "验证身份以继续",
            onSuccess = { AppLock.unlock() },
            onError = { error = it },
            onUnavailable = { action, reason ->
                if (action == LockPolicy.Action.DISABLE) {
                    HushCore.prefs.setScreenLock(false); AppLock.unlock()
                    Toast.makeText(context, "设备未设置锁屏密码或生物识别，屏幕锁已关闭", Toast.LENGTH_LONG).show()
                } else error = "$reason，请稍后点“解锁”重试"
            },
        )
    }
    LaunchedEffect(Unit) { prompt() }
    // A1：锁屏页自己持有焦点，键盘/硬件按键输入不会落到下层的输入框
    val holder = remember { FocusRequester() }
    LaunchedEffect(Unit) { runCatching { holder.requestFocus() } }
    // 锁定期间返回键不得操作下层导航：直接把应用退到后台
    BackHandler { (context as? Activity)?.moveTaskToBack(true) }
    Column(
        Modifier.fillMaxSize().background(Hush.colors.background)
            // 拦截触摸，不让事件落到下层的主导航
            .pointerInput(Unit) { detectTapGestures { } }
            .focusRequester(holder).focusable()
            // 吞掉全部按键（硬件键盘、adb input），返回键由上面的 BackHandler 处理
            .onPreviewKeyEvent { it.key != Key.Back }
            .testTag("lock_screen")
            .padding(32.dp),
        horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.Center,
    ) {
        Image(painterResource(R.drawable.hush_logo), "Hush", modifier = Modifier.size(96.dp).clip(RoundedCornerShape(24.dp)))
        Spacer(Modifier.height(20.dp))
        Text("Hush 已锁定", style = HushType.title, color = Hush.colors.textPrimary)
        error?.let { Spacer(Modifier.height(8.dp)); Text(it, color = Hush.colors.textSecondary, style = HushType.secondary) }
        Spacer(Modifier.height(32.dp))
        PrimaryButton("解锁", onClick = { prompt() })
    }
}
