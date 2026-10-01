package com.hush.app.ui.moments

import androidx.compose.runtime.saveable.rememberSaveable
import com.hush.app.util.Savers
import com.hush.app.util.net

import android.net.Uri
import android.widget.Toast
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Visibility
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.RadioButton
import androidx.compose.material3.RadioButtonDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.material3.TextFieldDefaults
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.height
import coil3.compose.AsyncImage
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.FriendPickerList
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.SettingsGroup
import com.hush.app.ui.components.SettingsRow
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.DebugTextSink
import com.hush.app.util.MediaUtils
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.Visibility
import uniffi.hushcore.VisibilityMode

/** 发布动态：文字 + 最多 9 张图 + 谁可以看。 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun ComposeMomentScreen(onBack: () -> Unit, onPosted: () -> Unit) {
    val context = LocalContext.current
    var text by rememberSaveable { mutableStateOf("") }
    var images by rememberSaveable(stateSaver = Savers.UriList) { mutableStateOf(listOf<Uri>()) }
    var mode by rememberSaveable { mutableStateOf(VisibilityMode.ALL) }
    var picked by rememberSaveable(stateSaver = Savers.StringSet) { mutableStateOf(setOf<String>()) }
    var visDialog by rememberSaveable { mutableStateOf(false) }
    var pickFor by rememberSaveable { mutableStateOf<VisibilityMode?>(null) }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    DebugTextSink { text = it }
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.PickMultipleVisualMedia(9)) { uris ->
        images = (images + uris).distinct().take(9)
    }

    if (pickFor != null) {
        FriendSelect(pickFor!!, picked, onDone = { sel -> picked = sel; mode = if (sel.isEmpty()) VisibilityMode.ALL else pickFor!!; pickFor = null }, onBack = { pickFor = null })
        return
    }

    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("发表动态", onBack = onBack, actions = {
            Button(
                onClick = {
                    busy = true
                    scope.launch {
                        try {
                            withContext(Dispatchers.IO) {
                                val files = ArrayList<java.io.File>()
                                try {
                                    images.forEach { uri -> files += MediaUtils.prepareJpeg(context, uri, 2048, 80) }
                                    HushCore.client.postMoment(text.trim(), files.map { it.path }, Visibility(mode, if (mode == VisibilityMode.ALL) emptyList() else picked.toList()))
                                } finally {
                                    // 核心已把图片存入 files/media，临时副本随即删除
                                    MediaUtils.deleteTemp(*files.toTypedArray())
                                }
                            }
                            onPosted()
                        } catch (e: Exception) {
                            busy = false
                            Toast.makeText(context, "发布失败：${e.userMessage()}", Toast.LENGTH_SHORT).show()
                        }
                    }
                },
                enabled = !busy && (text.isNotBlank() || images.isNotEmpty()),
                colors = ButtonDefaults.buttonColors(containerColor = Hush.colors.accent),
                modifier = Modifier.padding(end = 8.dp).height(36.dp).testTag("post_moment"),
            ) { if (busy) CircularProgressIndicator(Modifier.size(18.dp), color = Color.White, strokeWidth = 2.dp) else Text("发布") }
        })
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
            TextField(
                value = text, onValueChange = { text = it },
                placeholder = { Text("这一刻的想法…") },
                colors = TextFieldDefaults.colors(
                    focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent,
                    focusedContainerColor = Hush.colors.background, unfocusedContainerColor = Hush.colors.background,
                ),
                modifier = Modifier.fillMaxWidth().heightIn(min = 120.dp).testTag("moment_text"),
            )
            FlowRow(Modifier.padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(6.dp), verticalArrangement = Arrangement.spacedBy(6.dp), maxItemsInEachRow = 3) {
                images.forEach { uri ->
                    Box(Modifier.size(100.dp).clip(RoundedCornerShape(6.dp))) {
                        AsyncImage(uri, null, contentScale = ContentScale.Crop, modifier = Modifier.fillMaxSize())
                        Box(
                            Modifier.align(Alignment.TopEnd).padding(4.dp).size(20.dp).clip(CircleShape).background(Color.Black.copy(alpha = 0.5f))
                                .clickable { images = images - uri },
                            contentAlignment = Alignment.Center,
                        ) { Icon(Icons.Filled.Close, "移除", tint = Color.White, modifier = Modifier.size(14.dp)) }
                    }
                }
                if (images.size < 9) {
                    Box(
                        Modifier.size(100.dp).clip(RoundedCornerShape(6.dp)).background(Hush.colors.surface)
                            .clickable { picker.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)) }
                            .testTag("moment_add_image"),
                        contentAlignment = Alignment.Center,
                    ) { Icon(Icons.Filled.Add, "添加图片", tint = Hush.colors.textSecondary, modifier = Modifier.size(36.dp)) }
                }
            }
            SettingsGroup(footer = "图片发布前会压缩到长边 ≤ 2048 像素") {
                SettingsRow("谁可以看", icon = Icons.Filled.Visibility, value = visibilityLabel(mode, picked.size), onClick = { visDialog = true })
            }
        }
    }
    if (visDialog) {
        AlertDialog(
            onDismissRequest = { visDialog = false },
            title = { Text("谁可以看") },
            text = {
                Column {
                    listOf(VisibilityMode.ALL to "公开给所有好友", VisibilityMode.INCLUDE to "部分可见（选择好友）", VisibilityMode.EXCLUDE to "不给谁看（选择好友）").forEach { (m, label) ->
                        Row(
                            Modifier.fillMaxWidth().selectable(m == mode) {
                                visDialog = false
                                if (m == VisibilityMode.ALL) { mode = m; picked = emptySet() } else { if (m != mode) picked = emptySet(); pickFor = m }
                            }.padding(vertical = 6.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            RadioButton(m == mode, onClick = null, colors = RadioButtonDefaults.colors(selectedColor = Hush.colors.accent))
                            Text(label, style = HushType.body, modifier = Modifier.padding(start = 8.dp))
                        }
                    }
                }
            },
            confirmButton = { TextButton(onClick = { visDialog = false }) { Text("取消") } },
        )
    }
}

private fun visibilityLabel(mode: VisibilityMode, n: Int) = when (mode) {
    VisibilityMode.ALL -> "公开"
    VisibilityMode.INCLUDE -> "部分可见（$n 人）"
    VisibilityMode.EXCLUDE -> "不给 $n 人看"
}

@Composable
private fun FriendSelect(mode: VisibilityMode, initial: Set<String>, onDone: (Set<String>) -> Unit, onBack: () -> Unit) {
    val friends by ContactsStore.friends.collectAsState()
    var sel by rememberSaveable(stateSaver = Savers.StringSet) { mutableStateOf(initial) }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar(if (mode == VisibilityMode.INCLUDE) "选择可见的好友" else "选择不给看的好友", onBack = onBack, actions = {
            TextButton(onClick = { onDone(sel) }) { Text("完成(${sel.size})") }
        })
        FriendPickerList(friends, sel, multi = true) { p -> sel = if (p.id in sel) sel - p.id else sel + p.id }
    }
}
