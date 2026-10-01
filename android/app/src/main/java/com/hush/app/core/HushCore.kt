package com.hush.app.core

import com.hush.app.util.MediaUtils
import com.hush.app.util.net

import android.content.Context
import android.os.Build
import com.hush.app.util.HushLog as Log
import android.util.Log as AndroidLog
import com.hush.app.BuildConfig
import uniffi.hushcore.LogLevel
import uniffi.hushcore.LogListener
import uniffi.hushcore.setLogListener
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import uniffi.hushcore.ConnectionState
import uniffi.hushcore.CoreEvent
import uniffi.hushcore.EventListener
import uniffi.hushcore.HushClient
import uniffi.hushcore.MyAccount
import java.io.File

/**
 * 核心库单例：持有 [HushClient]，把核心事件转成 [SharedFlow]，并维护登录态与连接状态。
 * 业务数据（好友、资料、媒体缓存、朋友圈通知）拆在 [ContactsStore]、[MediaCache]、[MomentNotifStore]。
 */
object HushCore {
    private const val TAG = "HushCore"

    lateinit var client: HushClient
        private set
    lateinit var prefs: AppPrefs
        private set
    lateinit var appContext: Context
        private set

    /** 应用级协程作用域（进程存活期间）。 */
    val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // B13：缓冲满时不再悄悄丢最旧的事件。tryEmit 失败（最慢的订阅者积压了 1024 条）时记下“有事件丢失”，
    // 由 [resync] 通知各页面从核心重新读取当前状态自愈。
    private val _events = MutableSharedFlow<CoreEvent>(extraBufferCapacity = 1024, onBufferOverflow = BufferOverflow.SUSPEND)
    val events: SharedFlow<CoreEvent> = _events

    private val eventsLost = java.util.concurrent.atomic.AtomicBoolean(false)
    private val _resync = MutableStateFlow(0)
    /** 每次递增表示“事件流丢过事件”：订阅方应从核心重新加载（会话列表、聊天页最新一页、好友与申请）。 */
    val resync: StateFlow<Int> = _resync

    private suspend fun resyncLoop() {
        while (true) {
            kotlinx.coroutines.delay(1000)
            if (eventsLost.compareAndSet(true, false)) {
                Log.w(TAG, "event bus overflowed, asking screens to resync")
                _resync.value++
                if (isLoggedIn) {
                    scope.launch(Dispatchers.IO) { runCatching { ContactsStore.reloadLocalFriends() } }
                    scope.launch(Dispatchers.IO) { runCatching { ContactsStore.refreshRequests() } }
                }
            }
        }
    }

    private val _connection = MutableStateFlow(ConnectionState.DISCONNECTED)
    val connection: StateFlow<ConnectionState> = _connection

    private val _account = MutableStateFlow<MyAccount?>(null)
    val account: StateFlow<MyAccount?> = _account

    /** 被动登出原因（被踢下线等），UI 据此回到欢迎页。 */
    private val _loggedOut = MutableSharedFlow<String>(extraBufferCapacity = 4)
    val loggedOut: SharedFlow<String> = _loggedOut

    /** 当前在前台打开的会话 id（用于通知抑制与 mark_read）。 */
    @Volatile var activeConversationId: String? = null

    /** 应用是否在前台。 */
    @Volatile var appInForeground: Boolean = false

    @Volatile private var started = false
    /** 已为哪个账号做过登录后的全量刷新（好友、申请、群、朋友圈通知）。 */
    private val sessionRefreshedFor = java.util.concurrent.atomic.AtomicReference<String?>(null)
    @Volatile private var lastConnChange = 0L

    val myId: String get() = _account.value?.userId ?: ""

    private val _ready = MutableStateFlow(false)
    /**
     * 本地数据库已打开、[client] 可用。数据库在后台线程打开（Keystore 解包密钥 + SQLCipher 打开/迁移可能耗时数百毫秒），
     * 打开前界面显示启动页，前台服务、通知等组件先 [awaitReady]。
     */
    val ready: StateFlow<Boolean> = _ready

    private val _openError = MutableStateFlow<Throwable?>(null)
    /** 打开本地数据失败（启动页显示错误与重试）。 */
    val openError: StateFlow<Throwable?> = _openError

    suspend fun awaitReady() {
        _ready.first { it }
    }

    /** 主线程只做轻量初始化；打开数据库在 IO 线程进行。 */
    fun init(context: Context) {
        appContext = context.applicationContext
        prefs = AppPrefs(appContext)
        scope.launch { dispatchSideEffects() }
        scope.launch { connectionWatchdog() }
        scope.launch { fallbackPoller() }
        scope.launch { resyncLoop() }
        NetworkMonitor.start(appContext) { onNetworkAvailable() }
        openInBackground()
    }

    /** 启动时与启动页“重试”调用。 */
    fun openInBackground() {
        if (_ready.value) return
        _openError.value = null
        scope.launch(Dispatchers.IO) {
            try {
                open()
            } catch (t: Throwable) {
                Log.w(TAG, "open local data failed", t)
                _openError.value = t
            }
        }
    }

