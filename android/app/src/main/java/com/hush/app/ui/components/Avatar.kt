package com.hush.app.ui.components

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Groups
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Shape
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import coil3.compose.AsyncImage
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import uniffi.hushcore.Profile
import com.hush.app.core.MediaCache
import com.hush.app.ui.theme.AvatarPalette
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import java.io.File

fun avatarColor(seed: String): Color = AvatarPalette[Math.floorMod(seed.hashCode(), AvatarPalette.size)]

/** 群聊发送者昵称颜色：按 user_id 取较深的色相，保证在浅/深色背景上可读。 */
private val NamePalette = listOf(
    Color(0xFF2C6BED), Color(0xFF1F8A5B), Color(0xFFC0662B), Color(0xFFB8457A),
    Color(0xFF7249C2), Color(0xFF1E8A80), Color(0xFFC24444), Color(0xFF3F7CB3),
)

fun nameColor(seed: String): Color = NamePalette[Math.floorMod(seed.hashCode(), NamePalette.size)]

fun initialOf(name: String): String {
    val t = name.trim()
    if (t.isEmpty()) return "?"
    val cp = t.codePointAt(0)
    return String(Character.toChars(cp)).uppercase()
}

/**
 * 通用头像：有图显示图片；否则按 seed 哈希取色 + 名字首字。
 */
@Composable
fun Avatar(seed: String, name: String, avatarId: String?, size: Dp, modifier: Modifier = Modifier, group: Boolean = false, shape: Shape = CircleShape) {
    val path = rememberMediaPath(avatarId)
    LaunchedEffect(avatarId) { avatarId?.let { MediaCache.requestAvatar(it) } }
    Box(
        modifier.size(size).clip(shape).background(avatarColor(seed)),
        contentAlignment = Alignment.Center,
    ) {
        if (path != null) {
            AsyncImage(model = File(path), contentDescription = name, contentScale = ContentScale.Crop, modifier = Modifier.size(size))
        } else if (group && name.isBlank()) {
            Icon(Icons.Filled.Groups, null, tint = Color.White, modifier = Modifier.size(size * 0.55f))
        } else {
            Text(initialOf(name), color = Color.White, fontWeight = FontWeight.SemiBold, fontSize = (size.value * 0.42f).sp)
        }
    }
}

/**
 * 订阅某个媒体（头像 / 朋友圈图片）的本地路径。只按该 id 派生，
 * 其他图片下载完成时不会让所有头像、图片重组。
 */
@Composable
fun rememberMediaPath(id: String?): String? {
    val flow = remember(id) { MediaCache.paths.map { m -> id?.let { m[it] } }.distinctUntilChanged() }
    return flow.collectAsState(initial = id?.let(MediaCache::pathOf)).value
}

/** 订阅某用户资料（本人取账号资料），缺失时自动拉取。只按该 user_id 派生，别人资料更新不触发重组。 */
@Composable
fun rememberProfile(userId: String): Profile? {
    val acct by HushCore.account.collectAsState()
    val flow = remember(userId) { ContactsStore.profiles.map { it[userId] }.distinctUntilChanged() }
    val profile by flow.collectAsState(initial = ContactsStore.cached(userId))
    LaunchedEffect(userId) { ContactsStore.ensure(userId) }
    return if (acct?.userId == userId) acct?.profile else profile
}

@Composable
fun rememberName(userId: String, fallback: String = ""): String = rememberProfile(userId)?.name ?: fallback

/** 按 user_id 显示头像（自动加载资料）。 */
@Composable
fun UserAvatar(userId: String, size: Dp, modifier: Modifier = Modifier, shape: Shape = CircleShape) {
    val p = rememberProfile(userId)
    Avatar(seed = userId, name = p?.name ?: "", avatarId = p?.avatarId, size = size, modifier = modifier, shape = shape)
}
