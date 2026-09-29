package com.hush.app.ui.chat

import android.graphics.BitmapFactory
import androidx.compose.animation.animateColorAsState
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.InsertDriveFile
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.Done
import androidx.compose.material.icons.filled.DoneAll
import androidx.compose.material.icons.filled.ErrorOutline
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PhoneMissed
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Schedule
import androidx.compose.material.icons.filled.Timer
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.blur
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import coil3.compose.AsyncImage
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.components.nameColor
import com.hush.app.ui.components.rememberName
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.MediaUtils
import com.hush.app.util.MessageText
import com.hush.app.util.TimeFormat
import uniffi.hushcore.AttachmentItem
import uniffi.hushcore.AttachmentKind
import uniffi.hushcore.CallEndReason
import uniffi.hushcore.MessageBody
import uniffi.hushcore.MessageItem
import uniffi.hushcore.MessageStatus
import uniffi.hushcore.Mention
import com.hush.app.util.MentionEditor
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.material.icons.automirrored.filled.Forward
import java.io.File

class BubbleActions(
    val onLongPress: (MessageItem) -> Unit,
    val onQuoteClick: (String) -> Unit,
    val onImageClick: (MessageItem, Int) -> Unit,
    val onFileClick: (MessageItem, Int) -> Unit,
    val onResend: (MessageItem) -> Unit,
    val onReactionClick: (MessageItem, String) -> Unit,
)

