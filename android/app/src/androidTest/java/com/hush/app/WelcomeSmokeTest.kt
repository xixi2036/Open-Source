package com.hush.app

import androidx.compose.ui.test.assertIsDisplayed
import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onAllNodesWithTag
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.test.onNodeWithText
import androidx.compose.ui.test.performClick
import androidx.test.ext.junit.runners.AndroidJUnit4
import com.hush.app.core.HushCore
import org.junit.Assume.assumeTrue
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/**
 * 最小的界面冒烟测试：冷启动 → 本地数据库在后台打开（启动页）→ 未登录时进入欢迎页 → 点“开始使用”进入服务器地址页。
 * 覆盖 Application 初始化、核心库（JNA + libhushcore.so）加载、Compose 根界面与登录流程第一步。
 *
 * 运行（只对指定设备，全新安装的未登录状态）：
 *   ANDROID_SERIAL=emulator-5590 ./gradlew connectedDebugAndroidTest
 * 注意：Gradle 的 connected 任务结束后会卸载应用（设备上的登录数据随之清除），并且默认对所有已连接设备执行，
 * 所以务必用 ANDROID_SERIAL 限定设备。设备上已登录时本用例跳过（不会去动已有账号）。
 */
@RunWith(AndroidJUnit4::class)
class WelcomeSmokeTest {
    @get:Rule
    val rule = createAndroidComposeRule<MainActivity>()

    @Test
    fun launchesToWelcomeAndOpensServerStep() {
        // 启动页期间数据库在后台线程打开（含迁移），最多等 30 秒
        rule.waitUntil(30_000) { HushCore.ready.value }
        assumeTrue("设备上已登录，跳过欢迎页用例", !HushCore.isLoggedIn)
        rule.waitUntil(10_000) { rule.onAllNodesWithTag("start").fetchSemanticsNodes().isNotEmpty() }
        rule.onNodeWithText("Hush 密语").assertIsDisplayed()
        rule.onNodeWithTag("start").assertIsDisplayed().performClick()
        rule.waitUntil(5_000) { rule.onAllNodesWithTag("server").fetchSemanticsNodes().isNotEmpty() }
        rule.onNodeWithTag("server").assertIsDisplayed()
        rule.onNodeWithTag("next").assertIsDisplayed()
    }
}
