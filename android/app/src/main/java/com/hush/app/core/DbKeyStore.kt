package com.hush.app.core

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import com.hush.app.util.HushLog
import java.security.KeyStore
import java.security.SecureRandom
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * 32 字节数据库密钥：首次随机生成，用 Android Keystore 中的 AES/GCM 密钥加密后存 SharedPreferences。
 *
 * 解包失败时不会静默换新密钥：只有确定旧密钥已永久无法恢复（包装密钥不存在 / 密文损坏）才生成新密钥；
 * Keystore 暂时不可用时重试，仍失败则抛出异常，保留旧密钥与数据（宁可本次启动失败，也不清空身份密钥和聊天记录）。
 */
object DbKeyStore {
    private const val TAG = "DbKeyStore"
    private const val KEY_ALIAS = "hush_db_wrap_key"
    private const val PREFS = "hush_secure"
    private const val PREF_BLOB = "db_key_blob"
    private const val ATTEMPTS = 3

    fun loadOrCreate(context: Context): ByteArray {
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val stored = prefs.getString(PREF_BLOB, null)
        if (stored != null) {
            var last: Throwable? = null
            for (attempt in 1..ATTEMPTS) {
                val present = try {
                    wrapKeyPresent()
                } catch (e: Exception) {
                    last = e; Thread.sleep(300); continue
                }
                try {
                    if (!present) throw IllegalStateException("wrapping key missing")
                    return unwrap(wrappingKey(), stored)
                } catch (e: Exception) {
                    last = e
                    when (CoreOpen.classifyUnwrapFailure(present, e)) {
                        CoreOpen.UnwrapFailure.TRANSIENT -> if (attempt < ATTEMPTS) Thread.sleep(300)
                        else -> {
                            // 旧库密钥已永久无法恢复：生成新密钥；旧数据库随后会因密钥不匹配被改名备份（见 HushCore.init）
                            HushLog.w(TAG, "db key unrecoverable, generating a new one", e)
                            return create(prefs)
                        }
                    }
                }
            }
            throw IllegalStateException("无法读取数据库密钥（Android Keystore 暂时不可用）", last)
        }
        return create(prefs)
    }

    private fun create(prefs: android.content.SharedPreferences): ByteArray {
        val raw = ByteArray(32).also { SecureRandom().nextBytes(it) }
        // 必须同步落盘：若进程在异步写入前退出，用该密钥建好的库下次将无法打开
        check(prefs.edit().putString(PREF_BLOB, wrap(wrappingKey(), raw)).commit()) { "保存数据库密钥失败" }
        return raw
    }

    private fun keyStore(): KeyStore = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }

    private fun wrapKeyPresent(): Boolean = keyStore().containsAlias(KEY_ALIAS)

    private fun wrappingKey(): SecretKey {
        (keyStore().getKey(KEY_ALIAS, null) as? SecretKey)?.let { return it }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        gen.init(
            KeyGenParameterSpec.Builder(KEY_ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build(),
        )
        return gen.generateKey()
    }

    private fun wrap(key: SecretKey, raw: ByteArray): String {
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.ENCRYPT_MODE, key)
        val ct = cipher.doFinal(raw)
        return Base64.encodeToString(cipher.iv + ct, Base64.NO_WRAP)
    }

    private fun unwrap(key: SecretKey, blob: String): ByteArray {
        val bytes = Base64.decode(blob, Base64.NO_WRAP)
        require(bytes.size > 12) { "blob too short" }
        val iv = bytes.copyOfRange(0, 12)
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.DECRYPT_MODE, key, GCMParameterSpec(128, iv))
        return cipher.doFinal(bytes, 12, bytes.size - 12)
    }
}
