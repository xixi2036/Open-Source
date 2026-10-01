package com.hush.app.ui.chat

import androidx.compose.runtime.saveable.rememberSaveable
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Call
import androidx.compose.material.icons.filled.PushPin
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.NotificationsOff
import androidx.compose.material.icons.filled.Notifications
import androidx.compose.material.icons.filled.Remove
import androidx.compose.material.icons.filled.Security
import androidx.compose.material.icons.filled.Timer
import androidx.compose.material.icons.filled.Videocam
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Icon
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.RadioButtonDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.lifecycle.viewmodel.compose.viewModel
import com.hush.app.core.HushCore
import com.hush.app.ui.components.Avatar
import com.hush.app.ui.components.ConfirmDialog
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.RowDivider
import com.hush.app.ui.components.SettingsGroup
import com.hush.app.ui.components.SettingsRow
import com.hush.app.ui.components.SwitchRow
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.components.rememberProfile
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.TimeFormat
import uniffi.hushcore.GroupMember
import uniffi.hushcore.GroupRole

@Composable
fun ConversationSettingsScreen(
    convId: String,
    onBack: () -> Unit,
    onSafety: (String) -> Unit,
    onProfile: (String) -> Unit,
    onAddMembers: (String) -> Unit,
    onCall: (String, Boolean) -> Unit,
    onLeft: () -> Unit,
) {
    val context = LocalContext.current
    val app = context.applicationContext as android.app.Application
    val vm: ConversationSettingsViewModel = viewModel(key = "cs_$convId") { ConversationSettingsViewModel(app, convId) }
    val s by vm.state.collectAsState()
    val finished by vm.finished.collectAsState()
    LaunchedEffect(finished) { if (finished != null) onLeft() }
    com.hush.app.util.ToastCollector(vm.toast)

    var disappearingDialog by rememberSaveable { mutableStateOf(false) }
    var confirm by rememberSaveable { mutableStateOf<String?>(null) }

    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar(if (vm.groupId != null) "群聊信息" else "聊天信息", onBack = onBack)
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(bottom = 24.dp)) {
            if (vm.peerId != null) DirectHeader(vm.peerId, s.summary?.muted == true, onCall, onProfile, vm)
            else GroupHeader(s, vm, onAddMembers, onProfile)

            SettingsGroup {
                SettingsRow(
                    "阅后即焚", value = TimeFormat.disappearing(s.summary?.disappearingSecs?.toInt() ?: 0), icon = Icons.Filled.Timer,
                    onClick = { disappearingDialog = true },
                )
                RowDivider(52)
                SwitchRow("免打扰", s.summary?.muted == true, icon = Icons.Filled.NotificationsOff) { vm.setMuted(it) }
                RowDivider(52)
                SwitchRow("置顶聊天", s.summary?.pinned == true, icon = Icons.Filled.PushPin) { vm.setPinned(it) }
                if (vm.peerId != null) {
                    RowDivider(52)
                    SettingsRow("查看安全码", icon = Icons.Filled.Security, onClick = { onSafety(vm.peerId) })
                }
            }
            SettingsGroup {
                SettingsRow("清空聊天记录", titleColor = Hush.colors.danger, showChevron = false, onClick = { confirm = "clear" })
                if (vm.peerId != null) {
                    RowDivider()
                    SettingsRow(if (s.blocked) "取消拉黑" else "拉黑", titleColor = Hush.colors.danger, showChevron = false, onClick = { confirm = "block" })
                    RowDivider()
                    SettingsRow("删除好友", titleColor = Hush.colors.danger, showChevron = false, onClick = { confirm = "delete" })
                } else if (s.group?.active != false) {
                    RowDivider()
                    SettingsRow("退出群聊", titleColor = Hush.colors.danger, showChevron = false, onClick = { confirm = "leave" })
                }
            }
        }
    }

    if (disappearingDialog) {
        DisappearingDialog(s.summary?.disappearingSecs?.toInt() ?: 0, onPick = { vm.setDisappearing(it); disappearingDialog = false }, onDismiss = { disappearingDialog = false })
    }
    when (confirm) {
        "clear" -> ConfirmDialog("清空聊天记录", "仅清空本机记录，对方不受影响", "清空", true, { vm.clear() }, { confirm = null })
        "block" -> ConfirmDialog(if (s.blocked) "取消拉黑" else "拉黑", if (s.blocked) null else "拉黑后将不再收到对方的消息", "确定", !s.blocked, { vm.toggleBlock() }, { confirm = null })
        "delete" -> ConfirmDialog("删除好友", "将同时删除与对方的聊天会话", "删除", true, { vm.removeFriend() }, { confirm = null })
        "leave" -> ConfirmDialog("退出群聊", "退出后将不再接收此群消息", "退出", true, { vm.leave() }, { confirm = null })
    }
}

