package com.hush.app.ui.auth

import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.animation.AnimatedContent
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.PhotoCamera
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.OutlinedTextFieldDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import com.hush.app.BuildConfig
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.viewmodel.compose.viewModel
import coil3.compose.AsyncImage
import com.hush.app.R
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import java.io.File

@Composable
fun AuthScreen(vm: AuthViewModel = viewModel()) {
    val s by vm.state.collectAsState()
    BackHandler(enabled = s.step != AuthStep.WELCOME && s.step != AuthStep.PROFILE) { vm.back() }

    Box(Modifier.fillMaxSize().background(Hush.colors.background).systemBarsPadding().imePadding()) {
        AnimatedContent(targetState = s.step, label = "auth") { step ->
            when (step) {
                AuthStep.WELCOME -> Welcome { vm.go(AuthStep.SERVER) }
                AuthStep.SERVER -> StepPage(
                    title = "服务器地址", desc = "本地部署的 Hush 服务器地址", onBack = { vm.back() },
                    button = "下一步", busy = false, error = s.error, onNext = vm::submitServer,
                ) {
                    Field(s.server, vm::setServer, "http://10.0.2.2:8080", KeyboardType.Uri, "server", vm::submitServer)
                }
                AuthStep.EMAIL -> StepPage(
                    title = "输入你的邮箱", desc = "我们会向该邮箱发送 6 位验证码", onBack = { vm.back() },
                    button = "下一步", busy = s.busy, error = s.error, onNext = vm::requestCode,
                ) {
                    Field(s.email, vm::setEmail, "name@example.com", KeyboardType.Email, "email", vm::requestCode)
                }
                AuthStep.CODE -> StepPage(
                    title = "输入验证码",
                    desc = "验证码已发送至 ${s.email}" + if (BuildConfig.DEBUG) "\n开发模式验证码见服务器日志（默认 123456）" else "",
                    onBack = { vm.back() }, button = null, busy = s.busy, error = s.error, onNext = {},
                ) {
                    CodeInput(s.code, vm::setCode)
                    Spacer(Modifier.height(16.dp))
                    TextButton(onClick = vm::requestCode, enabled = s.resendIn == 0 && !s.busy) {
                        Text(if (s.resendIn > 0) "重新发送（${s.resendIn}s）" else "重新发送")
                    }
                }
                AuthStep.PROFILE -> ProfileStep(s, vm)
            }
        }
    }
}

@Composable
private fun Welcome(onStart: () -> Unit) {
    Column(
        Modifier.fillMaxSize().padding(24.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Spacer(Modifier.weight(1f))
        Image(
            painterResource(R.drawable.hush_logo), "Hush",
            modifier = Modifier.size(120.dp).clip(RoundedCornerShape(28.dp)),
        )
        Spacer(Modifier.height(24.dp))
        Text("Hush 密语", style = HushType.largeTitle, color = Hush.colors.textPrimary)
        Spacer(Modifier.height(8.dp))
        Text("端到端加密的私密聊天", style = HushType.body, color = Hush.colors.textSecondary)
        Spacer(Modifier.weight(1.2f))
        PrimaryButton("开始使用", onClick = onStart, modifier = Modifier.testTag("start"))
        Spacer(Modifier.height(24.dp))
    }
}

@Composable
private fun StepPage(
    title: String,
    desc: String,
    onBack: () -> Unit,
    button: String?,
    busy: Boolean,
    error: String?,
    onNext: () -> Unit,
    content: @Composable () -> Unit,
) {
    Column(Modifier.fillMaxSize().padding(horizontal = 24.dp)) {
        IconButton(onClick = onBack, modifier = Modifier.padding(top = 4.dp)) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "返回") }
        Spacer(Modifier.height(24.dp))
        Text(title, style = HushType.largeTitle.copy(fontSize = 26.sp), color = Hush.colors.textPrimary)
        Spacer(Modifier.height(8.dp))
        Text(desc, style = HushType.body.copy(fontSize = 14.sp), color = Hush.colors.textSecondary)
        Spacer(Modifier.height(32.dp))
        content()
        if (error != null) {
            Spacer(Modifier.height(12.dp))
            Text(error, color = Hush.colors.danger, style = HushType.secondary)
        }
        Spacer(Modifier.weight(1f))
        if (button != null) PrimaryButton(button, onClick = onNext, busy = busy, modifier = Modifier.testTag("next"))
        else if (busy) Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) { CircularProgressIndicator() }
        Spacer(Modifier.height(24.dp))
    }
}

