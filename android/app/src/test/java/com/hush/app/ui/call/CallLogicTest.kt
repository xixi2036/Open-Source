package com.hush.app.ui.call

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class CallLogicTest {
    @Test
    fun busyPhases() {
        val busy = CallPhase.entries.filter(CallLogic::isBusy)
        assertEquals(listOf(CallPhase.OUTGOING, CallPhase.INCOMING, CallPhase.CONNECTING, CallPhase.ACTIVE), busy)
    }

    /** A03：从锁屏接听后 phase 变为 CONNECTING / ACTIVE，界面仍须显示在锁屏之上。 */
    @Test
    fun showWhenLockedCoversWholeCallNotOnlyRinging() {
        assertTrue(CallLogic.showWhenLocked(CallPhase.INCOMING))
        assertTrue(CallLogic.showWhenLocked(CallPhase.CONNECTING))
        assertTrue(CallLogic.showWhenLocked(CallPhase.ACTIVE))
        assertTrue(CallLogic.showWhenLocked(CallPhase.OUTGOING))
        assertFalse(CallLogic.showWhenLocked(CallPhase.ENDED))
        assertFalse(CallLogic.showWhenLocked(CallPhase.IDLE))
    }

    @Test
    fun turnScreenOnOnlyWhileRinging() {
        assertEquals(listOf(CallPhase.INCOMING), CallPhase.entries.filter(CallLogic::turnScreenOn))
    }

    /** A02：建立过程中挂断（ENDED）或换成另一通话后，挂起点恢复的协程必须放弃。 */
    @Test
    fun stillCurrentRejectsEndedOrReplacedCall() {
        assertTrue(CallLogic.stillCurrent("c1", "c1", CallPhase.OUTGOING))
        assertTrue(CallLogic.stillCurrent("c1", "c1", CallPhase.CONNECTING))
        assertFalse(CallLogic.stillCurrent("c1", "c1", CallPhase.ENDED))
        assertFalse(CallLogic.stillCurrent("c1", "", CallPhase.IDLE))
        assertFalse(CallLogic.stillCurrent("c1", "c2", CallPhase.OUTGOING))
    }

    @Test
    fun fastSyncWhileEstablishing() {
        assertEquals(
            listOf(CallPhase.OUTGOING, CallPhase.INCOMING, CallPhase.CONNECTING),
            CallPhase.entries.filter(CallLogic::needsFastSync),
        )
    }

    @Test
    fun remoteFrameWindow() {
        assertFalse(CallLogic.hasRecentFrame(0, 10_000))
        assertTrue(CallLogic.hasRecentFrame(9_000, 10_000))
        assertFalse(CallLogic.hasRecentFrame(8_000, 10_000))
    }

    /** A13 / 音频设备选择：有线 > 蓝牙 > （视频外放 / 语音听筒）；没有听筒的设备退回扬声器。 */
    @Test
    fun defaultRoute() {
        val phone = listOf(AudioRoute.EARPIECE, AudioRoute.SPEAKER)
        assertEquals(AudioRoute.SPEAKER, CallLogic.defaultRoute(video = true, available = phone))
        assertEquals(AudioRoute.EARPIECE, CallLogic.defaultRoute(video = false, available = phone))
        assertEquals(AudioRoute.BLUETOOTH, CallLogic.defaultRoute(video = true, available = phone + AudioRoute.BLUETOOTH))
        assertEquals(AudioRoute.WIRED, CallLogic.defaultRoute(video = false, available = phone + AudioRoute.BLUETOOTH + AudioRoute.WIRED))
        assertEquals(AudioRoute.SPEAKER, CallLogic.defaultRoute(video = false, available = listOf(AudioRoute.SPEAKER)))
    }

    /** 只有听筒/扬声器时按钮直接切换；接了耳机或蓝牙时返回 null（弹出设备列表）。 */
    @Test
    fun toggleTarget() {
        val phone = listOf(AudioRoute.EARPIECE, AudioRoute.SPEAKER)
        assertEquals(AudioRoute.SPEAKER, CallLogic.toggleTarget(AudioRoute.EARPIECE, phone))
        assertEquals(AudioRoute.EARPIECE, CallLogic.toggleTarget(AudioRoute.SPEAKER, phone))
        assertEquals(AudioRoute.SPEAKER, CallLogic.toggleTarget(AudioRoute.SPEAKER, listOf(AudioRoute.SPEAKER)))
        assertEquals(null, CallLogic.toggleTarget(AudioRoute.EARPIECE, phone + AudioRoute.BLUETOOTH))
    }

    /** 耳机插拔：新接入的设备自动切过去；当前设备消失回默认；其余保持用户选择。 */
    @Test
    fun routeAfterDeviceChange() {
        val phone = listOf(AudioRoute.EARPIECE, AudioRoute.SPEAKER)
        val withBt = phone + AudioRoute.BLUETOOTH
        assertEquals(AudioRoute.BLUETOOTH, CallLogic.routeAfterDeviceChange(AudioRoute.SPEAKER, phone, withBt, video = true))
        assertEquals(AudioRoute.EARPIECE, CallLogic.routeAfterDeviceChange(AudioRoute.BLUETOOTH, withBt, phone, video = false))
        assertEquals(AudioRoute.SPEAKER, CallLogic.routeAfterDeviceChange(AudioRoute.BLUETOOTH, withBt, phone, video = true))
        assertEquals(AudioRoute.SPEAKER, CallLogic.routeAfterDeviceChange(AudioRoute.SPEAKER, withBt, withBt, video = false))
    }

    /** B7：距离感应只在语音通话 + 听筒 + 呼叫/通话阶段启用。 */
    @Test
    fun proximity() {
        assertTrue(CallLogic.proximityOn(CallPhase.ACTIVE, video = false, route = AudioRoute.EARPIECE))
        assertTrue(CallLogic.proximityOn(CallPhase.OUTGOING, video = false, route = AudioRoute.EARPIECE))
        assertFalse(CallLogic.proximityOn(CallPhase.ACTIVE, video = true, route = AudioRoute.EARPIECE))
        assertFalse(CallLogic.proximityOn(CallPhase.ACTIVE, video = false, route = AudioRoute.SPEAKER))
        assertFalse(CallLogic.proximityOn(CallPhase.INCOMING, video = false, route = AudioRoute.EARPIECE))
        assertFalse(CallLogic.proximityOn(CallPhase.ENDED, video = false, route = AudioRoute.EARPIECE))
    }

    /** B6：铃声与振动遵循系统情景模式（0 静音、1 振动、2 响铃）。 */
    @Test
    fun ringerMode() {
        assertFalse(CallLogic.ringSound(0)); assertFalse(CallLogic.ringVibrate(0))
        assertFalse(CallLogic.ringSound(1)); assertTrue(CallLogic.ringVibrate(1))
        assertTrue(CallLogic.ringSound(2)); assertTrue(CallLogic.ringVibrate(2))
    }

    /** B9：同时互拨时恰好一方让步，且两端按同一规则得出一致结论。 */
    @Test
    fun glare() {
        val a = "0a1b"; val b = "f9e8"
        assertFalse(CallLogic.yieldsOnGlare(myId = a, peerId = b))
        assertTrue(CallLogic.yieldsOnGlare(myId = b, peerId = a))
        assertTrue(CallLogic.yieldsOnGlare(a, b) != CallLogic.yieldsOnGlare(b, a))
    }

    /** 通话最小化：来电响铃与空闲 / 已结束不能最小化。 */
    @Test
    fun minimize() {
        assertTrue(CallLogic.canMinimize(CallPhase.OUTGOING))
        assertTrue(CallLogic.canMinimize(CallPhase.CONNECTING))
        assertTrue(CallLogic.canMinimize(CallPhase.ACTIVE))
        assertFalse(CallLogic.canMinimize(CallPhase.INCOMING))
        assertFalse(CallLogic.canMinimize(CallPhase.ENDED))
        assertFalse(CallLogic.canMinimize(CallPhase.IDLE))
    }

    /** A05：前台服务类型按已授权权限计算，未授权的类型不能声明（否则 startForeground 抛异常）。 */
    @Test
    fun serviceTypes() {
        assertEquals(CallLogic.ServiceTypes(microphone = true, camera = true), CallLogic.serviceTypes(true, true, true))
        assertEquals(CallLogic.ServiceTypes(microphone = true, camera = false), CallLogic.serviceTypes(false, true, true))
        assertEquals(CallLogic.ServiceTypes(microphone = true, camera = false), CallLogic.serviceTypes(true, true, false))
        assertFalse(CallLogic.serviceTypes(true, false, false).any)
    }
}
