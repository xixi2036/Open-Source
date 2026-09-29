package com.hush.app.ui.auth

import com.hush.app.util.net

import android.app.Application
import android.net.Uri
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.hush.app.core.HushCore
import com.hush.app.core.ContactsStore
import com.hush.app.util.MediaUtils
import com.hush.app.util.userMessage
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

enum class AuthStep { WELCOME, SERVER, EMAIL, CODE, PROFILE }

data class AuthState(
    val step: AuthStep = AuthStep.WELCOME,
    val server: String = HushCore.prefs.lastServerUrl,
    val email: String = "",
    val code: String = "",
    val name: String = "",
    val avatarPath: String? = null,
    val busy: Boolean = false,
    val error: String? = null,
    val resendIn: Int = 0,
)

/** 登录流程：服务器地址 → 邮箱 → 验证码 → 资料。 */
class AuthViewModel(app: Application) : AndroidViewModel(app) {
    private val _state = MutableStateFlow(AuthState())
    val state: StateFlow<AuthState> = _state
    private var countdown: Job? = null

    fun go(step: AuthStep) = _state.update { it.copy(step = step, error = null) }

    fun back(): Boolean {
        val prev = when (_state.value.step) {
            AuthStep.WELCOME, AuthStep.PROFILE -> return false
            AuthStep.SERVER -> AuthStep.WELCOME
            AuthStep.EMAIL -> AuthStep.SERVER
            AuthStep.CODE -> AuthStep.EMAIL
        }
        go(prev); return true
    }

    fun setServer(v: String) = _state.update { it.copy(server = v, error = null) }
    fun setEmail(v: String) = _state.update { it.copy(email = v, error = null) }
    fun setName(v: String) = _state.update { it.copy(name = v, error = null) }

    fun submitServer() {
        val url = _state.value.server.trim().trimEnd('/')
        if (!url.startsWith("http://") && !url.startsWith("https://")) {
            _state.update { it.copy(error = "地址需以 http:// 或 https:// 开头") }; return
        }
        HushCore.prefs.lastServerUrl = url
        _state.update { it.copy(server = url, step = AuthStep.EMAIL, error = null) }
    }

    fun requestCode() {
        val s = _state.value
        val email = s.email.trim().lowercase()
        if (!Regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$").matches(email)) {
            _state.update { it.copy(error = "请输入有效的邮箱") }; return
        }
        _state.update { it.copy(busy = true, error = null, email = email) }
        viewModelScope.launch {
            try {
                withContext(Dispatchers.IO) { net { HushCore.client.requestCode(s.server, email) } }
                _state.update { it.copy(busy = false, step = AuthStep.CODE, code = "") }
                startCountdown()
            } catch (e: Exception) {
                _state.update { it.copy(busy = false, error = e.userMessage()) }
            }
        }
    }

    private fun startCountdown() {
        countdown?.cancel()
        countdown = viewModelScope.launch {
            for (i in 60 downTo 0) {
                _state.update { it.copy(resendIn = i) }
                if (i > 0) delay(1000)
            }
        }
    }

    fun setCode(v: String) {
        val digits = v.filter(Char::isDigit).take(6)
        _state.update { it.copy(code = digits, error = null) }
        if (digits.length == 6 && !_state.value.busy) verify()
    }

    private fun verify() {
        val s = _state.value
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            try {
                val acct = withContext(Dispatchers.IO) { HushCore.client.verifyCode(s.server, s.email, s.code) }
                if (acct.isNew || acct.profile.name.isBlank()) {
                    _state.update { it.copy(busy = false, step = AuthStep.PROFILE, name = acct.profile.name) }
                } else {
                    finish()
                }
            } catch (e: Exception) {
                _state.update { it.copy(busy = false, error = e.userMessage(), code = "") }
            }
        }
    }

    fun pickAvatar(uri: Uri) {
        viewModelScope.launch(Dispatchers.IO) {
            runCatching {
                val small = MediaUtils.prepareAvatar(getApplication(), uri)
                val old = _state.value.avatarPath
                _state.update { it.copy(avatarPath = small.path) }
                old?.let { MediaUtils.deleteTemp(java.io.File(it)) }
            }
        }
    }

    fun submitProfile() {
        val s = _state.value
        if (s.name.isBlank()) {
            _state.update { it.copy(error = "请填写昵称") }; return
        }
        _state.update { it.copy(busy = true, error = null) }
        viewModelScope.launch {
            try {
                val p = withContext(Dispatchers.IO) { net { HushCore.client.updateProfile(s.name.trim(), null, s.avatarPath) } }
                s.avatarPath?.let { MediaUtils.deleteTemp(java.io.File(it)) }
                ContactsStore.put(p)
                HushCore.refreshAccount()
                finish()
            } catch (e: Exception) {
                _state.update { it.copy(busy = false, error = e.userMessage()) }
            }
        }
    }

    /** 登录完成：刷新账号后 HushRoot 切到主界面，并由它统一调用 startSession()。 */
    private fun finish() {
        HushCore.refreshAccount()
        _state.update { it.copy(busy = false) }
    }
}
