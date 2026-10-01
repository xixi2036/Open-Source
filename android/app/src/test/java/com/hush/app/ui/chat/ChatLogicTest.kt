package com.hush.app.ui.chat

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ChatLogicTest {
    private data class M(val id: String, val ts: Long, val v: Int = 0)
    private fun up(list: List<M>, m: M) = MessageOrder.upsert(list, m, { it.id }, { it.ts })

    /** B11：新消息按 sort_ts 插入，而不是一律追加。 */
    @Test
    fun upsertKeepsSortOrder() {
        val base = listOf(M("a", 10), M("b", 20), M("d", 40))
        assertEquals(listOf("a", "b", "d", "e"), up(base, M("e", 50)).map { it.id })
        assertEquals(listOf("a", "b", "c", "d"), up(base, M("c", 30)).map { it.id })
        assertEquals(listOf("z", "a", "b", "d"), up(base, M("z", 5)).map { it.id })
        assertEquals("相同时间戳排在已有消息之后", listOf("a", "b", "b2", "d"), up(base, M("b2", 20)).map { it.id })
        assertEquals(listOf("x"), up(emptyList(), M("x", 1)).map { it.id })
    }

    /** 已有消息：位置不变时原地替换；sort_ts 变化（服务端接受后改为服务端时间）时移动到新位置。 */
    @Test
    fun upsertReplacesAndMoves() {
        val base = listOf(M("a", 10), M("b", 20), M("c", 30))
        val same = up(base, M("b", 20, v = 1))
        assertEquals(listOf("a", "b", "c"), same.map { it.id }); assertEquals(1, same[1].v)
        assertEquals(listOf("a", "c", "b"), up(base, M("b", 35)).map { it.id })
        assertEquals(listOf("c", "a", "b"), up(base, M("c", 5)).map { it.id })
        assertEquals(3, up(base, M("a", 25)).size)
    }

    /** B10：计量网络下大图不自动下载；语音始终自动；超过 20MB 都不自动。 */
    @Test
    fun autoDownload() {
        assertTrue(AutoDownload.shouldAuto(isImage = true, isVoice = false, size = 5_000_000, metered = false))
        assertFalse(AutoDownload.shouldAuto(isImage = true, isVoice = false, size = 5_000_000, metered = true))
        assertTrue(AutoDownload.shouldAuto(isImage = true, isVoice = false, size = 800_000, metered = true))
        assertTrue(AutoDownload.shouldAuto(isImage = false, isVoice = true, size = 300_000, metered = true))
        assertFalse(AutoDownload.shouldAuto(isImage = true, isVoice = false, size = 20_000_000, metered = false))
        assertFalse("文件、视频不自动下载", AutoDownload.shouldAuto(isImage = false, isVoice = false, size = 10, metered = false))
    }

    /** C7：最长 60 秒，最后 10 秒倒计时。 */
    @Test
    fun recordLimit() {
        assertFalse(RecordLimit.reachedMax(59_999))
        assertTrue(RecordLimit.reachedMax(60_000))
        assertNull(RecordLimit.remainingHint(49_000))
        assertEquals(10, RecordLimit.remainingHint(50_000))
        assertEquals(1, RecordLimit.remainingHint(59_500))
        assertEquals(0, RecordLimit.remainingHint(60_000))
    }

    @Test
    fun videoLimit() {
        assertFalse(VideoLimit.tooLarge(100L * 1024 * 1024))
        assertTrue(VideoLimit.tooLarge(100L * 1024 * 1024 + 1))
        assertFalse("大小未知（-1）先放行，拷贝后再按实际大小判断", VideoLimit.tooLarge(-1))
    }

    /** 链接：只识别显式的 http / https；句末标点、中文标点不算在链接里。 */
    @Test
    fun links() {
        assertEquals(emptyList<Links.Link>(), Links.find("没有链接 a.b.c www.example.com"))
        val t = "看这个 https://example.com/a?b=1。还有http://10.0.2.2:8091/x, 以及(https://a.b/c_(d))"
        assertEquals(listOf("https://example.com/a?b=1", "http://10.0.2.2:8091/x", "https://a.b/c_(d)"), Links.find(t).map { it.url })
        val l = Links.find(t).first()
        assertEquals("https://example.com/a?b=1", t.substring(l.start, l.end))
        assertEquals(listOf("HTTPS://EXAMPLE.COM"), Links.find("HTTPS://EXAMPLE.COM!").map { it.url })
        assertEquals(emptyList<String>(), Links.find("http://").map { it.url })
        assertEquals(emptyList<String>(), Links.find("javascript:alert(1) file:///etc/passwd").map { it.url })
    }

    /** 非 https 链接打开前需要确认。 */
    @Test
    fun linkConfirm() {
        assertFalse(Links.needsConfirm("https://example.com"))
        assertFalse(Links.needsConfirm("HTTPS://example.com"))
        assertTrue(Links.needsConfirm("http://example.com"))
    }

    /** C6：平移不超出图片边界；没超出容器的方向不能平移。 */
    @Test
    fun zoomBounds() {
        assertEquals(1f, ZoomMath.clampScale(0.3f), 0f)
        assertEquals(5f, ZoomMath.clampScale(9f), 0f)
        // 1000x2000 的容器里放一张横图（显示为 1000x500）
        val (cw, ch) = ZoomMath.fitSize(1000f, 2000f, 2000f, 1000f)
        assertEquals(1000f, cw, 0.01f); assertEquals(500f, ch, 0.01f)
        // 放大 2 倍：宽 2000（超出 1000 → 每边 500），高 1000（未超出 2000 → 不能纵向平移）
        val max = ZoomMath.maxOffset(2f, 1000f, 2000f, cw, ch)
        assertEquals(500f to 0f, max)
        assertEquals(500f to 0f, ZoomMath.clampOffset(9999f, 300f, max))
        val neg = ZoomMath.clampOffset(-9999f, -300f, max)
        assertEquals(-500f, neg.first, 0f); assertEquals(0f, neg.second, 0f)
        assertEquals(0f to 0f, ZoomMath.maxOffset(1f, 1000f, 2000f, cw, ch))
        // 尺寸未知：按铺满容器处理
        assertEquals(1000f to 2000f, ZoomMath.fitSize(1000f, 2000f, 0f, 0f))
    }
}
