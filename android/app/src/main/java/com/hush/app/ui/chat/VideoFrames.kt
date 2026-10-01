package com.hush.app.ui.chat

import android.graphics.Bitmap
import android.media.MediaMetadataRetriever
import android.util.LruCache
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/** 本地视频的首帧（聊天气泡里的封面），按路径缓存少量几张。 */
object VideoFrames {
    private val cache = LruCache<String, ImageBitmap>(12)

    suspend fun load(path: String): ImageBitmap? {
        cache.get(path)?.let { return it }
        return withContext(Dispatchers.IO) {
            val r = MediaMetadataRetriever()
            try {
                r.setDataSource(path)
                val bmp: Bitmap? = if (android.os.Build.VERSION.SDK_INT >= 27) {
                    r.getScaledFrameAtTime(0, MediaMetadataRetriever.OPTION_CLOSEST_SYNC, 640, 640)
                } else r.getFrameAtTime(0, MediaMetadataRetriever.OPTION_CLOSEST_SYNC)
                bmp?.asImageBitmap()?.also { cache.put(path, it) }
            } catch (e: Exception) {
                null
            } finally {
                runCatching { r.release() }
            }
        }
    }
}
