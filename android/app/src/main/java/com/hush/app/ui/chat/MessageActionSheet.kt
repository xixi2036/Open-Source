package com.hush.app.ui.chat

import androidx.compose.material.icons.filled.Checklist
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.widget.Toast
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Reply
import androidx.compose.material.icons.automirrored.filled.Forward
import androidx.compose.material.icons.automirrored.filled.Undo
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Info
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.hush.app.core.HushCore
import com.hush.app.ui.components.ConfirmDialog
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.TimeFormat
import uniffi.hushcore.MessageBody
import uniffi.hushcore.MessageItem
import uniffi.hushcore.MessageStatus

val QuickEmojis = listOf("👍", "❤️", "😂", "😮", "😢", "🙏")

/** 长按消息菜单：回应、回复、复制、转发、撤回、删除、详情。 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MessageActionSheet(m: MessageItem, vm: ChatViewModel, onForward: (MessageItem) -> Unit, onSelect: (MessageItem) -> Unit, onDismiss: () -> Unit) {
    val context = LocalContext.current
    val sheet = rememberModalBottomSheetState(skipPartiallyExpanded = true)
    var confirmRecall by remember { mutableStateOf(false) }
    var showInfo by remember { mutableStateOf(false) }
    val visible = m.body is MessageBody.Text || m.body is MessageBody.Attachments
    val canRecall = m.isOutgoing && visible && m.status != MessageStatus.FAILED && m.status != MessageStatus.SENDING &&
        System.currentTimeMillis() - m.sentAt < 24 * 3600 * 1000L
    // U2：文字与已上传的附件可转发（发送中/失败的附件还没有服务端 id）
    val canForward = Selection.forwardable(m)
    val myReaction = m.reactions.firstOrNull { HushCore.myId in it.userIds }?.emoji

    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = sheet, containerColor = Hush.colors.background) {
        Column(Modifier.fillMaxWidth().navigationBarsPadding().padding(bottom = 12.dp)) {
            if (visible) {
                Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp), horizontalArrangement = Arrangement.SpaceBetween) {
                    QuickEmojis.forEach { e ->
                        Box(
                            Modifier.size(48.dp).clip(CircleShape)
                                .background(if (e == myReaction) Hush.colors.accent.copy(alpha = 0.2f) else Hush.colors.surface)
                                .clickable { vm.react(m, e); onDismiss() }
                                .testTag("emoji_$e"),
                            contentAlignment = Alignment.Center,
                        ) { Text(e, fontSize = 24.sp) }
                    }
                }
                Spacer(Modifier.height(8.dp))
                SheetItem(Icons.AutoMirrored.Filled.Reply, "回复", "act_reply") { vm.setReply(m); onDismiss() }
            }
            val text = (m.body as? MessageBody.Text)?.text ?: (m.body as? MessageBody.Attachments)?.caption
            if (!text.isNullOrEmpty()) {
                SheetItem(Icons.Filled.ContentCopy, "复制", "act_copy") {
                    val cm = context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
                    cm.setPrimaryClip(ClipData.newPlainText("Hush", text))
                    Toast.makeText(context, "已复制", Toast.LENGTH_SHORT).show()
                    onDismiss()
                }
            }
            if (canForward) SheetItem(Icons.AutoMirrored.Filled.Forward, "转发", "act_forward") { onDismiss(); onForward(m) }
            if (Selection.selectable(m)) SheetItem(Icons.Filled.Checklist, "多选", "act_select") { onDismiss(); onSelect(m) }
            if (canRecall) SheetItem(Icons.AutoMirrored.Filled.Undo, "撤回", "act_recall") { confirmRecall = true }
            SheetItem(Icons.Filled.Delete, "删除", "act_delete", Hush.colors.danger) { vm.deleteLocal(m); onDismiss() }
            SheetItem(Icons.Filled.Info, "详情", "act_info") { showInfo = true }
        }
    }
    if (confirmRecall) {
        ConfirmDialog("撤回消息", "撤回后双方的聊天记录中都将不再显示这条消息", confirm = "撤回", onConfirm = { vm.recall(m); onDismiss() }, onDismiss = { confirmRecall = false })
    }
    if (showInfo) {
        val status = when (m.status) {
            MessageStatus.SENDING -> "发送中"; MessageStatus.SENT -> "已发送"; MessageStatus.DELIVERED -> "已送达"
            MessageStatus.READ -> "已读"; MessageStatus.FAILED -> "发送失败"
        }
        val lines = buildString {
            append("发送时间：${TimeFormat.daySeparator(m.sentAt)} ${TimeFormat.hhmm(m.sentAt)}\n")
            if (m.serverTs > 0) append("服务器时间：${TimeFormat.hhmm(m.serverTs)}\n")
            append("状态：$status")
            if (m.expireSecs > 0u) append("\n阅后即焚：${TimeFormat.disappearing(m.expireSecs.toInt())}")
        }
        ConfirmDialog("消息详情", lines, confirm = "好", onConfirm = {}, onDismiss = { showInfo = false; onDismiss() })
    }
}

@Composable
private fun SheetItem(icon: ImageVector, label: String, tag: String, color: Color = Color.Unspecified, onClick: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onClick).padding(horizontal = 24.dp, vertical = 14.dp).testTag(tag),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, null, tint = if (color != Color.Unspecified) color else Hush.colors.textPrimary)
        Spacer(Modifier.width(16.dp))
        Text(label, style = HushType.body, color = if (color != Color.Unspecified) color else Hush.colors.textPrimary)
    }
}
