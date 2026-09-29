package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ConvIdTest {
    @Test
    fun parseAndBuild() {
        assertEquals("abc", ConvId.peerOf("u:abc"))
        assertNull(ConvId.groupOf("u:abc"))
        assertEquals("g1", ConvId.groupOf("g:g1"))
        assertNull(ConvId.peerOf("g:g1"))
        assertNull(ConvId.peerOf("x"))
        assertEquals("u:abc", ConvId.direct("abc"))
        assertEquals("g:g1", ConvId.group("g1"))
    }
}
