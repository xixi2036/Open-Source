package com.hush.app.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Typography
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.sp

/** ui-guide 第 1 节设计令牌。 */
@Immutable
data class HushColors(
    val accent: Color,
    val onAccent: Color,
    val background: Color,
    val surface: Color,
    val incomingBubble: Color,
    val textPrimary: Color,
    val textSecondary: Color,
    val divider: Color,
    val danger: Color,
    val success: Color,
    /** 通话界面背景渐变（上 → 下），通话按钮高亮时的图标色取上端色。 */
    val callBackgroundTop: Color,
    val callBackgroundBottom: Color,
    /** “新的朋友”入口图标底色。 */
    val newFriends: Color,
    /** 朋友圈封面上的消息提醒条底色。 */
    val momentsNoticeBar: Color,
    /** 输入框描边、未选中控件边框（比分割线更醒目，深色模式下仍可辨认）。 */
    val outline: Color,
)

val LightHush = HushColors(
    accent = Color(0xFF2C6BED), onAccent = Color.White, background = Color.White, surface = Color(0xFFF6F6F6),
    incomingBubble = Color(0xFFE9E9EB), textPrimary = Color(0xFF1B1B1D), textSecondary = Color(0xFF6B6B70),
    divider = Color(0xFFE3E3E3), danger = Color(0xFFE0413B), success = Color(0xFF1FA95F),
    callBackgroundTop = Color(0xFF1B2A4A), callBackgroundBottom = Color(0xFF0E1526),
    newFriends = Color(0xFFF5A623), momentsNoticeBar = Color(0xFF4A4A4F), outline = Color(0xFFB8B8BE),
)

val DarkHush = HushColors(
    accent = Color(0xFF5B8DF5), onAccent = Color.White, background = Color(0xFF121212), surface = Color(0xFF1E1E1E),
    incomingBubble = Color(0xFF2C2C2E), textPrimary = Color(0xFFE9E9E9), textSecondary = Color(0xFFA0A0A5),
    divider = Color(0xFF2C2C2E), danger = Color(0xFFFF6961), success = Color(0xFF34C77B),
    callBackgroundTop = Color(0xFF1B2A4A), callBackgroundBottom = Color(0xFF0E1526),
    newFriends = Color(0xFFF5A623), momentsNoticeBar = Color(0xFF4A4A4F), outline = Color(0xFF5C5C62),
)

val LocalHushColors = staticCompositionLocalOf { LightHush }

object Hush {
    val colors: HushColors @Composable get() = LocalHushColors.current
}

/** 字号：标题 20 semibold，正文 16，次要 13，时间戳 11。 */
object HushType {
    val title = TextStyle(fontSize = 20.sp, fontWeight = FontWeight.SemiBold)
    val largeTitle = TextStyle(fontSize = 28.sp, fontWeight = FontWeight.Bold)
    val body = TextStyle(fontSize = 16.sp)
    val bodyMedium = TextStyle(fontSize = 16.sp, fontWeight = FontWeight.Medium)
    val secondary = TextStyle(fontSize = 13.sp)
    val timestamp = TextStyle(fontSize = 11.sp)
}

/** 八种柔和头像底色。 */
val AvatarPalette = listOf(
    Color(0xFF7FA7F5), Color(0xFF8FD3B6), Color(0xFFF5B97F), Color(0xFFE59AB5),
    Color(0xFFB39DDB), Color(0xFF80CBC4), Color(0xFFF2A0A0), Color(0xFF9FC5E8),
)

@Composable
fun HushTheme(dark: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    val c = if (dark) DarkHush else LightHush
    val scheme = if (dark) {
        darkColorScheme(
            primary = c.accent, onPrimary = c.onAccent, background = c.background, onBackground = c.textPrimary,
            surface = c.background, onSurface = c.textPrimary, surfaceVariant = c.surface, onSurfaceVariant = c.textSecondary,
            surfaceContainer = c.surface, surfaceContainerHigh = c.surface, surfaceContainerHighest = c.incomingBubble,
            surfaceContainerLow = c.background, surfaceContainerLowest = c.background,
            outline = c.outline, outlineVariant = c.divider, error = c.danger, secondaryContainer = c.accent.copy(alpha = 0.25f),
            onSecondaryContainer = c.textPrimary, primaryContainer = c.accent.copy(alpha = 0.25f),
        )
    } else {
        lightColorScheme(
            primary = c.accent, onPrimary = c.onAccent, background = c.background, onBackground = c.textPrimary,
            surface = c.background, onSurface = c.textPrimary, surfaceVariant = c.surface, onSurfaceVariant = c.textSecondary,
            surfaceContainer = c.surface, surfaceContainerHigh = c.surface, surfaceContainerHighest = c.incomingBubble,
            surfaceContainerLow = c.background, surfaceContainerLowest = c.background,
            outline = c.outline, outlineVariant = c.divider, error = c.danger, secondaryContainer = c.accent.copy(alpha = 0.14f),
            onSecondaryContainer = c.textPrimary, primaryContainer = c.accent.copy(alpha = 0.14f),
        )
    }
    val typography = Typography(
        titleLarge = HushType.title, bodyLarge = HushType.body, bodyMedium = HushType.body.copy(fontSize = 15.sp),
        bodySmall = HushType.secondary, labelSmall = HushType.timestamp,
    )
    CompositionLocalProvider(LocalHushColors provides c) {
        MaterialTheme(colorScheme = scheme, typography = typography) {
            CompositionLocalProvider(androidx.compose.material3.LocalContentColor provides c.textPrimary, content = content)
        }
    }
}
