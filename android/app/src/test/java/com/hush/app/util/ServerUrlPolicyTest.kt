package com.hush.app.util

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** B1：正式版只对回环与私有网段放行明文，其余强制 https。 */
class ServerUrlPolicyTest {
    private fun ok(url: String, debug: Boolean = false) = ServerUrlPolicy.check(url, debug) is ServerUrlPolicy.Result.Ok

    @Test
    fun httpsAlwaysAllowed() {
        assertTrue(ok("https://hush.example.com"))
        assertTrue(ok("https://8.8.8.8:8443"))
        assertTrue(ok("HTTPS://Hush.Example.com/"))
    }

    @Test
    fun cleartextOnlyForLoopbackAndPrivateRanges() {
        for (u in listOf(
            "http://localhost:8080", "http://127.0.0.1:8080", "http://127.8.9.1", "http://10.0.2.2:8091", "http://10.255.255.255",
            "http://172.16.0.1", "http://172.31.255.254:8080", "http://192.168.1.10:8080", "http://169.254.1.1",
            "http://[::1]:8080", "http://[fd12:3456::1]:8080", "http://[fe80::1]:8080", "http://nas.local:8080", "http://Hush-Server.local",
        )) assertTrue(u, ok(u))
        for (u in listOf(
            "http://hush.example.com", "http://8.8.8.8:8080", "http://172.15.0.1", "http://172.32.0.1", "http://192.169.1.1",
            "http://11.0.0.1", "http://[2001:db8::1]:8080", "http://localhost.evil.com", "http://10.0.0.1.evil.com",
            "http://127.1", "http://0x7f.0.0.1", "http://1270.0.0.1", "http://nas.local.evil.com", "http://local",
        )) assertFalse(u, ok(u))
    }

    @Test
    fun debugKeepsAnyCleartext() {
        assertTrue(ok("http://hush.example.com", debug = true))
        assertTrue(ok("http://10.0.2.2:8080", debug = true))
    }

    @Test
    fun malformed() {
        assertFalse(ok("hush.example.com"))
        assertFalse(ok("ftp://10.0.0.1"))
        assertFalse(ok("http://"))
        assertFalse(ok("https://user:pw@hush.example.com"))
        assertFalse(ok(""))
    }

    @Test
    fun normalizesTrailingSlashAndSpaces() {
        assertEquals(ServerUrlPolicy.Result.Ok("http://10.0.2.2:8091"), ServerUrlPolicy.check("  http://10.0.2.2:8091/ ", false))
    }
}
