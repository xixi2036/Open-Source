package com.hush.app.ui.me

import com.hush.app.util.net

import android.widget.Toast
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.PhotoCamera
import androidx.compose.material3.Icon
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.OutlinedTextFieldDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
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
import coil3.compose.AsyncImage
import com.hush.app.core.ContactsStore
import com.hush.app.core.HushCore
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.components.UserAvatar
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.MediaUtils
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File

/** 编辑资料：头像 / 昵称 / 签名。 */
@Composable
fun EditProfileScreen(onBack: () -> Unit) {
    val context = LocalContext.current
    val acct by HushCore.account.collectAsState()
    var name by remember { mutableStateOf(acct?.profile?.name ?: "") }
    var about by remember { mutableStateOf(acct?.profile?.about ?: "") }
    var avatar by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri ->
        uri ?: return@rememberLauncherForActivityResult
        scope.launch {
            val old = avatar
            avatar = withContext(Dispatchers.IO) { runCatching { MediaUtils.prepareAvatar(context, uri).path }.getOrNull() } ?: old
            if (old != null && old != avatar) MediaUtils.deleteTemp(File(old))
        }
    }
    Column(Modifier.fillMaxSize().background(Hush.colors.background)) {
        HushTopBar("编辑资料", onBack = onBack, actions = {
            TextButton(enabled = name.isNotBlank() && !busy, modifier = Modifier.testTag("save_profile"), onClick = {
                busy = true
                scope.launch {
                    try {
                        val p = withContext(Dispatchers.IO) { net { HushCore.client.updateProfile(name.trim(), about.trim(), avatar) } }
                        avatar?.let { MediaUtils.deleteTemp(File(it)) }
                        ContactsStore.put(p)
                        HushCore.refreshAccount()
                        onBack()
                    } catch (e: Exception) {
                        busy = false
                        Toast.makeText(context, e.userMessage(), Toast.LENGTH_SHORT).show()
                    }
                }
            }) { Text("保存") }
        })
        Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally) {
            Box(Modifier.size(96.dp).clip(CircleShape).clickable { picker.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)) }, contentAlignment = Alignment.Center) {
                if (avatar != null) AsyncImage(File(avatar!!), null, contentScale = ContentScale.Crop, modifier = Modifier.fillMaxSize())
                else acct?.let { UserAvatar(it.userId, 96.dp) }
                Box(Modifier.align(Alignment.BottomEnd).size(30.dp).clip(CircleShape).background(Hush.colors.accent), contentAlignment = Alignment.Center) {
                    Icon(Icons.Filled.PhotoCamera, "更换头像", tint = Color.White, modifier = Modifier.size(16.dp))
                }
            }
            Spacer(Modifier.height(24.dp))
            OutlinedTextField(
                name, { name = it }, label = { Text("昵称") }, singleLine = true, shape = RoundedCornerShape(12.dp),
                colors = OutlinedTextFieldDefaults.colors(focusedBorderColor = Hush.colors.accent, unfocusedBorderColor = Hush.colors.divider),
                modifier = Modifier.fillMaxWidth().testTag("edit_name"),
            )
            Spacer(Modifier.height(12.dp))
            OutlinedTextField(
                about, { about = it }, label = { Text("个性签名") }, maxLines = 3, shape = RoundedCornerShape(12.dp),
                colors = OutlinedTextFieldDefaults.colors(focusedBorderColor = Hush.colors.accent, unfocusedBorderColor = Hush.colors.divider),
                modifier = Modifier.fillMaxWidth().testTag("edit_about"),
            )
            Spacer(Modifier.height(8.dp))
            Text(acct?.email ?: "", style = HushType.secondary, color = Hush.colors.textSecondary)
        }
    }
}
