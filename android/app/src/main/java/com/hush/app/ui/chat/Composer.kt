package com.hush.app.ui.chat

import android.Manifest
import android.content.pm.PackageManager
import android.widget.Toast
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Reply
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.AttachFile
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Image
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.platform.LocalSoftwareKeyboardController
import androidx.compose.runtime.collectAsState
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.input.OffsetMapping
import androidx.compose.ui.text.input.TransformedText
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import com.hush.app.util.MentionSpan
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import com.hush.app.ui.components.rememberName
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.R
import com.hush.app.util.DebugTextSink
import com.hush.app.util.MessageText
import com.hush.app.util.TimeFormat
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import uniffi.hushcore.MessageItem

/** 输入框中的 @ 片段用强调色显示（偏移不变）。 */
private fun mentionHighlight(spans: List<MentionSpan>, color: Color): VisualTransformation {
    if (spans.isEmpty()) return VisualTransformation.None
    return VisualTransformation { t ->
        val styled = buildAnnotatedString {
            append(t)
            spans.forEach { if (it.end <= t.length) addStyle(SpanStyle(color = color), it.start, it.end) }
        }
        TransformedText(styled, OffsetMapping.Identity)
    }
}

/** 输入栏：“+”附件、多行文本、发送/按住录音。 */
@Composable
fun Composer(vm: ChatViewModel, replyTo: MessageItem?) {
    val context = LocalContext.current
    // 输入内容保存在 ViewModel：进入时从核心草稿恢复、离开时 set_draft（U3），并维护 @ 区间（U4）
    val composer by vm.composer.collectAsState()
    val text = composer.value.text
    var attachMenu by remember { mutableStateOf(false) }
    var recording by remember { mutableStateOf(false) }
    var cancelArmed by remember { mutableStateOf(false) }
    var recStart by remember { mutableLongStateOf(0L) }
    var elapsed by remember { mutableLongStateOf(0L) }
    val recorder = remember { VoiceRecorder(context) }
    // 按住录音时输入栏被移出组合（被移出群、登出、返回等）：手势协程被取消，必须在这里停止录音、释放麦克风
    DisposableEffect(recorder) { onDispose { recorder.cancel() } }

    DebugTextSink { vm.setComposerText(it) }
    // 选择“回复”后聚焦输入框并弹出键盘
    val focus = remember { FocusRequester() }
    val keyboard = LocalSoftwareKeyboardController.current
    LaunchedEffect(replyTo?.id) {
        if (replyTo != null) {
            runCatching { focus.requestFocus() }
            keyboard?.show()
        }
    }
    LaunchedEffect(recording) {
        while (recording) { elapsed = System.currentTimeMillis() - recStart; delay(200) }
    }

    val pickImage = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri -> uri?.let { vm.sendImage(it) } }
    val pickFile = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri -> uri?.let { vm.sendFile(it) } }
    val micDenied = stringResource(R.string.perm_mic_voice_message)
    val micPerm = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { ok ->
        if (!ok) Toast.makeText(context, micDenied, Toast.LENGTH_SHORT).show()
    }

    if (composer.mentionAt != null) MentionPicker(vm)
    Column(Modifier.fillMaxWidth().background(Hush.colors.background)) {
        if (replyTo != null) {
            Row(
                Modifier.fillMaxWidth().background(Hush.colors.surface).padding(start = 16.dp, top = 6.dp, bottom = 6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(Icons.AutoMirrored.Filled.Reply, null, tint = Hush.colors.accent, modifier = Modifier.size(18.dp))
                Spacer(Modifier.width(8.dp))
                Column(Modifier.weight(1f)) {
                    Text("回复 ${if (replyTo.isOutgoing) "自己" else rememberName(replyTo.senderId, "对方")}", style = HushType.secondary, color = Hush.colors.accent)
                    Text(MessageText.preview(replyTo), style = HushType.secondary, color = Hush.colors.textSecondary, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
                IconButton(onClick = { vm.setReply(null) }) { Icon(Icons.Filled.Close, "取消回复", tint = Hush.colors.textSecondary) }
            }
        }
        if (recording) {
            Box(
                Modifier.fillMaxWidth().background(if (cancelArmed) Hush.colors.danger.copy(alpha = 0.12f) else Hush.colors.accent.copy(alpha = 0.1f)).padding(10.dp),
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    "● ${TimeFormat.duration(elapsed / 1000)}   " + if (cancelArmed) "松开手指，取消发送" else "松开发送，上滑取消",
                    color = if (cancelArmed) Hush.colors.danger else Hush.colors.accent, style = HushType.body,
                    modifier = Modifier.testTag("recording"),
                )
            }
        }
        Row(Modifier.fillMaxWidth().padding(horizontal = 6.dp, vertical = 6.dp), verticalAlignment = Alignment.Bottom) {
            Box {
                IconButton(onClick = { attachMenu = true }, modifier = Modifier.testTag("attach")) {
                    Icon(Icons.Filled.Add, "附件", tint = Hush.colors.textSecondary)
                }
                DropdownMenu(expanded = attachMenu, onDismissRequest = { attachMenu = false }) {
                    DropdownMenuItem(text = { Text("相册图片") }, leadingIcon = { Icon(Icons.Filled.Image, null) }, modifier = Modifier.testTag("attach_image"), onClick = {
                        attachMenu = false; pickImage.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly))
                    })
                    DropdownMenuItem(text = { Text("文件") }, leadingIcon = { Icon(Icons.Filled.AttachFile, null) }, modifier = Modifier.testTag("attach_file"), onClick = {
                        attachMenu = false; pickFile.launch(arrayOf("*/*"))
                    })
                }
            }
            BasicTextField(
                value = composer.value,
                onValueChange = vm::onComposerChange,
                visualTransformation = mentionHighlight(composer.spans, Hush.colors.accent),
                maxLines = 5,
                textStyle = HushType.body.copy(color = Hush.colors.textPrimary),
                cursorBrush = SolidColor(Hush.colors.accent),
                modifier = Modifier.weight(1f).padding(vertical = 4.dp).focusRequester(focus).testTag("composer"),
                decorationBox = { inner ->
                    Box(
                        Modifier.clip(RoundedCornerShape(20.dp)).background(Hush.colors.surface).heightIn(min = 40.dp)
                            .padding(horizontal = 14.dp, vertical = 9.dp),
                        contentAlignment = Alignment.CenterStart,
                    ) {
                        if (text.isEmpty()) Text("发送消息", color = Hush.colors.textSecondary, style = HushType.body)
                        inner()
                    }
                },
            )
            Spacer(Modifier.width(6.dp))
            if (text.isNotBlank()) {
                Box(
                    Modifier.padding(bottom = 2.dp).size(44.dp).clip(CircleShape).background(Hush.colors.accent)
                        .clickable { vm.sendComposer() }
                        .testTag("send"),
                    contentAlignment = Alignment.Center,
                ) { Icon(Icons.AutoMirrored.Filled.Send, "发送", tint = Color.White, modifier = Modifier.size(22.dp)) }
            } else {
                Box(
                    Modifier.padding(bottom = 2.dp).size(44.dp).clip(CircleShape)
                        .background(if (recording) Hush.colors.accent else Hush.colors.surface)
                        .testTag("mic")
                        .pointerInput(Unit) {
                            awaitEachGesture {
                                val down = awaitFirstDown()
                                if (ContextCompat.checkSelfPermission(context, Manifest.permission.RECORD_AUDIO) != PackageManager.PERMISSION_GRANTED) {
                                    micPerm.launch(Manifest.permission.RECORD_AUDIO); return@awaitEachGesture
                                }
                                if (!recorder.start()) {
                                    Toast.makeText(context, "无法开始录音", Toast.LENGTH_SHORT).show(); return@awaitEachGesture
                                }
                                recStart = System.currentTimeMillis(); elapsed = 0; recording = true; cancelArmed = false
                                val startY = down.position.y
                                try {
                                    while (true) {
                                        val ev = awaitPointerEvent()
                                        val change = ev.changes.firstOrNull { it.id == down.id } ?: break
                                        cancelArmed = startY - change.position.y > 150f
                                        if (!change.pressed) break
                                    }
                                } catch (e: CancellationException) {
                                    // 手势被取消（界面移除、手势被父级抢走）：丢弃录音
                                    recording = false
                                    recorder.cancel()
                                    throw e
                                }
                                recording = false
                                if (cancelArmed) recorder.cancel()
                                else {
                                    val res = recorder.stop()
                                    if (res == null) Toast.makeText(context, "说话时间太短", Toast.LENGTH_SHORT).show()
                                    else vm.sendVoice(res.first, res.second)
                                }
                            }
                        },
                    contentAlignment = Alignment.Center,
                ) { Icon(Icons.Filled.Mic, "按住录音", tint = if (recording) Color.White else Hush.colors.textSecondary) }
            }
        }
    }
}
