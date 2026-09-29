package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Test

class GallerySaverTest {
    /** A16：按真实格式保存，GIF / PNG 不再一律存成 image/jpeg + .jpg。 */
    @Test
    fun keepsRealFormat() {
        assertEquals("image/gif" to "gif", GallerySaver.fileType("image/gif"))
        assertEquals("image/png" to "png", GallerySaver.fileType("image/png"))
        assertEquals("image/webp" to "webp", GallerySaver.fileType("image/webp"))
        assertEquals("image/jpeg" to "jpg", GallerySaver.fileType("image/jpeg"))
        assertEquals("image/jpeg" to "jpg", GallerySaver.fileType(null))
    }
}
