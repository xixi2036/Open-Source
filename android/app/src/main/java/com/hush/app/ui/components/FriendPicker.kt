package com.hush.app.ui.components

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CheckboxDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import uniffi.hushcore.Profile

/** 好友选择列表：multi=false 时点击即回调；multi=true 时显示复选框。 */
@Composable
fun FriendPickerList(
    friends: List<Profile>,
    selected: Set<String>,
    multi: Boolean,
    disabled: Set<String> = emptySet(),
    modifier: Modifier = Modifier,
    onToggle: (Profile) -> Unit,
) {
    LazyColumn(modifier) {
        items(friends, key = { it.id }) { p ->
            val off = p.id in disabled
            Row(
                Modifier.fillMaxWidth().clickable(enabled = !off) { onToggle(p) }.padding(horizontal = 16.dp, vertical = 10.dp).testTag("pick_${p.name}"),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                if (multi) {
                    Checkbox(
                        // onCheckedChange 始终非空：为 null 时 Checkbox 不占 48dp 触摸区，已在群内的行会整体左移错位
                        checked = p.id in selected || off, onCheckedChange = { if (!off) onToggle(p) }, enabled = !off,
                        colors = CheckboxDefaults.colors(checkedColor = Hush.colors.accent),
                    )
                    Spacer(Modifier.width(4.dp))
                }
                Avatar(p.id, p.name, p.avatarId, 44.dp)
                Spacer(Modifier.width(12.dp))
                Text(p.name, style = HushType.body, color = if (off) Hush.colors.textSecondary else Hush.colors.textPrimary)
            }
            RowDivider(72)
        }
    }
}
