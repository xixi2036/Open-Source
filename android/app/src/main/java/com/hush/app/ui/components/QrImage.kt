package com.hush.app.ui.components

import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.FilterQuality
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.unit.Dp
import com.hush.app.util.QrCodes

@Composable
fun QrImage(content: String, size: Dp, modifier: Modifier = Modifier) {
    if (content.isEmpty()) return
    val bmp = remember(content) { QrCodes.bitmap(content).asImageBitmap() }
    Image(bmp, contentDescription = "二维码", modifier = modifier.size(size), filterQuality = FilterQuality.None)
}