/** 居中灰字（系统消息、撤回提示、日期分隔）。 */
@Composable
fun CenterNote(text: String, italic: Boolean = false, pill: Boolean = false) {
    Box(Modifier.fillMaxWidth().padding(vertical = 6.dp), contentAlignment = Alignment.Center) {
        Text(
            text,
            style = HushType.secondary.copy(fontStyle = if (italic) FontStyle.Italic else FontStyle.Normal),
            color = Hush.colors.textSecondary, textAlign = TextAlign.Center,
            modifier = (if (pill) Modifier.clip(RoundedCornerShape(10.dp)).background(Hush.colors.surface) else Modifier)
                .padding(horizontal = 10.dp, vertical = 3.dp),
        )
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
fun MessageRow(m: MessageItem, isGroup: Boolean, showSenderHeader: Boolean, actions: BubbleActions, highlight: Boolean = false) {
    when {
        m.deleted -> {
            val note = if (m.isOutgoing) "你撤回了一条消息" else "\"${rememberName(m.senderId, "对方")}\" 撤回了一条消息"
            CenterNote(note); return
        }
        m.body is MessageBody.System -> { CenterNote((m.body as MessageBody.System).text, pill = true); return }
    }
    val out = m.isOutgoing
    val highlightColor by animateColorAsState(if (highlight) Hush.colors.accent.copy(alpha = 0.14f) else Color.Transparent, label = "highlight")
    Row(
        Modifier.fillMaxWidth().background(highlightColor).padding(horizontal = 10.dp, vertical = 2.dp),
        horizontalArrangement = if (out) Arrangement.End else Arrangement.Start,
        verticalAlignment = Alignment.Top,
    ) {
        if (!out && isGroup) {
            if (showSenderHeader) UserAvatar(m.senderId, 32.dp, Modifier.padding(top = 18.dp)) else Spacer(Modifier.width(32.dp))
            Spacer(Modifier.width(6.dp))
        }
        if (out && m.status == MessageStatus.FAILED) {
            Icon(
                Icons.Filled.ErrorOutline, "发送失败，点击重发", tint = Hush.colors.danger,
                modifier = Modifier.align(Alignment.CenterVertically).padding(end = 6.dp).size(22.dp).clickable { actions.onResend(m) }.testTag("resend"),
            )
        }
        Column(horizontalAlignment = if (out) Alignment.End else Alignment.Start, modifier = Modifier.widthIn(max = 290.dp)) {
            if (!out && isGroup && showSenderHeader) {
                Text(
                    rememberName(m.senderId, "成员"), style = HushType.secondary, color = nameColor(m.senderId),
                    modifier = Modifier.padding(start = 12.dp, bottom = 2.dp),
                )
            }
            // U4：提到我的消息，气泡左侧强调竖线
            val mentionsMe = !out && m.mentions.any { it.userId == com.hush.app.core.HushCore.myId }
            if (mentionsMe) {
                Row(Modifier.height(IntrinsicSize.Min)) {
                    Box(Modifier.padding(end = 5.dp).width(3.dp).fillMaxHeight().clip(RoundedCornerShape(2.dp)).background(Hush.colors.accent).testTag("mention_bar_${m.id}"))
                    Bubble(m, actions)
                }
            } else Bubble(m, actions)
            if (m.reactions.isNotEmpty()) Reactions(m, actions)
        }
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun Bubble(m: MessageItem, actions: BubbleActions) {
    val out = m.isOutgoing
    val bg = if (out) Hush.colors.accent else Hush.colors.incomingBubble
    val fg = if (out) Hush.colors.onAccent else Hush.colors.textPrimary
    val body = m.body
    val isImageOnly = body is MessageBody.Attachments && body.items.all { it.kind == AttachmentKind.IMAGE } && body.caption.isNullOrEmpty() && m.quote == null
    Column(
        Modifier.clip(RoundedCornerShape(18.dp))
            .background(if (isImageOnly) Color.Transparent else bg)
            .combinedClickable(onClick = {}, onLongClick = { actions.onLongPress(m) })
            .testTag("msg_${m.id}"),
    ) {
        if (m.forwarded) {
            // U2：转发的消息在气泡顶部显示小标签
            Row(
                (if (isImageOnly) Modifier.padding(bottom = 4.dp).clip(RoundedCornerShape(8.dp)).background(bg).padding(horizontal = 8.dp, vertical = 2.dp)
                else Modifier.padding(start = 12.dp, end = 12.dp, top = 6.dp))
                    .testTag("forwarded_${m.id}"),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(Icons.AutoMirrored.Filled.Forward, null, tint = fg.copy(alpha = 0.7f), modifier = Modifier.size(12.dp))
                Spacer(Modifier.width(3.dp))
                Text("转发的消息", style = HushType.timestamp, color = fg.copy(alpha = 0.7f))
            }
        }
        m.quote?.let { q ->
            Column(
                Modifier.padding(start = 8.dp, end = 8.dp, top = 8.dp).clip(RoundedCornerShape(10.dp))
                    .background(if (out) Color.White.copy(alpha = 0.18f) else Hush.colors.background.copy(alpha = 0.6f))
                    .clickable { actions.onQuoteClick(q.id) }
                    .padding(horizontal = 10.dp, vertical = 6.dp),
            ) {
                Text(if (q.author == com.hush.app.core.HushCore.myId) "你" else rememberName(q.author, "对方"), style = HushType.secondary, color = if (out) fg else Hush.colors.accent, maxLines = 1)
                Text(q.text, style = HushType.secondary, color = fg.copy(alpha = 0.85f), maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
        }
        when (body) {
            is MessageBody.Text -> TextContent(body.text, m, fg, mentions = m.mentions)
            is MessageBody.Attachments -> {
                if (isImageOnly) {
                    // 纯图片：时间与状态叠加在图片右下角
                    Box {
                        Column { body.items.forEachIndexed { i, a -> ImageContent(m, a, i, actions, rounded = true) } }
                        Footer(m, fg, overlay = true, modifier = Modifier.align(Alignment.BottomEnd))
                    }
                } else {
                    // 宽度取最宽的子项（图片/文件/说明文字），时间与状态始终贴在气泡右下角
                    Column(Modifier.width(IntrinsicSize.Max)) {
                        body.items.forEachIndexed { i, a ->
                            when (a.kind) {
                                AttachmentKind.IMAGE, AttachmentKind.VIDEO -> ImageContent(m, a, i, actions, rounded = false)
                                AttachmentKind.VOICE -> VoiceContent(m, a, fg)
                                AttachmentKind.FILE -> FileContent(m, a, i, fg, actions)
                            }
                        }
                        if (!body.caption.isNullOrEmpty()) TextContent(body.caption!!, m, fg, Modifier.fillMaxWidth())
                        else Footer(m, fg, modifier = Modifier.align(Alignment.End).padding(end = 12.dp, bottom = 6.dp))
                    }
                }
            }
            is MessageBody.Undecryptable -> {
                Text("⚠️ 无法解密的消息", style = HushType.body.copy(fontStyle = FontStyle.Italic), color = Hush.colors.textSecondary, modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp))
            }
            is MessageBody.CallLog -> CallLogContent(m, body, fg)
            is MessageBody.System -> Unit
        }
    }
}

@Composable
private fun TextContent(text: String, m: MessageItem, fg: Color, modifier: Modifier = Modifier, mentions: List<Mention> = emptyList()) {
    // U4：@ 片段强调显示。发出的气泡底色就是强调色，改用加粗 + 下划线
    val accent = Hush.colors.accent
    val styled = remember(text, mentions, m.isOutgoing, accent) {
        val ranges = MentionEditor.highlightRanges(text, mentions)
        if (ranges.isEmpty()) null else buildAnnotatedString {
            append(text)
            val style = if (m.isOutgoing) SpanStyle(fontWeight = FontWeight.SemiBold, textDecoration = TextDecoration.Underline)
            else SpanStyle(color = accent, fontWeight = FontWeight.Medium)
            ranges.forEach { addStyle(style, it.first, it.last + 1) }
        }
    }
    Column(modifier.padding(start = 12.dp, end = 12.dp, top = 7.dp, bottom = 5.dp)) {
        if (styled != null) Text(styled, style = HushType.body, color = fg) else Text(text, style = HushType.body, color = fg)
        Footer(m, fg, modifier = Modifier.align(Alignment.End))
    }
}

/** 气泡右下角：阅后即焚图标 + 时间 + 状态。 */
@Composable
private fun Footer(m: MessageItem, fg: Color, modifier: Modifier = Modifier, overlay: Boolean = false) {
    val color = if (overlay) Color.White else fg.copy(alpha = 0.72f)
    Row(
        modifier.then(if (overlay) Modifier.padding(6.dp).clip(RoundedCornerShape(8.dp)).background(Color.Black.copy(alpha = 0.35f)).padding(horizontal = 6.dp, vertical = 1.dp) else Modifier.padding(top = 1.dp)),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (m.expireSecs > 0u) {
            Icon(Icons.Filled.Timer, "阅后即焚", tint = color, modifier = Modifier.size(11.dp).testTag("disappearing"))
            Spacer(Modifier.width(3.dp))
        }
        Text(TimeFormat.hhmm(m.sentAt), style = HushType.timestamp, color = color)
        if (m.isOutgoing) {
            Spacer(Modifier.width(3.dp))
            StatusIcon(m.status, color)
        }
    }
}

@Composable
private fun StatusIcon(s: MessageStatus, color: Color) {
    val mod = Modifier.size(14.dp)
    when (s) {
        MessageStatus.SENDING -> Icon(Icons.Filled.Schedule, "发送中", tint = color, modifier = mod.testTag("st_sending"))
        MessageStatus.SENT -> Icon(Icons.Filled.Done, "已发送", tint = color, modifier = mod.testTag("st_sent"))
        MessageStatus.DELIVERED -> Icon(Icons.Filled.DoneAll, "已送达", tint = color, modifier = mod.testTag("st_delivered"))
        MessageStatus.READ -> Box(Modifier.size(15.dp).clip(CircleShape).background(color).testTag("st_read"), contentAlignment = Alignment.Center) {
            Icon(Icons.Filled.DoneAll, "已读", tint = Hush.colors.accent, modifier = Modifier.size(11.dp))
        }
        MessageStatus.FAILED -> Icon(Icons.Filled.ErrorOutline, "发送失败", tint = Hush.colors.danger, modifier = mod)
    }
}

private fun displaySize(a: AttachmentItem): Pair<Dp, Dp> {
    val w = a.width.toInt().takeIf { it > 0 } ?: 1
    val h = a.height.toInt().takeIf { it > 0 } ?: 1
    val maxW = 220f
    val maxH = 280f
    var dw = maxW
    var dh = maxW * h / w
    if (dh > maxH) { dh = maxH; dw = maxH * w / h }
    return dw.coerceAtLeast(90f).dp to dh.coerceAtLeast(90f).dp
}

@Composable
private fun ImageContent(m: MessageItem, a: AttachmentItem, index: Int, actions: BubbleActions, rounded: Boolean) {
    val (w, h) = displaySize(a)
    val local = rememberExistingPath(a.localPath)
    val thumb = remember(a.id, a.thumbnail) { a.thumbnail?.let { runCatching { BitmapFactory.decodeByteArray(it, 0, it.size)?.asImageBitmap() }.getOrNull() } }
    Box(
        Modifier.padding(if (rounded) 0.dp else 4.dp).size(w, h).clip(RoundedCornerShape(if (rounded) 18.dp else 14.dp))
            .background(Hush.colors.surface)
            .combinedClickable(onClick = { actions.onImageClick(m, index) }, onLongClick = { actions.onLongPress(m) })
            .testTag("image_${m.id}"),
        contentAlignment = Alignment.Center,
    ) {
        when {
            local != null -> AsyncImage(File(local), "图片", contentScale = ContentScale.Crop, modifier = Modifier.size(w, h))
            thumb != null -> {
                Image(thumb, "图片", contentScale = ContentScale.Crop, modifier = Modifier.size(w, h).blur(12.dp))
                CircularProgressIndicator(Modifier.size(28.dp), color = Color.White, strokeWidth = 2.dp)
            }
            else -> CircularProgressIndicator(Modifier.size(28.dp), color = Hush.colors.accent, strokeWidth = 2.dp)
        }
    }
}

@Composable
private fun VoiceContent(m: MessageItem, a: AttachmentItem, fg: Color) {
    val playing by VoicePlayer.state.collectAsState()
    val key = m.id + a.id
    val me = playing?.takeIf { it.key == key }
    val local = rememberExistingPath(a.localPath)
    val secs = (a.durationMs.toLong() + 500) / 1000
    Row(
        Modifier.padding(start = 8.dp, end = 12.dp, top = 8.dp, bottom = 2.dp).width((110 + (secs.coerceAtMost(60) * 2)).toInt().dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(
            Modifier.size(34.dp).clip(CircleShape).background(fg.copy(alpha = 0.18f))
                .clickable(enabled = local != null) { local?.let { VoicePlayer.toggle(key, it) } }
                .testTag("voice_${m.id}"),
            contentAlignment = Alignment.Center,
        ) {
            if (local == null) CircularProgressIndicator(Modifier.size(18.dp), color = fg, strokeWidth = 2.dp)
            else Icon(if (me != null) Icons.Filled.Pause else Icons.Filled.PlayArrow, if (me != null) "暂停" else "播放", tint = fg)
        }
        Spacer(Modifier.width(8.dp))
        LinearProgressIndicator(
            progress = { me?.progress ?: 0f },
            modifier = Modifier.weight(1f).height(3.dp).clip(RoundedCornerShape(2.dp)),
            color = fg, trackColor = fg.copy(alpha = 0.25f), drawStopIndicator = {},
        )
        Spacer(Modifier.width(8.dp))
        Text("${secs}″", style = HushType.secondary, color = fg)
    }
}

@Composable
private fun FileContent(m: MessageItem, a: AttachmentItem, index: Int, fg: Color, actions: BubbleActions) {
    Row(
        Modifier.padding(start = 10.dp, end = 12.dp, top = 10.dp, bottom = 2.dp).widthIn(min = 180.dp)
            .clickable { actions.onFileClick(m, index) }.testTag("file_${m.id}"),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(40.dp).clip(RoundedCornerShape(8.dp)).background(fg.copy(alpha = 0.15f)), contentAlignment = Alignment.Center) {
            Icon(Icons.AutoMirrored.Filled.InsertDriveFile, "文件", tint = fg)
        }
        Spacer(Modifier.width(10.dp))
        Column(Modifier.weight(1f, fill = false)) {
            Text(a.fileName, style = HushType.body.copy(fontSize = HushType.secondary.fontSize * 1.1f), color = fg, maxLines = 2, overflow = TextOverflow.Ellipsis)
            Text(MediaUtils.humanSize(a.size.toLong()), style = HushType.timestamp, color = fg.copy(alpha = 0.7f))
        }
    }
}

@Composable
private fun CallLogContent(m: MessageItem, b: MessageBody.CallLog, fg: Color) {
    // 只有未接来电用红色；其余原因（已拒绝、对方忙线、已取消、通话失败）用普通颜色
    val missed = b.reason == CallEndReason.MISSED
    Row(Modifier.padding(horizontal = 12.dp, vertical = 9.dp), verticalAlignment = Alignment.CenterVertically) {
        Icon(
            when { missed -> Icons.Filled.PhoneMissed; b.video -> Icons.Filled.Videocam; else -> Icons.Filled.Call },
            null, tint = if (missed) Hush.colors.danger else fg, modifier = Modifier.size(18.dp),
        )
        Spacer(Modifier.width(6.dp))
        Text(MessageText.callLog(b), style = HushType.body, color = if (missed) Hush.colors.danger else fg)
        Spacer(Modifier.width(8.dp))
        Text(TimeFormat.hhmm(m.sentAt), style = HushType.timestamp, color = fg.copy(alpha = 0.7f))
    }
}

@Composable
private fun Reactions(m: MessageItem, actions: BubbleActions) {
    Row(Modifier.padding(top = 2.dp, start = 6.dp, end = 6.dp), horizontalArrangement = Arrangement.spacedBy(4.dp)) {
        m.reactions.forEach { r ->
            val mine = r.userIds.contains(com.hush.app.core.HushCore.myId)
            Row(
                Modifier.heightIn(min = 22.dp).clip(RoundedCornerShape(11.dp))
                    .background(if (mine) Hush.colors.accent.copy(alpha = 0.18f) else Hush.colors.surface)
                    .clickable { actions.onReactionClick(m, r.emoji) }
                    .padding(horizontal = 7.dp, vertical = 1.dp)
                    .testTag("reaction_${r.emoji}"),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(r.emoji, style = HushType.secondary)
                if (r.userIds.size > 1) {
                    Spacer(Modifier.width(3.dp))
                    Text(r.userIds.size.toString(), style = HushType.timestamp, color = Hush.colors.textSecondary)
                }
            }
        }
    }
}

/** 本地附件路径（文件仍存在时）；只在路径变化时检查磁盘，而不是每次重组都 stat。 */
@Composable
private fun rememberExistingPath(path: String?): String? = remember(path) { path?.takeIf { File(it).exists() } }
