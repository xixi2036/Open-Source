package com.hush.app.util

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.cancel
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotSame
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

/** C2：多次写入合并为一次发射；刚写入就能读到。 */
class BatchedMapFlowTest {
    @Test
    fun batchesWritesIntoOneEmission() {
        val scope = CoroutineScope(Dispatchers.Unconfined)
        // 时间窗设得很长：只靠手动 flush，验证合并语义
        val m = BatchedMapFlow<String, String>(scope, windowMs = 60_000)
        val before = m.flow.value
        repeat(200) { m.put("k$it", "v$it") }
        assertSame("时间窗内不发射", before, m.flow.value)
        assertEquals("待合并区可读", "v7", m["k7"])
        assertTrue(m.containsKey("k199"))
        m.flush()
        assertEquals(200, m.flow.value.size)
        val after = m.flow.value
        m.put("k1", "v1")
        m.flush()
        assertSame("值未变化不产生新表", after, m.flow.value)
        m.putAll(mapOf("k1" to "x", "new" to "y"))
        assertNotSame(after, m.flow.value)
        assertEquals("x", m.flow.value["k1"])
        assertEquals(201, m.flow.value.size)
        m.clear()
        assertTrue(m.flow.value.isEmpty())
        assertEquals(null, m["k1"])
        scope.cancel()
    }
}
