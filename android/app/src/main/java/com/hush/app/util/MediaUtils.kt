package com.hush.app.util

import androidx.core.graphics.get
import androidx.core.graphics.scale
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

    /** 发送前处理好的图片。 */
    data class PreparedImage(val file: File, val mime: String, val name: String)

    /**
     * 聊天图片发送前的处理（B14）：
     * - GIF 原样发送（保留动画）；
     * - 带透明像素的 PNG / WebP 缩放后仍存为 PNG（转 JPEG 会把透明区域变成黑底）；
     * - 其余压缩为 JPEG。
     * 返回的文件可能就是 [picked] 本身（GIF、无法解码时），调用方用 [deleteTemp] 一并清理即可。
     */
    fun prepareImage(context: Context, picked: Picked, maxSide: Int = 2048, quality: Int = 85): PreparedImage {
        if (picked.mime == "image/gif") return PreparedImage(picked.file, "image/gif", picked.name)
        val mayHaveAlpha = picked.mime == "image/png" || picked.mime == "image/webp"
        if (mayHaveAlpha) {
            val bmp = decodeScaled(picked.file.path, maxSide)
            if (bmp != null && hasTransparency(bmp)) {
                val out = File(outgoingDir(context), "img_${UUID.randomUUID().toString().take(8)}.png")
                out.outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
                return PreparedImage(out, "image/png", out.name)
            }
        }
        val out = compressJpeg(context, picked.file.path, maxSide, quality)
        // 无法解码时 compressJpeg 原样返回输入文件：保持原 mime
        return if (out == picked.file) PreparedImage(out, picked.mime, picked.name) else PreparedImage(out, "image/jpeg", out.name)
    }

    /** 是否真的含有透明像素（有 alpha 通道但全不透明的 PNG 照片仍按 JPEG 压缩）。抽样检查，最多约 4096 个点。 */
    fun hasTransparency(bmp: Bitmap): Boolean {
        if (!bmp.hasAlpha()) return false
        val stepX = (bmp.width / 64).coerceAtLeast(1)
        val stepY = (bmp.height / 64).coerceAtLeast(1)
        var y = 0
        while (y < bmp.height) {
            var x = 0
            while (x < bmp.width) {
                if ((bmp[x, y] ushr 24) < 250) return true
                x += stepX
            }
            y += stepY
        }
        return false
    }

    /** content:// 的大小（字节），未知时返回 -1。用于在拷贝前拦截过大的视频。 */
    fun querySize(context: Context, uri: Uri): Long = runCatching {
        context.contentResolver.query(uri, arrayOf(OpenableColumns.SIZE), null, null, null)?.use { c ->
            if (c.moveToFirst() && !c.isNull(0)) c.getLong(0) else -1L
        } ?: -1L
    }.getOrDefault(-1L)

    data class VideoInfo(val width: Int, val height: Int, val durationMs: Long, val thumbnail: ByteArray?)

    /** 视频的尺寸（已按旋转角纠正）、时长与 ≤10KB 的 JPEG 首帧缩略图。 */
    fun videoInfo(path: String): VideoInfo {
        val r = android.media.MediaMetadataRetriever()
        return try {
            r.setDataSource(path)
            fun meta(k: Int) = r.extractMetadata(k)?.toLongOrNull() ?: 0L
            var w = meta(android.media.MediaMetadataRetriever.METADATA_KEY_VIDEO_WIDTH).toInt()
            var h = meta(android.media.MediaMetadataRetriever.METADATA_KEY_VIDEO_HEIGHT).toInt()
            val rot = meta(android.media.MediaMetadataRetriever.METADATA_KEY_VIDEO_ROTATION).toInt()
            if (rot == 90 || rot == 270) { val t = w; w = h; h = t }
            val dur = meta(android.media.MediaMetadataRetriever.METADATA_KEY_DURATION)
            val frame = runCatching { r.getFrameAtTime(0, android.media.MediaMetadataRetriever.OPTION_CLOSEST_SYNC) }.getOrNull()
            VideoInfo(w, h, dur, frame?.let { jpegThumb(scaleTo(it, 160)) })
        } catch (e: Exception) {
            VideoInfo(0, 0, 0, null)
        } finally {
            runCatching { r.release() }
        }
    }

    private fun scaleTo(bmp: Bitmap, maxSide: Int): Bitmap {
        val long = maxOf(bmp.width, bmp.height)
        if (long <= maxSide) return bmp
        val s = maxSide.toFloat() / long
        return bmp.scale((bmp.width * s).toInt().coerceAtLeast(1), (bmp.height * s).toInt().coerceAtLeast(1))
    }

    private fun jpegThumb(bmp: Bitmap): ByteArray {
        var q = 60
        while (true) {
            val bos = ByteArrayOutputStream()
            bmp.compress(Bitmap.CompressFormat.JPEG, q, bos)
            if (bos.size() <= 10_000 || q <= 20) return bos.toByteArray()
            q -= 15
        }
    }

    /** 应用内拍照的输出文件（cacheDir/outgoing，经 FileProvider 交给系统相机写入）。 */
    fun newCaptureFile(context: Context): File = File(outgoingDir(context), "capture_${System.currentTimeMillis()}.jpg")

    fun humanSize(bytes: Long): String = when {
        bytes < 1024 -> "$bytes B"
        bytes < 1024 * 1024 -> "%.1f KB".format(bytes / 1024.0)
        else -> "%.1f MB".format(bytes / 1024.0 / 1024.0)
    }
}
