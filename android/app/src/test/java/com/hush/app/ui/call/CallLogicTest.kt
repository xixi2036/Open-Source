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

    /** A13：接耳机时视频通话不强制外放；语音通话默认听筒。 */
    @Test
    fun speakerDefault() {
        assertTrue(CallLogic.defaultSpeaker(video = true, headsetConnected = false))
        assertFalse(CallLogic.defaultSpeaker(video = true, headsetConnected = true))
        assertFalse(CallLogic.defaultSpeaker(video = false, headsetConnected = false))
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
