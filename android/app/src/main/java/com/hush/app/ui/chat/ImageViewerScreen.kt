package com.hush.app.ui.chat

import android.Manifest
import android.content.pm.PackageManager
import android.graphics.BitmapFactory
import android.widget.Toast
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.gestures.rememberTransformableState
import androidx.compose.foundation.gestures.transformable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.pager.HorizontalPager
import androidx.compose.foundation.pager.rememberPagerState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Download
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import coil3.compose.AsyncImage
import coil3.request.ImageRequest
import coil3.size.Size
import com.hush.app.R
import com.hush.app.core.HushCore
import com.hush.app.ui.components.LightStatusBarIcons
import com.hush.app.util.GallerySaver
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.AttachmentKind
import uniffi.hushcore.MessageBody
import java.io.File

/**
 * 全屏图片查看：双指缩放（平移不超出图片边界）、双击放大、保存到相册。
 * 从聊天页进入（[convId] 非空）时可左右滑动查看同一会话里已下载的其他图片（C6）。
 */
@Composable
fun ImageViewerScreen(path: String, onBack: () -> Unit, convId: String? = null) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    // 同会话的图片：最近 300 条消息里已下载到本地的图片，按时间升序；加载完成前只有当前这张
    var paths by remember { mutableStateOf(listOf(path)) }
    val pager = rememberPagerState(initialPage = 0) { paths.size }
    LaunchedEffect(convId, path) {
        if (convId == null) return@LaunchedEffect
        val all = withContext(Dispatchers.IO) { conversationImages(convId) }
        val idx = all.indexOf(path)
        if (idx >= 0 && all.size > 1) {
            paths = all
            pager.scrollToPage(idx)
        }
    }
    val current = paths.getOrElse(pager.currentPage) { path }
    fun save() {
        scope.launch {
            val ok = withContext(Dispatchers.IO) { GallerySaver.save(context, File(current)) }
            Toast.makeText(context, if (ok) "已保存到相册" else "保存失败", Toast.LENGTH_SHORT).show()
        }
    }
    // Android 8–9：写入相册需要存储权限
    val storageDenied = stringResource(R.string.perm_storage_save)
    val storagePerm = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { ok ->
        if (ok) save() else Toast.makeText(context, storageDenied, Toast.LENGTH_SHORT).show()
    }
    LightStatusBarIcons()
    /** 当前页是否处于放大状态：放大时单指拖动用于平移，不翻页。 */
    var zoomed by remember { mutableStateOf(false) }
    LaunchedEffect(pager.currentPage) { zoomed = false }
    Box(Modifier.fillMaxSize().background(Color.Black).testTag("image_viewer")) {
        HorizontalPager(pager, userScrollEnabled = !zoomed, modifier = Modifier.fillMaxSize(), key = { paths[it] }) { page ->
            ZoomableImage(paths[page], active = page == pager.currentPage, onTap = onBack, onZoomChange = { if (page == pager.currentPage) zoomed = it })
        }
        // 顶部渐变遮罩：白色/浅色图片上也能看清关闭与保存按钮
        Row(
            Modifier.fillMaxWidth()
                .background(Brush.verticalGradient(listOf(Color.Black.copy(alpha = 0.55f), Color.Transparent)))
                .statusBarsPadding().padding(bottom = 16.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            IconButton(onClick = onBack) { Icon(Icons.Filled.Close, "关闭", tint = Color.White) }
            Box(Modifier.weight(1f), contentAlignment = Alignment.Center) {
                if (paths.size > 1) Text("${pager.currentPage + 1} / ${paths.size}", color = Color.White, modifier = Modifier.testTag("image_index"))
            }
            IconButton(onClick = {
                if (GallerySaver.needsLegacyPermission &&
                    ContextCompat.checkSelfPermission(context, Manifest.permission.WRITE_EXTERNAL_STORAGE) != PackageManager.PERMISSION_GRANTED
                ) storagePerm.launch(Manifest.permission.WRITE_EXTERNAL_STORAGE)
                else save()
            }) { Icon(Icons.Filled.Download, "保存到相册", tint = Color.White) }
        }
    }
}

/** 会话里已下载的图片路径（按时间升序）。 */
private fun conversationImages(convId: String): List<String> = runCatching {
    HushCore.client.messages(convId, null, 300u).flatMap { m ->
        if (m.deleted) emptyList()
        else (m.body as? MessageBody.Attachments)?.items.orEmpty()
            .filter { it.kind == AttachmentKind.IMAGE }
            .mapNotNull { a -> a.localPath?.takeIf { File(it).exists() } }
    }
}.getOrDefault(emptyList())

@Composable
private fun ZoomableImage(path: String, active: Boolean, onTap: () -> Unit, onZoomChange: (Boolean) -> Unit) {
    val context = LocalContext.current
    var scale by remember { mutableFloatStateOf(1f) }
    var offset by remember { mutableStateOf(Offset.Zero) }
    var container by remember { mutableStateOf(IntSize.Zero) }
    // 图片原始尺寸（只读文件头），用于计算按 Fit 显示后的实际大小，从而得到平移边界
    val imageSize = remember(path) {
        val o = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeFile(path, o)
        o.outWidth to o.outHeight
    }
    fun clamp(o: Offset, s: Float): Offset {
        val (cw, ch) = ZoomMath.fitSize(container.width.toFloat(), container.height.toFloat(), imageSize.first.toFloat(), imageSize.second.toFloat())
        val max = ZoomMath.maxOffset(s, container.width.toFloat(), container.height.toFloat(), cw, ch)
        val (x, y) = ZoomMath.clampOffset(o.x, o.y, max)
        return Offset(x, y)
    }
    // 翻到别的页后复位
    LaunchedEffect(active) { if (!active) { scale = 1f; offset = Offset.Zero } }
    val tf = rememberTransformableState { zoom, pan, _ ->
        val s = ZoomMath.clampScale(scale * zoom)
        scale = s
        offset = if (s == 1f) Offset.Zero else clamp(offset + pan, s)
        onZoomChange(s > 1f)
    }
    // 按原图尺寸解码：放大后不发虚（发送时已压到长边 ≤ 2048）
    val request = remember(path) { ImageRequest.Builder(context).data(File(path)).size(Size.ORIGINAL).build() }
    AsyncImage(
        request, "图片", contentScale = ContentScale.Fit,
        modifier = Modifier.fillMaxSize()
            .onSizeChanged { container = it }
            .pointerInput(Unit) {
                detectTapGestures(
                    onDoubleTap = {
                        if (scale > 1f) { scale = 1f; offset = Offset.Zero } else scale = 2.5f
                        onZoomChange(scale > 1f)
                    },
                    onTap = { onTap() },
                )
            }
            // 未放大时单指横向拖动交给翻页
            .transformable(tf, canPan = { scale > 1f })
            .graphicsLayer { scaleX = scale; scaleY = scale; translationX = offset.x; translationY = offset.y }
            .testTag("zoom_image"),
    )
}
