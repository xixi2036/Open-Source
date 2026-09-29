package com.hush.app.core

import uniffi.hushcore.HushException
import java.io.File
import javax.crypto.AEADBadTagException

/**
 * 打开本地数据的判定逻辑（JVM 单测见 CoreOpenTest）。
 * 原则：只有能确定“旧数据已无法用当前密钥打开”时才重置，而且重置是改名备份，不是删除；
 * 其余异常（磁盘满、IO、迁移失败、Keystore 暂时不可用）一律报错/重试，不动任何数据。
 */
object CoreOpen {
    /**
     * 核心在 db_key 与已有库不匹配时返回专门的 [HushException.KeyMismatch]（接口 v2 / C-E）。
     * 字段解密失败等其他存储错误仍是 Storage，不能据此重置数据。
     */
    fun isKeyMismatch(e: Throwable): Boolean = e is HushException.KeyMismatch

    /** 备份目录：与数据目录同级，带时间戳。 */
    fun backupDir(dataDir: File, now: Long): File = File(dataDir.parentFile, "${dataDir.name}.bak-$now")

    enum class UnwrapFailure {
        /** Keystore 中的包装密钥已不存在（例如被系统清除）：旧库密钥永久丢失。 */
        KEY_LOST,
        /** 存储的密文损坏（校验失败 / 格式错误）：同样无法恢复。 */
        CORRUPT,
        /** Keystore 暂时不可用等：不能覆盖旧密钥，应重试或报错。 */
        TRANSIENT,
    }

    fun classifyUnwrapFailure(wrapKeyPresent: Boolean, error: Throwable): UnwrapFailure = when {
        !wrapKeyPresent -> UnwrapFailure.KEY_LOST
        error is AEADBadTagException -> UnwrapFailure.CORRUPT
        error is IllegalArgumentException || error is ArrayIndexOutOfBoundsException -> UnwrapFailure.CORRUPT
        // KeyStoreException / ProviderException 等：Keystore 服务暂时异常
        else -> UnwrapFailure.TRANSIENT
    }
}