@Composable
private fun DirectHeader(peerId: String, muted: Boolean, onCall: (String, Boolean) -> Unit, onProfile: (String) -> Unit, vm: ConversationSettingsViewModel) {
    val p = rememberProfile(peerId)
    Column(Modifier.fillMaxWidth().padding(vertical = 16.dp), horizontalAlignment = Alignment.CenterHorizontally) {
        UserAvatar(peerId, 88.dp, Modifier.clickable { onProfile(peerId) })
        Spacer(Modifier.height(12.dp))
        Text(p?.name ?: "", style = HushType.title, color = Hush.colors.textPrimary)
        // 对方邮箱（接口 v2 / C-C：互为好友时核心才返回）
        p?.email?.takeIf { it.isNotBlank() }?.let { email ->
            Spacer(Modifier.height(2.dp))
            Text(email, style = HushType.secondary, color = Hush.colors.textSecondary, modifier = Modifier.testTag("peer_email"))
        }
        if (!p?.about.isNullOrBlank()) {
            Spacer(Modifier.height(4.dp))
            Text(p!!.about, style = HushType.secondary, color = Hush.colors.textSecondary, textAlign = TextAlign.Center, modifier = Modifier.padding(horizontal = 32.dp))
        }
        Spacer(Modifier.height(16.dp))
        Row(horizontalArrangement = Arrangement.spacedBy(16.dp)) {
            CircleAction(Icons.Filled.Call, "语音") { onCall(peerId, false) }
            CircleAction(Icons.Filled.Videocam, "视频") { onCall(peerId, true) }
            CircleAction(if (muted) Icons.Filled.NotificationsOff else Icons.Filled.Notifications, if (muted) "已静音" else "静音") { vm.setMuted(!muted) }
        }
    }
}

