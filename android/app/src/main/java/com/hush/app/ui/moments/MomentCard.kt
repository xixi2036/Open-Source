package com.hush.app.ui.moments

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ChatBubbleOutline
import androidx.compose.material.icons.filled.Favorite
import androidx.compose.material.icons.filled.FavoriteBorder
import androidx.compose.material.icons.filled.Lock
import androidx.compose.material.icons.filled.MoreHoriz
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
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
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import coil3.compose.AsyncImage
import com.hush.app.core.HushCore
import com.hush.app.core.MediaCache
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.ConfirmDialog
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.TimeFormat
import uniffi.hushcore.Comment
import uniffi.hushcore.Moment
import uniffi.hushcore.MomentMedia
import uniffi.hushcore.VisibilityMode
import java.io.File

class MomentActions(
    val onLike: (Moment) -> Unit,
    val onComment: (Moment, Comment?) -> Unit,
    val onDeleteComment: (Moment, Comment) -> Unit,
    val onDelete: (Moment) -> Unit,
    val onImage: (String) -> Unit,
    val onProfile: (String) -> Unit,
)

@OptIn(ExperimentalFoundationApi::class)
@Composable
fun MomentCard(m: Moment, actions: MomentActions) {
    var menu by remember { mutableStateOf(false) }
    var confirmDelete by remember { mutableStateOf(false) }
    val mine = m.author.id == HushCore.myId
    val liked = m.likes.any { it.id == HushCore.myId }
    Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 12.dp).testTag("moment_${m.id}")) {
        Avatar(m.author.id, m.author.name, m.author.avatarId, 44.dp, modifier = Modifier.clickable { actions.onProfile(m.author.id) })
        Spacer(Modifier.width(12.dp))
        Column(Modifier.weight(1f)) {
            Text(m.author.name, style = HushType.bodyMedium, color = Hush.colors.accent, modifier = Modifier.clickable { actions.onProfile(m.author.id) })
            if (m.text.isNotBlank()) {
                Spacer(Modifier.height(4.dp))
                Text(m.text, style = HushType.body, color = Hush.colors.textPrimary)
            }
            if (m.media.isNotEmpty()) {
                Spacer(Modifier.height(8.dp))
                MediaGrid(m.media, actions.onImage)
            }
            Spacer(Modifier.height(8.dp))
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(TimeFormat.relative(m.createdAt), style = HushType.secondary, color = Hush.colors.textSecondary)
                if (mine && m.visibility != null && m.visibility!!.mode != VisibilityMode.ALL) {
                    Spacer(Modifier.width(6.dp))
                    Icon(Icons.Filled.Lock, "部分可见", tint = Hush.colors.textSecondary, modifier = Modifier.size(12.dp))
                }
                if (mine) {
                    Spacer(Modifier.width(12.dp))
                    Text("删除", style = HushType.secondary, color = Hush.colors.accent, modifier = Modifier.clickable { confirmDelete = true }.testTag("moment_delete"))
                }
                Spacer(Modifier.weight(1f))
                Box {
                    Box(
                        Modifier.clip(RoundedCornerShape(4.dp)).background(Hush.colors.surface).clickable { menu = true }
                            .padding(horizontal = 8.dp, vertical = 2.dp).testTag("moment_more"),
                    ) { Icon(Icons.Filled.MoreHoriz, "赞/评论", tint = Hush.colors.accent, modifier = Modifier.size(18.dp)) }
                    DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                        DropdownMenuItem(
                            text = { Text(if (liked) "取消" else "赞") }, modifier = Modifier.testTag("moment_like"),
                            leadingIcon = { Icon(if (liked) Icons.Filled.FavoriteBorder else Icons.Filled.Favorite, null, tint = Hush.colors.danger) },
                            onClick = { menu = false; actions.onLike(m) },
                        )
                        DropdownMenuItem(
                            text = { Text("评论") }, modifier = Modifier.testTag("moment_comment"),
                            leadingIcon = { Icon(Icons.Filled.ChatBubbleOutline, null) },
                            onClick = { menu = false; actions.onComment(m, null) },
                        )
                    }
                }
            }
            if (m.likes.isNotEmpty() || m.comments.isNotEmpty()) {
                Spacer(Modifier.height(6.dp))
                Column(Modifier.fillMaxWidth().clip(RoundedCornerShape(6.dp)).background(Hush.colors.surface).padding(horizontal = 10.dp, vertical = 6.dp)) {
                    if (m.likes.isNotEmpty()) {
                        Row(verticalAlignment = Alignment.Top) {
                            Icon(Icons.Filled.Favorite, null, tint = Hush.colors.danger, modifier = Modifier.padding(top = 3.dp).size(14.dp))
                            Spacer(Modifier.width(6.dp))
                            Text(m.likes.joinToString("，") { it.name }, style = HushType.secondary.copy(fontWeight = FontWeight.Medium), color = Hush.colors.accent)
                        }
                    }
                    if (m.likes.isNotEmpty() && m.comments.isNotEmpty()) {
                        Spacer(Modifier.height(4.dp))
                    }
                    m.comments.forEach { c -> CommentLine(m, c, actions) }
                }
            }
        }
    }
    if (confirmDelete) {
        ConfirmDialog("删除动态", "删除后无法恢复", "删除", true, { actions.onDelete(m) }, { confirmDelete = false })
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun CommentLine(m: Moment, c: Comment, actions: MomentActions) {
    var confirm by remember { mutableStateOf(false) }
    val canDelete = c.author.id == HushCore.myId || m.author.id == HushCore.myId
    val accent = Hush.colors.accent
    val text = buildAnnotatedString {
        withStyle(SpanStyle(color = accent, fontWeight = FontWeight.Medium)) { append(c.author.name) }
        c.replyTo?.let {
            append(" 回复 ")
            withStyle(SpanStyle(color = accent, fontWeight = FontWeight.Medium)) { append(it.name) }
        }
        append("：")
        append(c.text)
    }
    Text(
        text, style = HushType.secondary.copy(fontSize = HushType.secondary.fontSize * 1.08f), color = Hush.colors.textPrimary,
        modifier = Modifier.fillMaxWidth()
            .combinedClickable(
                onClick = { if (c.author.id != HushCore.myId) actions.onComment(m, c) },
                onLongClick = { if (canDelete) confirm = true },
            )
            .padding(vertical = 2.dp).testTag("comment_${c.id}"),
    )
    if (confirm) ConfirmDialog("删除评论", null, "删除", true, { actions.onDeleteComment(m, c) }, { confirm = false })
}

/** 1 张大图保持比例；2–9 张 3 列方格。 */
@Composable
private fun MediaGrid(media: List<MomentMedia>, onImage: (String) -> Unit) {
    if (media.size == 1) {
        val one = media[0]
        val ratio = if (one.width > 0u && one.height > 0u) one.width.toFloat() / one.height.toFloat() else 1f
        val w = if (ratio >= 1f) 220.dp else (220f * ratio.coerceAtLeast(0.5f)).dp
        MomentImage(one, Modifier.widthIn(max = 220.dp).width(w).aspectRatio(ratio.coerceIn(0.5f, 2f)), onImage)
        return
    }
    Column(verticalArrangement = Arrangement.spacedBy(4.dp), modifier = Modifier.widthIn(max = 260.dp)) {
        media.chunked(3).forEach { row ->
            Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                row.forEach { MomentImage(it, Modifier.size(82.dp), onImage) }
            }
        }
    }
}

@Composable
private fun MomentImage(media: MomentMedia, modifier: Modifier, onImage: (String) -> Unit) {
    val path = com.hush.app.ui.components.rememberMediaPath(media.attachmentId)
    LaunchedEffect(media.attachmentId) { MediaCache.requestMedia(media.attachmentId) }
    Box(modifier.clip(RoundedCornerShape(4.dp)).background(Hush.colors.surface).clickable(enabled = path != null) { path?.let(onImage) }) {
        if (path != null) AsyncImage(File(path), "图片", contentScale = ContentScale.Crop, modifier = Modifier.matchParentSize())
    }
}
