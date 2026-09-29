package com.hush.app.ui.contacts

import android.Manifest
import android.content.pm.PackageManager
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.PhotoLibrary
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Icon
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import com.google.mlkit.vision.barcode.BarcodeScannerOptions
import com.google.mlkit.vision.barcode.BarcodeScanning
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.common.InputImage
import com.hush.app.core.HushCore
import com.hush.app.ui.components.HushTopBar
import com.hush.app.ui.theme.Hush
import com.hush.app.ui.theme.HushType
import com.hush.app.util.QrCodes
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean

/**
 * 扫一扫（CameraX + ML Kit）。safetyUser 非空时为安全码核验模式，否则识别好友名片 hush-user:v1:<id>。
 */
@Composable
fun ScanScreen(safetyUser: String?, onBack: () -> Unit, onUser: (String) -> Unit) {
    val context = LocalContext.current
    var granted by remember { mutableStateOf(ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) }
    val perm = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted = it }
    LaunchedEffect(Unit) { if (!granted) perm.launch(Manifest.permission.CAMERA) }
    var result by remember { mutableStateOf<String?>(null) }
    val handled = remember { AtomicBoolean(false) }
    val scope = rememberCoroutineScope()

    fun handle(raw: String) {
        if (!handled.compareAndSet(false, true)) return
        if (safetyUser != null) {
            // 核验会读写数据库，放到 IO 线程
            scope.launch {
                result = withContext(Dispatchers.IO) {
                    try {
                        if (HushCore.client.verifySafetyQr(safetyUser, raw)) "✅ 安全码一致，已标记为已核验" else "❌ 安全码不一致，请确认扫描的是对方的安全码二维码"
                    } catch (e: Exception) { e.userMessage() }
                }
            }
        } else {
            val uid = QrCodes.parseUser(raw)
            if (uid != null) onUser(uid) else result = "无法识别的二维码"
        }
    }
    val pickImage = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri: Uri? ->
        uri ?: return@rememberLauncherForActivityResult
        val img = runCatching { InputImage.fromFilePath(context, uri) }.getOrNull() ?: return@rememberLauncherForActivityResult
        scanner().process(img).addOnSuccessListener { codes ->
            val v = codes.firstNotNullOfOrNull { it.rawValue }
            if (v != null) handle(v) else result = "图片中没有找到二维码"
        }
    }

    Column(Modifier.fillMaxSize().background(Color.Black)) {
        HushTopBar(if (safetyUser != null) "扫码核验安全码" else "扫一扫", onBack = onBack)
        Box(Modifier.weight(1f), contentAlignment = Alignment.Center) {
            if (granted) CameraPreview { handle(it) }
            else Text("需要相机权限才能扫码", color = Color.White)
            Box(Modifier.size(240.dp).border(2.dp, Hush.colors.accent, RoundedCornerShape(16.dp)))
        }
        Column(Modifier.fillMaxWidth().background(Hush.colors.background).padding(16.dp), horizontalAlignment = Alignment.CenterHorizontally) {
            Text(if (safetyUser != null) "将对方的安全码二维码放入框内" else "将好友的二维码名片放入框内", style = HushType.secondary, color = Hush.colors.textSecondary)
            Spacer(Modifier.height(8.dp))
            OutlinedButton(onClick = { pickImage.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)) }) {
                Icon(Icons.Filled.PhotoLibrary, null); Text("  从相册选择")
            }
        }
    }
    result?.let {
        AlertDialog(
            onDismissRequest = { result = null; onBack() },
            title = { Text(if (safetyUser != null) "核验结果" else "扫描结果") },
            text = { Text(it) },
            confirmButton = { TextButton(onClick = { result = null; onBack() }) { Text("好") } },
        )
    }
}

private fun scanner() = BarcodeScanning.getClient(BarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build())

@Composable
private fun CameraPreview(onCode: (String) -> Unit) {
    val context = LocalContext.current
    val owner = LocalLifecycleOwner.current
    val executor = remember { Executors.newSingleThreadExecutor() }
    val scanner = remember { scanner() }
    DisposableEffect(Unit) { onDispose { executor.shutdown(); scanner.close() } }
    AndroidView(
        modifier = Modifier.fillMaxSize(),
        factory = { ctx ->
            val view = PreviewView(ctx)
            val providerFuture = ProcessCameraProvider.getInstance(ctx)
            providerFuture.addListener({
                val provider = providerFuture.get()
                val preview = Preview.Builder().build().also { it.surfaceProvider = view.surfaceProvider }
                val analysis = ImageAnalysis.Builder().setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST).build()
                analysis.setAnalyzer(executor) { proxy ->
                    @androidx.annotation.OptIn(androidx.camera.core.ExperimentalGetImage::class)
                    val media = proxy.image
                    if (media == null) { proxy.close(); return@setAnalyzer }
                    val img = InputImage.fromMediaImage(media, proxy.imageInfo.rotationDegrees)
                    scanner.process(img)
                        .addOnSuccessListener { codes -> codes.firstNotNullOfOrNull { it.rawValue }?.let { v -> ContextCompat.getMainExecutor(context).execute { onCode(v) } } }
                        .addOnCompleteListener { proxy.close() }
                }
                runCatching {
                    provider.unbindAll()
                    provider.bindToLifecycle(owner, CameraSelector.DEFAULT_BACK_CAMERA, preview, analysis)
                }
            }, ContextCompat.getMainExecutor(ctx))
            view
        },
    )
}
