package com.hush.app.util

/**
 * 窗口隐私策略（B12，JVM 单测见 WindowPrivacyTest）。
 * 返回 (是否 FLAG_SECURE, 是否隐藏最近任务缩略图)。
 *
 * - “屏幕安全”开：FLAG_SECURE（禁止截屏录屏，最近任务自然也看不到内容）。
 * - 只开“屏幕锁”：上锁的意义就是别人拿到手机看不到聊天内容，最近任务缩略图必须隐藏。
 *   Android 13+ 用 setRecentsScreenshotEnabled(false)（不影响用户自己截屏）；更低版本没有这个接口，只能用 FLAG_SECURE。
 */
object WindowPrivacy {
    fun decide(screenSecurity: Boolean, screenLock: Boolean, sdk: Int): Pair<Boolean, Boolean> {
        val hideRecents = screenSecurity || screenLock
        val flagSecure = screenSecurity || (screenLock && sdk < 33)
        return flagSecure to hideRecents
    }
}
