package com.hush.app.util

import android.content.ContentValues
import android.content.Context
import android.graphics.BitmapFactory
import android.net.Uri
import android.os.Build
import android.os.Environment
import android.provider.MediaStore
import java.io.File

/** 把图片保存到系统相册（MediaStore）。在 IO 线程调用。 */
object GallerySaver {
    /** 按解码得到的真实格式决定 MIME 与扩展名（不认识的按 JPEG）。 */
    fun fileType(detectedMime: String?): Pair<String, String> = when (detectedMime?.lowercase()) {
        "image/png" -> "image/png" to "png"
        "image/gif" -> "image/gif" to "gif"
        "image/webp" -> "image/webp" to "webp"
        "image/heic", "image/heif" -> detectedMime.lowercase() to "heic"
        else -> "image/jpeg" to "jpg"
    }

    /** API 26–28 写入 MediaStore 需要 WRITE_EXTERNAL_STORAGE 运行时权限。 */
    val needsLegacyPermission: Boolean get() = Build.VERSION.SDK_INT < 29

    fun save(context: Context, file: File): Boolean {
        val opts = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeFile(file.path, opts)
        val (mime, ext) = fileType(opts.outMimeType)
        val resolver = context.contentResolver
        val values = ContentValues().apply {
            put(MediaStore.Images.Media.DISPLAY_NAME, "hush_${System.currentTimeMillis()}.$ext")
            put(MediaStore.Images.Media.MIME_TYPE, mime)
            if (Build.VERSION.SDK_INT >= 29) {
                put(MediaStore.Images.Media.RELATIVE_PATH, Environment.DIRECTORY_PICTURES + "/Hush")
                put(MediaStore.Images.Media.IS_PENDING, 1)
            }
        }
        var uri: Uri? = null
        return try {
            uri = resolver.insert(MediaStore.Images.Media.EXTERNAL_CONTENT_URI, values) ?: return false
            (resolver.openOutputStream(uri) ?: error("openOutputStream")).use { out -> file.inputStream().use { it.copyTo(out) } }
            if (Build.VERSION.SDK_INT >= 29) {
                resolver.update(uri, ContentValues().apply { put(MediaStore.Images.Media.IS_PENDING, 0) }, null, null)
            }
            true
        } catch (e: Exception) {
            HushLog.w("GallerySaver", "save failed", e)
            // 写入失败：删掉已插入的空条目，避免相册里出现损坏的图片
            uri?.let { runCatching { resolver.delete(it, null, null) } }
            false
        }
    }
}