    @Synchronized private fun open() {
        if (_ready.value) return
        installCoreLogging()
        val dir = File(appContext.filesDir, "hush")
        val key = DbKeyStore.loadOrCreate(appContext)
        client = openClient(dir, key)
        client.setListener(object : EventListener {
            override fun onEvent(event: CoreEvent) {
                // 必须立即返回且不能抛异常：只做非阻塞投递
                try {
                    if (event is CoreEvent.ConnectionChanged) {
                        _connection.value = event.state
                        lastConnChange = System.currentTimeMillis()
                        if (event.state == ConnectionState.DISCONNECTED) recordDrop(lastConnChange)
                    }
                    if (!_events.tryEmit(event)) {
                        eventsLost.set(true)
                        // 被动登出与通话信令不能丢：改为挂起投递（不阻塞核心的回调线程）
                        if (event is CoreEvent.LoggedOut || event is CoreEvent.CallSignalReceived) scope.launch { _events.emit(event) }
                    }
                } catch (t: Throwable) {
                    Log.w(TAG, "onEvent", t)
                }
            }
        })
        if (client.isLoggedIn()) {
            _account.value = client.myAccount()
        }
        migrateLegacyHidden()
        _ready.value = true
    }

    /**
     * 打开核心库。只有核心明确报告“密钥与已有数据库不匹配”时才重置，而且是把旧目录改名备份，
     * 不删除；其他异常（磁盘满、IO、迁移失败等）短暂重试后抛出，宁可本次启动失败也不清空身份密钥与聊天记录。
     */
    private fun openClient(dir: File, key: ByteArray): HushClient {
        val device = Build.MODEL ?: "Android"
        var last: Exception? = null
        repeat(OPEN_ATTEMPTS) { attempt ->
            try {
                return HushClient(dir.path, key, "android", device)
            } catch (e: Exception) {
                last = e
                if (CoreOpen.isKeyMismatch(e)) {
                    Log.w(TAG, "db key mismatch, moving old data aside", e)
                    val backup = CoreOpen.backupDir(dir, System.currentTimeMillis())
                    check(dir.renameTo(backup)) { "无法备份旧数据目录" }
                    return HushClient(dir.path, key, "android", device)
                }
                Log.w(TAG, "open core failed (attempt ${attempt + 1})", e)
                if (attempt < OPEN_ATTEMPTS - 1) Thread.sleep(300)
            }
        }
        throw IllegalStateException("无法打开本地数据库", last)
    }

    private const val OPEN_ATTEMPTS = 3

    /**
     * 核心库日志转到 Logcat（tag "hushcore"）。核心保证日志里不含消息明文、密钥与 token；
     * debug 构建转发全部级别，release 只转发 warn 与 error。
     */
    private fun installCoreLogging() {
        setLogListener(object : LogListener {
            override fun onLog(level: LogLevel, module: String, message: String) {
                val text = "[$module] $message"
                when (level) {
                    LogLevel.ERROR -> AndroidLog.e("hushcore", text)
                    LogLevel.WARN -> AndroidLog.w("hushcore", text)
                    LogLevel.INFO -> if (BuildConfig.DEBUG) AndroidLog.i("hushcore", text)
                    LogLevel.DEBUG -> if (BuildConfig.DEBUG) AndroidLog.d("hushcore", text)
                }
            }
        })
    }

    /**
     * U1 迁移：v3 之前“删除”的会话只是被 App 隐藏（消息已清空、会话行还在）。
     * 旧隐藏列表中仍然没有任何消息的会话改由核心真正删除；期间已有新消息的会话本来就会重新显示，保持不动。
     */
    private fun migrateLegacyHidden() {
        val hidden = prefs.takeLegacyHiddenConversations()
        for (id in hidden) {
            runCatching {
                val c = client.conversation(id) ?: return@runCatching
                if (c.lastMessage == null && c.unreadCount == 0u) client.deleteConversation(id)
            }.onFailure { Log.w(TAG, "migrate hidden conversation", it) }
        }
    }

    val isLoggedIn: Boolean get() = _account.value != null

    /**
     * 登录/启动后：全量同步 + 启动 WS 循环。
     * WS 连上后会自动补拉积压；回前台且未连接时由 [onForeground] 用 syncNow() 补拉。
     * 同一 data_dir 只能有一个 HushClient：Activity 与前台服务共用本单例。
     */
    fun startSession() {
        val acct = client.myAccount()
        _account.value = acct
        scope.launch(Dispatchers.IO) {
            if (!started) {
                started = true
                runCatching { client.start() }.onFailure { started = false; Log.w(TAG, "start", it) }
            }
            // HushRoot 与 ConnectionService 都会调用本方法：同一账号只做一次全量刷新
            val uid = acct?.userId ?: return@launch
            if (sessionRefreshedFor.getAndSet(uid) == uid) return@launch
            launch { runCatching { ContactsStore.refreshRequests() } }
            launch { runCatching { ContactsStore.refreshFriends() } }
            launch { runCatching { net { client.refreshGroups() } } }
            launch { runCatching { MomentNotifStore.refresh() } }
        }
    }