@Composable
fun PrimaryButton(text: String, onClick: () -> Unit, modifier: Modifier = Modifier, busy: Boolean = false, enabled: Boolean = true) {
    Button(
        onClick = onClick, enabled = enabled && !busy,
        modifier = modifier.fillMaxWidth().height(52.dp),
        shape = RoundedCornerShape(26.dp),
        colors = ButtonDefaults.buttonColors(containerColor = Hush.colors.accent, contentColor = Hush.colors.onAccent),
    ) {
        if (busy) CircularProgressIndicator(Modifier.size(22.dp), color = Color.White, strokeWidth = 2.dp)
        else Text(text, style = HushType.bodyMedium)
    }
}

@Composable
private fun Field(value: String, onChange: (String) -> Unit, placeholder: String, type: KeyboardType, tag: String, onDone: () -> Unit) {
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    OutlinedTextField(
        value = value, onValueChange = onChange, singleLine = true,
        placeholder = { Text(placeholder, color = Hush.colors.textSecondary) },
        keyboardOptions = KeyboardOptions(keyboardType = type, imeAction = ImeAction.Next),
        keyboardActions = KeyboardActions(onNext = { onDone() }),
        shape = RoundedCornerShape(12.dp),
        colors = OutlinedTextFieldDefaults.colors(
            focusedBorderColor = Hush.colors.accent, unfocusedBorderColor = Hush.colors.divider,
            focusedContainerColor = Hush.colors.surface, unfocusedContainerColor = Hush.colors.surface,
        ),
        modifier = Modifier.fillMaxWidth().focusRequester(focus).testTag(tag),
    )
}

/** 6 位验证码：隐藏的 BasicTextField + 6 个格子；自动聚焦，输满自动提交。 */
@Composable
private fun CodeInput(code: String, onChange: (String) -> Unit) {
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    BasicTextField(
        value = code, onValueChange = onChange,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword),
        modifier = Modifier.focusRequester(focus).testTag("code"),
        textStyle = TextStyle(color = Color.Transparent),
        decorationBox = {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                repeat(6) { i ->
                    val ch = code.getOrNull(i)?.toString() ?: ""
                    val active = i == code.length
                    Box(
                        Modifier.weight(1f).height(56.dp).clip(RoundedCornerShape(12.dp)).background(Hush.colors.surface)
                            .border(if (active) 2.dp else 1.dp, if (active) Hush.colors.accent else Hush.colors.divider, RoundedCornerShape(12.dp)),
                        contentAlignment = Alignment.Center,
                    ) {
                        Text(ch, style = TextStyle(fontSize = 24.sp, fontWeight = FontWeight.SemiBold), color = Hush.colors.textPrimary)
                    }
                }
            }
        },
    )
}

@Composable
private fun ProfileStep(s: AuthState, vm: AuthViewModel) {
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri -> uri?.let(vm::pickAvatar) }
    Column(Modifier.fillMaxSize().padding(horizontal = 24.dp), horizontalAlignment = Alignment.CenterHorizontally) {
        Spacer(Modifier.height(48.dp))
        Text("设置你的资料", style = HushType.largeTitle.copy(fontSize = 26.sp), color = Hush.colors.textPrimary)
        Spacer(Modifier.height(8.dp))
        Text("好友会看到你的头像和昵称", style = HushType.body.copy(fontSize = 14.sp), color = Hush.colors.textSecondary, textAlign = TextAlign.Center)
        Spacer(Modifier.height(32.dp))
        Box(
            Modifier.size(112.dp).clip(CircleShape)
                .background(Brush.linearGradient(listOf(Hush.colors.accent, Hush.colors.accent.copy(alpha = 0.6f))))
                .clickable { picker.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)) }
                .testTag("avatar"),
            contentAlignment = Alignment.Center,
        ) {
            if (s.avatarPath != null) {
                AsyncImage(File(s.avatarPath), null, contentScale = ContentScale.Crop, modifier = Modifier.fillMaxSize())
            } else {
                Icon(Icons.Filled.PhotoCamera, "选择头像", tint = Color.White, modifier = Modifier.size(40.dp))
            }
        }
        Spacer(Modifier.height(8.dp))
        Text("点击选择头像", style = HushType.secondary, color = Hush.colors.textSecondary)
        Spacer(Modifier.height(24.dp))
        OutlinedTextField(
            value = s.name, onValueChange = vm::setName, singleLine = true,
            label = { Text("昵称（必填）") },
            shape = RoundedCornerShape(12.dp),
            colors = OutlinedTextFieldDefaults.colors(focusedBorderColor = Hush.colors.accent, unfocusedBorderColor = Hush.colors.divider),
            modifier = Modifier.fillMaxWidth().testTag("name"),
        )
        if (s.error != null) {
            Spacer(Modifier.height(12.dp)); Text(s.error, color = Hush.colors.danger, style = HushType.secondary)
        }
        Spacer(Modifier.weight(1f))
        PrimaryButton("完成", onClick = vm::submitProfile, busy = s.busy, enabled = s.name.isNotBlank(), modifier = Modifier.testTag("done"))
        Spacer(Modifier.height(24.dp))
    }
}