@Composable
fun CircleAction(icon: ImageVector, label: String, onClick: () -> Unit) {
    Column(horizontalAlignment = Alignment.CenterHorizontally, modifier = Modifier.clip(RoundedCornerShape(12.dp)).clickable(onClick = onClick).padding(8.dp).testTag("action_$label")) {
        Box(Modifier.size(52.dp).clip(RoundedCornerShape(14.dp)).background(Hush.colors.surface), contentAlignment = Alignment.Center) {
            Icon(icon, label, tint = Hush.colors.accent)
        }
        Spacer(Modifier.height(4.dp))
        Text(label, style = HushType.secondary, color = Hush.colors.textPrimary)
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun GroupHeader(s: ConvSettingsState, vm: ConversationSettingsViewModel, onAddMembers: (String) -> Unit, onProfile: (String) -> Unit) {
    val g = s.group
    var edit by rememberSaveable { mutableStateOf<String?>(null) }
    // A4：只保存成员 id（可跨重建恢复），成员对象每次从最新的群信息里取
    var memberMenuId by rememberSaveable { mutableStateOf<String?>(null) }
    var removeMode by rememberSaveable { mutableStateOf(false) }
    var confirmRemoveId by rememberSaveable { mutableStateOf<String?>(null) }
    val memberMenu = memberMenuId?.let { id -> g?.members?.firstOrNull { it.userId == id } }
    val confirmRemove = confirmRemoveId?.let { id -> g?.members?.firstOrNull { it.userId == id } }
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri -> uri?.let(vm::setGroupAvatar) }
    Column(Modifier.fillMaxWidth().padding(vertical = 16.dp), horizontalAlignment = Alignment.CenterHorizontally) {
        Avatar(
            vm.groupId ?: "", g?.name ?: "", g?.avatarId, 88.dp, group = true,
            modifier = Modifier.clickable(enabled = s.isAdmin) { picker.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)) },
        )
        Spacer(Modifier.height(12.dp))
        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.clickable(enabled = s.isAdmin) { edit = "name" }) {
            Text(g?.name ?: "", style = HushType.title, color = Hush.colors.textPrimary)
            if (s.isAdmin) { Spacer(Modifier.width(4.dp)); Icon(Icons.Filled.Edit, "编辑群名", tint = Hush.colors.textSecondary, modifier = Modifier.size(16.dp)) }
        }
        Text("${g?.members?.size ?: 0} 位成员", style = HushType.secondary, color = Hush.colors.textSecondary)
    }
    SettingsGroup(title = "群公告") {
        SettingsRow(
            g?.announcement?.takeIf { it.isNotBlank() } ?: "暂无公告", showChevron = s.isAdmin,
            onClick = if (s.isAdmin) ({ edit = "announcement" }) else null,
        )
    }
    SettingsGroup(title = "群成员") {
        FlowRow(Modifier.fillMaxWidth().padding(12.dp), horizontalArrangement = Arrangement.Start, maxItemsInEachRow = 5) {
            g?.members?.sortedBy { it.role.ordinal }?.forEach { m ->
                val removable = removeMode && s.isAdmin && m.userId != HushCore.myId && m.role != GroupRole.OWNER
                MemberCell(m, removable, onClick = {
                    // 移除模式下只有带减号的成员可点，且需二次确认；群主与自己不可移除
                    if (removeMode) { if (removable) confirmRemoveId = m.userId } else memberMenuId = m.userId
                })
            }
            // 服务端只允许群主/管理员添加成员（普通成员调用返回 403），普通成员不显示“添加”
            if (s.group?.active != false && s.isAdmin) {
                GridButton(Icons.Filled.Add, "添加") { vm.groupId?.let(onAddMembers) }
                if (s.isAdmin && (g?.members?.size ?: 0) > 1) GridButton(if (removeMode) Icons.Filled.Check else Icons.Filled.Remove, if (removeMode) "完成" else "移除") { removeMode = !removeMode }
            }
        }
    }
    memberMenu?.let { m ->
        val isMe = m.userId == HushCore.myId
        AlertDialog(
            onDismissRequest = { memberMenuId = null },
            title = { Text(com.hush.app.core.ContactsStore.nameOf(m.userId)) },
            text = {
                Column {
                    Text(when (m.role) { GroupRole.OWNER -> "群主"; GroupRole.ADMIN -> "管理员"; GroupRole.MEMBER -> "成员" }, color = Hush.colors.textSecondary)
                    Spacer(Modifier.height(12.dp))
                    DialogOption("查看资料") { memberMenuId = null; onProfile(m.userId) }
                    if (s.isOwner && !isMe && m.role != GroupRole.OWNER) {
                        DialogOption(if (m.role == GroupRole.ADMIN) "取消管理员" else "设为管理员") { vm.setAdmin(m.userId, m.role != GroupRole.ADMIN); memberMenuId = null }
                    }
                    if (s.isAdmin && !isMe && m.role != GroupRole.OWNER) {
                        DialogOption("移出群聊", danger = true) { confirmRemoveId = m.userId; memberMenuId = null }
                    }
                }
            },
            confirmButton = { TextButton(onClick = { memberMenuId = null }) { Text("关闭") } },
        )
    }
    confirmRemove?.let { m ->
        ConfirmDialog(
            "移出群聊", "确定将 ${com.hush.app.core.ContactsStore.nameOf(m.userId)} 移出群聊？", "移出", danger = true,
            onConfirm = { vm.removeMember(m.userId) }, onDismiss = { confirmRemoveId = null },
        )
    }
    edit?.let { field ->
        var text by rememberSaveable(field) { mutableStateOf(if (field == "name") g?.name ?: "" else g?.announcement ?: "") }
        com.hush.app.util.DebugTextSink { text = it }
        AlertDialog(
            onDismissRequest = { edit = null },
            title = { Text(if (field == "name") "修改群名称" else "编辑群公告") },
            text = { OutlinedTextField(text, { text = it }, singleLine = field == "name", modifier = Modifier.fillMaxWidth().testTag("edit_field")) },
            confirmButton = {
                TextButton(onClick = {
                    if (field == "name") { if (text.isNotBlank()) vm.rename(text) } else vm.setAnnouncement(text)
                    edit = null
                }) { Text("保存") }
            },
            dismissButton = { TextButton(onClick = { edit = null }) { Text("取消") } },
        )
    }
}

