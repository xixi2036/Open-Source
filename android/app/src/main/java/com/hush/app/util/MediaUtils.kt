package com.hush.app.util

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Matrix
import android.net.Uri
import android.provider.OpenableColumns
import androidx.exifinterface.media.ExifInterface
import java.io.ByteArrayOutputStream
import java.io.File
import java.util.UUID

/**
 * 相册/文件 URI 处理：拷贝到私有缓存、尺寸、缩略图、压缩。
 *
 * 发送用的临时明文副本放在 cacheDir/outgoing（录音在 cacheDir/voice）。核心在 send_attachment / update_profile /
 * post_moment 调用时就会读入并在 files/ 自存一份，所以调用返回（无论成败）后应立即 [deleteTemp]；
 * 退出登录与 App 启动时再用 [clearTemp] 兜底清理。
 */
object MediaUtils {
    data class Picked(val file: File, val name: String, val mime: String, val size: Long)

    private const val DIR_OUTGOING = "outgoing"
    private const val DIR_VOICE = "voice"

    private fun outgoingDir(context: Context) = File(context.cacheDir, DIR_OUTGOING).apply { mkdirs() }

    /** 录音输出目录。 */
    fun voiceDir(context: Context) = File(context.cacheDir, DIR_VOICE).apply { mkdirs() }

    /** 删除临时文件（忽略 null 与重复）。 */
    fun deleteTemp(vararg files: File?) {
        files.filterNotNull().distinct().forEach { runCatching { it.delete() } }
    }

    /** 清空全部发送用临时文件（退出登录、App 启动时调用）。 */
    fun clearTemp(context: Context) {
        runCatching { File(context.cacheDir, DIR_OUTGOING).deleteRecursively() }
        runCatching { File(context.cacheDir, DIR_VOICE).deleteRecursively() }
    }

    /** 拷贝并压缩为 JPEG，中间的原图副本随即删除。 */
    fun prepareJpeg(context: Context, uri: Uri, maxSide: Int, quality: Int): File {
        val picked = copyToCache(context, uri)
        val out = try {
            compressJpeg(context, picked.file.path, maxSide, quality)
        } catch (e: Exception) {
            deleteTemp(picked.file); throw e
        }
        if (out != picked.file) deleteTemp(picked.file)
        return out
    }

    /** 头像：长边 512、质量 85（资料、群头像、注册共用）。 */
    fun prepareAvatar(context: Context, uri: Uri): File = prepareJpeg(context, uri, 512, 85)

    fun displayName(context: Context, uri: Uri): String? =
        context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
            if (c.moveToFirst()) c.getString(0) else null
        }

    /** 把 content:// 拷贝到 cacheDir/outgoing 下，返回本地文件（用完须 [deleteTemp]）。 */
    fun copyToCache(context: Context, uri: Uri): Picked {
        val name = displayName(context, uri) ?: "file_${System.currentTimeMillis()}"
        val mime = context.contentResolver.getType(uri) ?: "application/octet-stream"
        val safeName = name.replace(Regex("[/\\\\:]"), "_")
        val out = File(outgoingDir(context), "${UUID.randomUUID().toString().take(8)}_$safeName")
        try {
            (context.contentResolver.openInputStream(uri) ?: error("无法读取所选文件")).use { input -> out.outputStream().use { input.copyTo(it) } }
        } catch (e: Exception) {
            out.delete(); throw e
        }
        return Picked(out, name, mime, out.length())
    }

    fun imageSize(path: String): Pair<Int, Int> {
        val opts = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeFile(path, opts)
        val rot = rotation(path)
        return if (rot == 90 || rot == 270) opts.outHeight to opts.outWidth else opts.outWidth to opts.outHeight
    }

    private fun rotation(path: String): Int = runCatching {
        when (ExifInterface(path).getAttributeInt(ExifInterface.TAG_ORIENTATION, ExifInterface.ORIENTATION_NORMAL)) {
            ExifInterface.ORIENTATION_ROTATE_90 -> 90
            ExifInterface.ORIENTATION_ROTATE_180 -> 180
            ExifInterface.ORIENTATION_ROTATE_270 -> 270
            else -> 0
        }
    }.getOrDefault(0)

    /** 按长边缩放解码并纠正方向。 */
    fun decodeScaled(path: String, maxSide: Int): Bitmap? {
        val opts = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeFile(path, opts)
        if (opts.outWidth <= 0) return null
        var sample = 1
        while (maxOf(opts.outWidth, opts.outHeight) / (sample * 2) >= maxSide) sample *= 2
        val bmp = BitmapFactory.decodeFile(path, BitmapFactory.Options().apply { inSampleSize = sample }) ?: return null
        val longSide = maxOf(bmp.width, bmp.height)
        val scale = if (longSide > maxSide) maxSide.toFloat() / longSide else 1f
        val rot = rotation(path)
        if (scale == 1f && rot == 0) return bmp
        val m = Matrix().apply { postScale(scale, scale); postRotate(rot.toFloat()) }
        return Bitmap.createBitmap(bmp, 0, 0, bmp.width, bmp.height, m, true)
    }

    /** ≤10KB 的 JPEG 缩略图。 */
    fun thumbnail(path: String): ByteArray? {
        val bmp = decodeScaled(path, 96) ?: return null
        var q = 60
        while (true) {
            val bos = ByteArrayOutputStream()
            bmp.compress(Bitmap.CompressFormat.JPEG, q, bos)
            if (bos.size() <= 10_000 || q <= 20) return bos.toByteArray()
            q -= 15
        }
    }

    /** 压缩到长边 ≤ maxSide、JPEG 质量 quality，输出到 cacheDir/outgoing；无法解码时原样返回输入文件。 */
    fun compressJpeg(context: Context, path: String, maxSide: Int = 2048, quality: Int = 80): File {
        val bmp = decodeScaled(path, maxSide) ?: return File(path)
        val out = File(outgoingDir(context), "img_${UUID.randomUUID().toString().take(8)}.jpg")
        out.outputStream().use { bmp.compress(Bitmap.CompressFormat.JPEG, quality, it) }
        return out
    }

    fun humanSize(bytes: Long): String = when {
        bytes < 1024 -> "$bytes B"
        bytes < 1024 * 1024 -> "%.1f KB".format(bytes / 1024.0)
        else -> "%.1f MB".format(bytes / 1024.0 / 1024.0)
    }
}