    /**
     * 连接看门狗：核心在被同账号新连接替换（4000）两次后会保持登录但停止 WS 循环，
     * 此时状态长时间停在 Disconnected（正常重连退避最长 30 秒会进入 Connecting），需要重新 start()。
     */
    private suspend fun connectionWatchdog() {
        while (true) {
            kotlinx.coroutines.delay(10_000)
            val idle = System.currentTimeMillis() - lastConnChange
            // B2：不再要求应用在前台（后台被挤下线后只有这里能把 WS 循环拉起来）
            if (ConnectionPolicy.shouldRestart(isLoggedIn, started, _connection.value == ConnectionState.DISCONNECTED, idle, NetworkMonitor.online.value)) {
                Log.i(TAG, "watchdog: restart connection")
                lastConnChange = System.currentTimeMillis()
                runCatching { client.start() }
            }
        }
    }

    /**
     * 系统网络恢复 / 切换（B2）：立即确保 WS 循环在运行并用 HTTP 补拉一次积压。
     * 设备可能正要休眠，用一个限时 WakeLock 保证这次补拉能跑完（B3）。
     */
    private fun onNetworkAvailable() {
        if (!::client.isInitialized || !isLoggedIn || !started) return
        scope.launch(Dispatchers.IO) {
            KeepAlive.withWakeLock(appContext, "net-restore", 15_000) {
                lastConnChange = System.currentTimeMillis()
                runCatching { client.start() }
                runCatching { client.syncNow() }
            }
        }
    }

    /** 通话建立期间置为 true：信令对时延敏感，WS 断线期间用 HTTP 高频补拉。 */
    @Volatile var fastSync: Boolean = false

    private val drops = ArrayDeque<Long>()

    @Synchronized private fun recordDrop(ts: Long) {
        drops.addLast(ts)
        while (drops.size > 10) drops.removeFirst()
    }

    /** 最近 2 分钟内 WS 掉线 ≥2 次视为网络不稳定（此时 WS 可能处于“假连接”状态）。 */
    @Synchronized private fun networkUnstable(now: Long): Boolean = drops.count { now - it < 120_000 } >= 2

    /**
     * HTTP 兜底补拉：通话建立期间每 1.5 秒一次；前台且 WS 未连接时每 5 秒一次；
     * 前台且网络不稳定（WS 频繁掉线，可能处于假连接）时每 3 秒一次；后台且 WS 未连接时每 30 秒一次（B2）。
     * （模拟器网络会回收空闲连接，WS 周期性掉线，见 docs/issues/android.md 第 6 条。）
     */
    private suspend fun fallbackPoller() {
        var last = 0L
        while (true) {
            kotlinx.coroutines.delay(1500)
            if (!isLoggedIn || !started) continue
            val now = System.currentTimeMillis()
            val due = ConnectionPolicy.pollDue(
                fastSync = fastSync,
                foreground = appInForeground,
                connected = _connection.value == ConnectionState.CONNECTED,
                unstable = networkUnstable(now),
                online = NetworkMonitor.online.value,
                sinceLastMs = now - last,
            )
            if (due) {
                last = now
                runCatching { client.syncNow() }
            }
        }
    }

    /** 回到前台：未连接时用 HTTP 立即补拉一次积压（核心修复后可与 WS 并发）。 */
    fun onForeground() {
        if (!isLoggedIn || _connection.value == ConnectionState.CONNECTED) return
        scope.launch(Dispatchers.IO) { runCatching { client.syncNow() } }
    }

    fun refreshAccount() {
        _account.value = client.myAccount()
    }

    suspend fun logout() = withContext(Dispatchers.IO) {
        runCatching { client.logout() }
        clearLocalState()
    }

    private fun clearLocalState() {
        _account.value = null
        started = false
        sessionRefreshedFor.set(null)
        activeConversationId = null
        prefs.clear()
        ContactsStore.clear()
        MomentNotifStore.clear()
        MediaCache.clear()
        com.hush.app.service.Notifier.cancelAllMessages(appContext)
        // 发送用的临时明文副本（cacheDir/outgoing、voice）
        MediaUtils.clearTemp(appContext)
    }

    private suspend fun dispatchSideEffects() {
        events.collect { ev ->
            when (ev) {
                is CoreEvent.FriendRequestReceived -> scope.launch(Dispatchers.IO) { runCatching { ContactsStore.refreshRequests() } }
                is CoreEvent.FriendsChanged -> scope.launch(Dispatchers.IO) { runCatching { ContactsStore.reloadLocalFriends() } }
                is CoreEvent.MomentNotification -> MomentNotifStore.onNew(ev.notification)
                is CoreEvent.LoggedOut -> {
                    clearLocalState()
                    _loggedOut.tryEmit(ev.reason)
                }
                else -> Unit
            }
        }
    }
}