@Composable
private fun DialogOption(text: String, danger: Boolean = false, onClick: () -> Unit) {
    Text(
        text, style = HushType.body, color = if (danger) Hush.colors.danger else Hush.colors.accent,
        modifier = Modifier.fillMaxWidth().clickable(onClick = onClick).padding(vertical = 12.dp),
    )
}

@Composable
private fun MemberCell(m: GroupMember, removable: Boolean, onClick: () -> Unit) {
    val p = rememberProfile(m.userId)
    Column(
        Modifier.width(64.dp).clickable(onClick = onClick).padding(vertical = 6.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Box {
            UserAvatar(m.userId, 48.dp)
            if (removable) {
                Box(Modifier.size(18.dp).clip(CircleShape).background(Hush.colors.danger).align(Alignment.TopEnd), contentAlignment = Alignment.Center) {
                    Icon(Icons.Filled.Remove, "移除", tint = androidx.compose.ui.graphics.Color.White, modifier = Modifier.size(14.dp))
                }
            }
        }
        Spacer(Modifier.height(4.dp))
        // 名字单独一行（过长省略），角色另起一行，不再因拼接“·群主”而把名字截掉
        Text(
            p?.name ?: "",
            style = HushType.timestamp.copy(fontSize = HushType.secondary.fontSize * 0.92f), color = Hush.colors.textSecondary,
            maxLines = 1, overflow = TextOverflow.Ellipsis,
        )
        val role = when (m.role) { GroupRole.OWNER -> "群主"; GroupRole.ADMIN -> "管理员"; else -> null }
        if (role != null) Text(role, style = HushType.timestamp.copy(fontSize = 10.sp), color = Hush.colors.accent, maxLines = 1)
    }
}

@Composable
private fun GridButton(icon: ImageVector, label: String, onClick: () -> Unit) {
    Column(Modifier.width(64.dp).clickable(onClick = onClick).padding(vertical = 6.dp).testTag("member_$label"), horizontalAlignment = Alignment.CenterHorizontally) {
        Box(
            Modifier.size(48.dp).clip(RoundedCornerShape(12.dp)).border(1.dp, Hush.colors.divider, RoundedCornerShape(12.dp)),
            contentAlignment = Alignment.Center,
        ) { Icon(icon, label, tint = Hush.colors.textSecondary) }
        Spacer(Modifier.height(4.dp))
        Text(label, style = HushType.timestamp.copy(fontSize = HushType.secondary.fontSize * 0.92f), color = Hush.colors.textSecondary)
    }
}

@Composable
fun DisappearingDialog(current: Int, onPick: (Int) -> Unit, onDismiss: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("阅后即焚") },
        text = {
            Column {
                TimeFormat.disappearingOptions.forEach { secs ->
                    Row(
                        Modifier.fillMaxWidth().selectable(selected = secs == current, onClick = { onPick(secs) }).padding(vertical = 6.dp).testTag("dis_$secs"),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        RadioButton(selected = secs == current, onClick = { onPick(secs) }, colors = RadioButtonDefaults.colors(selectedColor = Hush.colors.accent))
                        Text(TimeFormat.disappearing(secs), style = HushType.body)
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("取消") } },
    )
}
