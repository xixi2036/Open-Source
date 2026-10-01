package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Test

/** B12：开启屏幕锁时最近任务不得露出内容。 */
class WindowPrivacyTest {
    @Test
    fun decide() {
        // (FLAG_SECURE, 隐藏最近任务缩略图)
        assertEquals(false to false, WindowPrivacy.decide(screenSecurity = false, screenLock = false, sdk = 36))
        assertEquals(true to true, WindowPrivacy.decide(screenSecurity = true, screenLock = false, sdk = 36))
        // 只开屏幕锁：Android 13+ 只隐藏缩略图，仍可截屏
        assertEquals(false to true, WindowPrivacy.decide(screenSecurity = false, screenLock = true, sdk = 33))
        // 更低版本没有该接口，只能 FLAG_SECURE
        assertEquals(true to true, WindowPrivacy.decide(screenSecurity = false, screenLock = true, sdk = 32))
    }
}
