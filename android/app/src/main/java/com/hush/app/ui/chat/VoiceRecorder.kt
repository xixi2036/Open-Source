package com.hush.app.ui.chat

import android.content.Context
import android.media.MediaRecorder
import android.os.Build
import com.hush.app.util.MediaUtils
import java.io.File

/** 按住录音：MediaRecorder（AAC in m4a）。 */
class VoiceRecorder(private val context: Context) {
    private var recorder: MediaRecorder? = null
    private var file: File? = null
    private var startedAt = 0L

    fun start(): Boolean {
        stopQuietly()
        val out = File(MediaUtils.voiceDir(context), "rec_${System.currentTimeMillis()}.m4a")
        val r = if (Build.VERSION.SDK_INT >= 31) MediaRecorder(context) else @Suppress("DEPRECATION") MediaRecorder()
        return try {
            r.setAudioSource(MediaRecorder.AudioSource.MIC)
            r.setOutputFormat(MediaRecorder.OutputFormat.MPEG_4)
            r.setAudioEncoder(MediaRecorder.AudioEncoder.AAC)
            r.setAudioEncodingBitRate(64_000)
            r.setAudioSamplingRate(44_100)
            r.setOutputFile(out.path)
            r.prepare()
            r.start()
            recorder = r; file = out; startedAt = System.currentTimeMillis()
            true
        } catch (e: Exception) {
            r.release(); out.delete(); false
        }
    }

    /** 结束录音，返回 (文件, 时长毫秒)；过短或失败返回 null。 */
    fun stop(): Pair<File, Long>? {
        val r = recorder ?: return null
        val f = file
        val dur = System.currentTimeMillis() - startedAt
        recorder = null; file = null
        val ok = runCatching { r.stop() }.isSuccess
        r.release()
        if (!ok || f == null || dur < 800) { f?.delete(); return null }
        return f to dur
    }

    fun cancel() {
        stopQuietly()
    }

    private fun stopQuietly() {
        recorder?.let { runCatching { it.stop() }; it.release() }
        file?.delete()
        recorder = null; file = null
    }
}
