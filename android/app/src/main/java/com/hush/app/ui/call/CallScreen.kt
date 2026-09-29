package com.hush.app.ui.call

import android.Manifest
import android.content.pm.PackageManager
import android.widget.Toast
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.CallEnd
import androidx.compose.material.icons.filled.Cameraswitch
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material.icons.filled.VideocamOff
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import com.hush.app.R
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.components.rememberName
import com.hush.app.ui.theme.Hush
import com.hush.app.util.TimeFormat
import kotlinx.coroutines.delay
import org.webrtc.RendererCommon
import org.webrtc.SurfaceViewRenderer
import org.webrtc.VideoTrack

/** 全屏通话页：呼叫中 / 来电 / 通话中 / 已结束。 */
@Composable
fun CallScreen(s: CallUiState) {
    BackHandler { /* 通话页不响应返回键，需挂断 */ }
    com.hush.app.ui.components.LightStatusBarIcons()
    // 来电/去电页盖在聊天页之上时收起键盘，否则键盘会挡住接听/挂断按钮
    val keyboard = androidx.compose.ui.platform.LocalSoftwareKeyboardController.current
    val focusManager = androidx.compose.ui.platform.LocalFocusManager.current
    LaunchedEffect(Unit) {
        focusManager.clearFocus(force = true)
        keyboard?.hide()
    }
    val name = rememberName(s.peerId, "对方")
    Box(
        Modifier.fillMaxSize()
            .background(Brush.verticalGradient(listOf(Hush.colors.callBackgroundTop, Hush.colors.callBackgroundBottom)))
            .testTag("call_screen"),
    ) {
        // 只有真正收到对方画面时才铺满远端渲染器；否则显示与语音通话相同的头像占位，避免整块黑屏
        val showRemote = s.video && s.phase == CallPhase.ACTIVE && s.remoteVideo != null && s.remoteHasVideo
        if (showRemote && s.remoteVideo != null) {
            VideoView(s.remoteVideo, mirror = false, modifier = Modifier.fillMaxSize(), tag = "call_remote_video")
        }
        val pipModifier = Modifier.statusBarsPadding().padding(12.dp).size(110.dp, 160.dp).clip(RoundedCornerShape(12.dp)).align(Alignment.TopEnd)
        val localLive = s.video && s.localVideo != null && !s.cameraOff && s.cameraIssue == null
        if (localLive && s.localVideo != null) {
            // 接通前本地预览铺满；接通后始终是右上角小窗（对方无画面时让出位置给头像占位）
            val full = s.phase != CallPhase.ACTIVE
            VideoView(
                s.localVideo, mirror = s.frontCamera,
                modifier = if (full) Modifier.fillMaxSize() else pipModifier,
                tag = "call_local_video",
            )
        } else if (s.video && s.phase in setOf(CallPhase.OUTGOING, CallPhase.CONNECTING, CallPhase.ACTIVE)) {
            val hint = s.cameraIssue ?: if (s.cameraOff) "摄像头已关闭" else null
            if (hint != null) LocalPlaceholder(hint, pipModifier)
        }
        Column(
            Modifier.fillMaxSize().statusBarsPadding().navigationBarsPadding().padding(24.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            val showPeer = !showRemote
            Spacer(Modifier.height(64.dp))
            if (showPeer) {
                UserAvatar(s.peerId, 112.dp)
                Spacer(Modifier.height(20.dp))
                Text(name, color = Color.White, fontSize = 26.sp)
                Spacer(Modifier.height(8.dp))
            }
            StatusLine(s)
            if (s.video && s.phase == CallPhase.ACTIVE && !showRemote) RemoteVideoHint(s.connectedAt)
            Spacer(Modifier.weight(1f))
            Controls(s)
        }
    }
}

@Composable
private fun StatusLine(s: CallUiState) {
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(s.phase) { while (s.phase == CallPhase.ACTIVE) { now = System.currentTimeMillis(); delay(500) } }
    val text = when (s.phase) {
        CallPhase.OUTGOING -> "正在呼叫…"
        CallPhase.INCOMING -> if (s.video) "视频来电" else "语音来电"
        CallPhase.CONNECTING -> "正在连接…"
        CallPhase.ACTIVE -> TimeFormat.duration((now - s.connectedAt) / 1000)
        CallPhase.ENDED -> s.endReason ?: "通话已结束"
        CallPhase.IDLE -> ""
    }
    Text(text, color = Color.White.copy(alpha = 0.8f), fontSize = 16.sp, modifier = Modifier.testTag("call_status"))
}

/** 视频通话中对方没有画面时的提示：接通后前 3 秒视为等待首帧。 */
@Composable
private fun RemoteVideoHint(connectedAt: Long) {
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    val waiting = now - connectedAt < 3_000
    LaunchedEffect(waiting) { while (waiting) { delay(250); now = System.currentTimeMillis() } }
    Spacer(Modifier.height(12.dp))
    Text(
        if (waiting) "等待对方画面…" else "对方未开启摄像头",
        color = Color.White.copy(alpha = 0.6f), fontSize = 14.sp,
        modifier = Modifier.testTag("call_remote_video_hint"),
    )
}

/** 本地小窗占位：摄像头已关闭 / 无可用摄像头。 */
@Composable
private fun LocalPlaceholder(text: String, modifier: Modifier) {
    Column(
        modifier.background(Color.Black.copy(alpha = 0.45f)).testTag("call_local_placeholder"),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.Center,
    ) {
        Icon(Icons.Filled.VideocamOff, null, tint = Color.White.copy(alpha = 0.8f), modifier = Modifier.size(28.dp))
        Spacer(Modifier.height(8.dp))
        Text(text, color = Color.White.copy(alpha = 0.8f), fontSize = 12.sp, textAlign = TextAlign.Center, modifier = Modifier.padding(horizontal = 6.dp))
    }
}

@Composable
private fun Controls(s: CallUiState) {
    when (s.phase) {
        CallPhase.INCOMING -> Row(Modifier.fillMaxWidth().padding(bottom = 32.dp), horizontalArrangement = Arrangement.SpaceEvenly) {
            RoundButton(Icons.Filled.CallEnd, "拒绝", Hush.colors.danger, tag = "call_reject") { CallManager.reject() }
            AcceptButton(s.video)
        }
        CallPhase.ENDED -> Unit
        else -> Column(horizontalAlignment = Alignment.CenterHorizontally, modifier = Modifier.padding(bottom = 24.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceEvenly) {
                ToggleButton(if (s.muted) Icons.Filled.MicOff else Icons.Filled.Mic, if (s.muted) "取消静音" else "静音", s.muted) { CallManager.toggleMute() }
                ToggleButton(Icons.AutoMirrored.Filled.VolumeUp, "扬声器", s.speaker) { CallManager.toggleSpeaker() }
                if (s.video && s.cameraIssue != null) {
                    ToggleButton(Icons.Filled.VideocamOff, "无摄像头", true) {}
                } else if (s.video) {
                    ToggleButton(if (s.cameraOff) Icons.Filled.VideocamOff else Icons.Filled.Videocam, if (s.cameraOff) "开摄像头" else "关摄像头", s.cameraOff) { CallManager.toggleCamera() }
                    if (!s.cameraOff) ToggleButton(Icons.Filled.Cameraswitch, "切换", false) { CallManager.switchCamera() }
                }
            }
            Spacer(Modifier.height(32.dp))
            RoundButton(Icons.Filled.CallEnd, "挂断", Hush.colors.danger, tag = "call_hangup") { CallManager.hangup() }
        }
    }
}

@Composable
private fun AcceptButton(video: Boolean) {
    val context = LocalContext.current
    val denied = stringResource(R.string.perm_call_answer_mic)
    val perms = if (video) arrayOf(Manifest.permission.RECORD_AUDIO, Manifest.permission.CAMERA) else arrayOf(Manifest.permission.RECORD_AUDIO)
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { res ->
        if (res[Manifest.permission.RECORD_AUDIO] == true) CallManager.accept()
        else Toast.makeText(context, denied, Toast.LENGTH_SHORT).show()
    }
    fun accept() {
        if (perms.all { ContextCompat.checkSelfPermission(context, it) == PackageManager.PERMISSION_GRANTED }) CallManager.accept()
        else launcher.launch(perms)
    }
    // 从来电通知的“接听”按钮进入
    val requested by com.hush.app.ui.nav.NavRequests.acceptCall.collectAsState()
    LaunchedEffect(requested) {
        val id = requested ?: return@LaunchedEffect
        com.hush.app.ui.nav.NavRequests.acceptCall.value = null
        if (id == CallManager.state.value.callId) accept()
    }
    RoundButton(if (video) Icons.Filled.Videocam else Icons.Filled.Call, "接听", Hush.colors.success, tag = "call_accept") { accept() }
}

@Composable
private fun RoundButton(icon: ImageVector, label: String, color: Color, tag: String, onClick: () -> Unit) {
    Column(horizontalAlignment = Alignment.CenterHorizontally) {
        Box(Modifier.size(68.dp).clip(CircleShape).background(color).clickable(onClick = onClick).testTag(tag), contentAlignment = Alignment.Center) {
            Icon(icon, label, tint = Color.White, modifier = Modifier.size(32.dp))
        }
        Spacer(Modifier.height(8.dp))
        Text(label, color = Color.White, fontSize = 13.sp)
    }
}

@Composable
private fun ToggleButton(icon: ImageVector, label: String, active: Boolean, onClick: () -> Unit) {
    Column(horizontalAlignment = Alignment.CenterHorizontally, modifier = Modifier.width(72.dp)) {
        Box(
            Modifier.size(56.dp).clip(CircleShape).background(if (active) Color.White else Color.White.copy(alpha = 0.18f)).clickable(onClick = onClick).testTag("call_$label"),
            contentAlignment = Alignment.Center,
        ) { Icon(icon, label, tint = if (active) Hush.colors.callBackgroundTop else Color.White) }
        Spacer(Modifier.height(6.dp))
        Text(label, color = Color.White, fontSize = 12.sp)
    }
}

@Composable
private fun VideoView(track: VideoTrack, mirror: Boolean, modifier: Modifier, tag: String) {
    val context = LocalContext.current
    // 本地画面的 SurfaceView 固定在媒体覆盖层：z 序在 attach 后改不了，若按“是否小窗”决定，
    // 从接通前的全屏预览缩成小窗时会被后加入的远端 SurfaceView 盖住。
    val overlay = tag == "call_local_video"
    val renderer = remember {
        SurfaceViewRenderer(context).apply {
            init(CallManager.eglBase.eglBaseContext, null)
            setScalingType(RendererCommon.ScalingType.SCALE_ASPECT_FILL)
            setEnableHardwareScaler(true)
            if (overlay) setZOrderMediaOverlay(true)
        }
    }
    DisposableEffect(track) {
        runCatching { track.addSink(renderer) }
        onDispose { runCatching { track.removeSink(renderer) } }
    }
    DisposableEffect(Unit) { onDispose { renderer.release() } }
    AndroidView(factory = { renderer }, modifier = modifier.testTag(tag), update = { it.setMirror(mirror) })
}

/** 发起通话前申请权限的包装：返回 (peerId, video) -> Unit。 */
@Composable
fun rememberCallStarter(): (String, Boolean) -> Unit {
    val context = LocalContext.current
    val denied = stringResource(R.string.perm_call_mic)
    var pending by remember { androidx.compose.runtime.mutableStateOf<Pair<String, Boolean>?>(null) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { res ->
        val p = pending ?: return@rememberLauncherForActivityResult
        pending = null
        if (res[Manifest.permission.RECORD_AUDIO] == true || ContextCompat.checkSelfPermission(context, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) {
            CallManager.startCall(p.first, p.second)
        } else Toast.makeText(context, denied, Toast.LENGTH_SHORT).show()
    }
    return remember(launcher) {
        { peer, video ->
            val perms = if (video) arrayOf(Manifest.permission.RECORD_AUDIO, Manifest.permission.CAMERA) else arrayOf(Manifest.permission.RECORD_AUDIO)
            if (CallManager.isBusy) Toast.makeText(context, "正在通话中", Toast.LENGTH_SHORT).show()
            else if (perms.all { ContextCompat.checkSelfPermission(context, it) == PackageManager.PERMISSION_GRANTED }) CallManager.startCall(peer, video)
            else { pending = peer to video; launcher.launch(perms) }
        }
    }
}
