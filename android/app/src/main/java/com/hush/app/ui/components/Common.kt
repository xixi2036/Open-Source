package com.hush.app.ui.components

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.automirrored.filled.KeyboardArrowRight
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CenterAlignedTopAppBar
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HushTopBar(
    title: String,
    onBack: (() -> Unit)? = null,
    actions: @Composable RowScope.() -> Unit = {},
    subtitle: String? = null,
) {
    CenterAlignedTopAppBar(
        title = {
            Column(horizontalAlignment = Alignment.CenterHorizontally) {
                Text(title, style = HushType.title.copy(fontSize = HushType.title.fontSize * 0.9f), maxLines = 1, overflow = TextOverflow.Ellipsis)
                if (subtitle != null) Text(subtitle, style = HushType.secondary, color = Hush.colors.textSecondary)
            }
        },
        navigationIcon = {
            if (onBack != null) IconButton(onClick = onBack) { Icon(Icons.AutoMirrored.Filled.ArrowBack, "返回") }
        },
        actions = actions,
        colors = TopAppBarDefaults.centerAlignedTopAppBarColors(containerColor = Hush.colors.background),
    )
}

/** 大标题页头（Tab 页顶部）。 */
@Composable
fun LargeHeader(title: String, actions: @Composable RowScope.() -> Unit = {}) {
    Row(
        Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 12.dp, bottom = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(title, style = HushType.largeTitle, color = Hush.colors.textPrimary, modifier = Modifier.weight(1f))
        actions()
    }
}

/** 设置分组：圆角卡片 + 组标题。 */
@Composable
fun SettingsGroup(title: String? = null, footer: String? = null, content: @Composable ColumnScope.() -> Unit) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp)) {
        if (title != null) {
            Text(title, style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.padding(start = 4.dp, bottom = 6.dp))
        }
        Column(Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp)).background(Hush.colors.surface), content = content)
        if (footer != null) {
            Text(footer, style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.padding(start = 4.dp, top = 6.dp, end = 4.dp))
        }
    }
}

@Composable
fun SettingsRow(
    title: String,
    value: String? = null,
    modifier: Modifier = Modifier,
    icon: ImageVector? = null,
    titleColor: Color = Color.Unspecified,
    showChevron: Boolean = true,
    subtitle: String? = null,
    onClick: (() -> Unit)? = null,
    trailing: (@Composable () -> Unit)? = null,
) {
    Row(
        modifier.fillMaxWidth().heightIn(min = 52.dp)
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier)
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (icon != null) {
            Icon(icon, null, tint = if (titleColor != Color.Unspecified) titleColor else Hush.colors.accent, modifier = Modifier.size(22.dp))
            Spacer(Modifier.width(14.dp))
        }
        Column(Modifier.weight(1f)) {
            Text(title, style = HushType.body, color = if (titleColor != Color.Unspecified) titleColor else Hush.colors.textPrimary)
            if (subtitle != null) Text(subtitle, style = HushType.secondary, color = Hush.colors.textSecondary)
        }
        if (value != null) {
            Text(value, style = HushType.body.copy(fontSize = HushType.secondary.fontSize * 1.1f), color = Hush.colors.textSecondary, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.padding(start = 8.dp))
        }
        if (trailing != null) trailing()
        else if (showChevron && onClick != null) Icon(Icons.AutoMirrored.Filled.KeyboardArrowRight, null, tint = Hush.colors.textSecondary)
    }
}

@Composable
fun SwitchRow(title: String, checked: Boolean, subtitle: String? = null, icon: ImageVector? = null, enabled: Boolean = true, onChange: (Boolean) -> Unit) {
    SettingsRow(title = title, subtitle = subtitle, icon = icon, onClick = if (enabled) ({ onChange(!checked) }) else null, trailing = {
        Switch(
            checked = checked, onCheckedChange = if (enabled) onChange else null, enabled = enabled,
            // 关闭态显式配色：默认的 outline（= 分割线色）在浅色/深色下都几乎看不见滑块
            colors = SwitchDefaults.colors(
                checkedTrackColor = Hush.colors.accent,
                uncheckedThumbColor = Hush.colors.textSecondary,
                uncheckedBorderColor = Hush.colors.textSecondary,
                uncheckedTrackColor = Hush.colors.background,
            ),
        )
    })
}

@Composable
fun RowDivider(startPadding: Int = 16) {
    HorizontalDivider(Modifier.padding(start = startPadding.dp), color = Hush.colors.divider, thickness = 0.5.dp)
}

@Composable
fun ConfirmDialog(
    title: String,
    text: String? = null,
    confirm: String = "确定",
    danger: Boolean = false,
    onConfirm: () -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = text?.let { { Text(it) } },
        confirmButton = {
            TextButton(onClick = { onConfirm(); onDismiss() }) { Text(confirm, color = if (danger) Hush.colors.danger else Hush.colors.accent) }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("取消") } },
    )
}

@Composable
fun EmptyState(icon: ImageVector, text: String, modifier: Modifier = Modifier) {
    Column(modifier.fillMaxSize().padding(32.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.Center) {
        Box(Modifier.size(96.dp).clip(CircleShape).background(Hush.colors.surface), contentAlignment = Alignment.Center) {
            Icon(icon, null, tint = Hush.colors.accent.copy(alpha = 0.7f), modifier = Modifier.size(48.dp))
        }
        Spacer(Modifier.height(16.dp))
        Text(text, style = HushType.body, color = Hush.colors.textSecondary, textAlign = TextAlign.Center)
    }
}

/** 数量徽章：圆角底色 + 白字，超过 99 显示“99+”。 */
@Composable
fun CountBadge(count: Int, color: Color) {
    if (count <= 0) return
    Box(
        Modifier.heightIn(min = 20.dp).clip(RoundedCornerShape(10.dp)).background(color).padding(horizontal = 6.dp, vertical = 1.dp),
        contentAlignment = Alignment.Center,
    ) { Text(if (count > 99) "99+" else count.toString(), color = Color.White, style = HushType.timestamp.copy(fontSize = HushType.timestamp.fontSize * 1.1f)) }
}

/** 未读徽章：accent 色数量；muted 时灰色小点。 */
@Composable
fun UnreadBadge(count: Int, muted: Boolean = false) {
    if (count <= 0) return
    if (muted) Box(Modifier.size(10.dp).clip(CircleShape).background(Hush.colors.textSecondary.copy(alpha = 0.6f)))
    else CountBadge(count, Hush.colors.accent)
}

/**
 * 深色背景上的页面（朋友圈封面、图片查看、通话）：在 [enabled] 期间把状态栏图标切为浅色，
 * 离开组合或条件不再满足时恢复原值。
 */
@Composable
fun LightStatusBarIcons(enabled: Boolean = true) {
    val view = androidx.compose.ui.platform.LocalView.current
    androidx.compose.runtime.DisposableEffect(enabled) {
        if (!enabled) return@DisposableEffect onDispose {}
        val window = (view.context as? android.app.Activity)?.window
        val ctl = window?.let { androidx.core.view.WindowCompat.getInsetsController(it, view) }
        val prev = ctl?.isAppearanceLightStatusBars
        ctl?.isAppearanceLightStatusBars = false
        onDispose { if (prev != null) ctl.isAppearanceLightStatusBars = prev }
    }
}
