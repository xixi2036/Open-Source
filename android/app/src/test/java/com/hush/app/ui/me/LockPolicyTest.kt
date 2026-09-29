package com.hush.app.ui.me

import androidx.biometric.BiometricManager
import org.junit.Assert.assertEquals
import org.junit.Test

class LockPolicyTest {
    /** A08：只有设备已无任何凭据时才关闭屏幕锁；暂时性状态保持锁定并允许重试。 */
    @Test
    fun onlyNoneEnrolledDisablesLock() {
        assertEquals(LockPolicy.Action.PROMPT, LockPolicy.decide(BiometricManager.BIOMETRIC_SUCCESS))
        assertEquals(LockPolicy.Action.DISABLE, LockPolicy.decide(BiometricManager.BIOMETRIC_ERROR_NONE_ENROLLED))
        listOf(
            BiometricManager.BIOMETRIC_ERROR_HW_UNAVAILABLE,
            BiometricManager.BIOMETRIC_STATUS_UNKNOWN,
            BiometricManager.BIOMETRIC_ERROR_SECURITY_UPDATE_REQUIRED,
            BiometricManager.BIOMETRIC_ERROR_UNSUPPORTED,
            BiometricManager.BIOMETRIC_ERROR_NO_HARDWARE,
        ).forEach { assertEquals("status=$it", LockPolicy.Action.RETRY, LockPolicy.decide(it)) }
    }
}
