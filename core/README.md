# hushcore

Hush（密语）客户端核心库（Rust）。网络、端到端加密、本地存储，以及会话、消息、群、好友、朋友圈、通话信令这些业务逻辑都在这里实现一次，再通过 [UniFFI](https://mozilla.github.io/uniffi-rs/) 0.32（proc-macro 方式）生成 Kotlin（Android）和 Swift（iOS）绑定。两端原生代码只负责 UI 和平台能力。

- 加密：[vodozemac](https://github.com/matrix-org/vodozemac) 0.11 的 Olm（1:1）和 Megolm（群），SessionConfig 用 v2；附件与本地字段加密用 RustCrypto `aes-gcm`；安全码用 `sha2`。本库没有自己实现任何密码学原语，也没有参考 Signal 源码。
- 网络：`reqwest` + `tokio-tungstenite`。TLS 用 rustls，crypto provider 选 ring，根证书用 webpki-roots。不依赖 openssl / native-tls。
- 存储：`rusqlite`（bundled SQLite，WAL 模式），按 `PRAGMA user_version` 迁移（当前 v4）。消息正文、附件密钥、草稿、登录 token、收件箱中已解密的明文用库密钥做 AES-256-GCM 字段级加密。vodozemac pickle 用库密钥加密后存储。收到的信封先落收件箱再解密，棘轮状态与明文同一事务保存，进程被杀不丢消息。
- 附件：分块 AES-256-GCM（格式 v2：64 KiB 明文块，nonce = 8 字节随机前缀 ‖ 4 字节块序号，最后一块在 AAD 中标记 final），文件→文件流式加解密、流式上传下载，内存占用与附件大小无关；旧格式 v1（整文件 GCM）仍可解密。格式细节见 CORE_NOTES 第 1 节。
- 异步：库内有一个全局 tokio 多线程运行时。所有 `async` 导出函数都 spawn 到这个运行时上执行，调用方只等待结果。在 Kotlin 里是 `suspend fun`，在 Swift 里是 `async throws`。

设计与契约见 `docs/superpowers/specs/2026-09-24-hush-design.md`（第 3–5 节）。对契约的补充和已知限制见 [`CORE_NOTES.md`](CORE_NOTES.md)。

## 目录

```
src/
  lib.rs            UniFFI 导出：HushClient 对象（所有公开 API）
  types.rs          导出的 Record / Enum、CoreEvent、EventListener 回调接口
  error.rs          HushError
  content.rs        加密前明文 Content（设计 4.2）
  crypto/           olm.rs / megolm.rs / attachment.rs / fieldenc.rs / safety.rs
  store/            SQLite 表结构、迁移与读写（字段加密）：mod / sessions / social / messages / inbox（收件箱）
  api.rs            REST 客户端与服务端 JSON 结构
  ws.rs             WebSocket 连接（ws/wss）
  client/           业务实现：account / keys(密钥上传) / identity(身份与安全码) / secure(加密通道) /
                    messaging(发送) / receive(接收) / attachments / groups / social / calls / sync(WS 循环) / hooks(测试钩子)
tests/e2e.rs        起真实 Go 服务端的双/三客户端集成测试（自动启用 test-hooks 特性）
uniffi-bindgen.rs   绑定生成器入口
```

## API 概览（`HushClient`）

所有可能失败的方法都返回 `Result<_, HushError>`（Kotlin 抛 `HushException`，Swift 抛 `HushError`）。

| 分类 | 方法 |
|---|---|
| 构造 | `new(data_dir, db_key[32], platform, device_name)`（库密钥与已有库不符时返回 `KeyMismatch`）、`set_listener(EventListener)` |
| 账号 | `request_code(server_url, email)`、`verify_code(server_url, email, code) -> MyAccount`、`is_logged_in()`、`my_account()`、`logout()` |
| 连接 | `start()`（WS 循环 + OTK 补充 + 阅后即焚定时器；WS 任务已退出时会重新拉起）、`stop()`、`connection_state()`、`sync_now()`（HTTP 拉取一次） |
| 资料 | `update_profile(name?, about?, avatar_path?)`、`get_user`、`search_user_by_email`、`cached_profile`、`fetch_avatar(avatar_id) -> 本地路径` |
| 好友 | `send_friend_request`、`list_friend_requests`、`accept_friend_request`、`reject_friend_request`、`refresh_friends`、`friends`、`remove_friend`、`block`、`unblock`、`blocked`、`my_qr_payload` |
| 会话 | `conversations(archived)`、`conversation(id)`、`direct_conversation_id(user_id)`、`set_pinned` / `set_muted` / `set_archived`、`clear_conversation`、`delete_conversation`（v3）、`set_draft(conv_id, text?)`（v3） |
| 消息 | `messages(conv_id, before_ts?, limit)`（翻页游标为 `MessageItem.sort_ts`）、`search_messages`、`send_text(conv_id, text, quote_id?)`、`send_text_with_mentions(conv_id, text, quote_id?, mentions)`（v3）、`forward_message(message_id, conv_ids) -> [MessageItem]`（v3）、`send_attachment(conv_id, file_path, mime, kind, caption?, meta)`、`resend`、`download_attachment(message_id, index) -> 本地路径`、`react`、`delete_for_everyone`、`delete_local`、`mark_read`、`set_typing`、`set_disappearing` |
| 群 | `create_group`、`refresh_groups`、`groups`、`group_info`、`update_group`、`add_members`、`remove_member`、`leave_group`、`set_admin` |
| 安全码 | `safety_number(user_id) -> SafetyNumber`、`set_verified`、`verify_safety_qr(user_id, payload) -> bool` |
| 朋友圈 | `moments_feed(before?)`、`user_moments(uid, before?)`、`moment(id)`（不可见时 `NotFound`）、`post_moment(text, image_paths, visibility)`、`delete_moment`、`like_moment(id, like)`、`comment_moment(id, text, reply_to?)`、`delete_comment`、`moment_notifications`、`fetch_media(attachment_id) -> 本地路径` |
| 通话 | `turn_credentials()`、`send_call_signal(user_id, CallSignal)` |
| 设置 | `settings()`、`set_settings(Settings)` |

会话 id：单聊为 `u:<user_id>`，群聊为 `g:<group_id>`。

事件：平台层实现 `EventListener.on_event(CoreEvent)`，核心在后台线程回调。事件类型有 `ConnectionChanged`、`ConversationsChanged`、`MessageAdded`、`MessageUpdated`、`MessageRemoved`、`Typing`、`FriendRequestReceived`、`FriendsChanged`、`GroupsChanged`、`CallSignalReceived`、`MomentNotification`、`KeyChanged`、`LoggedOut`。回调**按发出顺序**在核心专门的事件线程 `hushcore-events` 上执行（不在任何内部锁内），平台层需要自己切换到 UI 线程。回调里抛出的异常会被核心兜住并记日志，不会终止连接或后台任务。`on_event` 仍应尽快返回：它阻塞期间后续事件会排队等待。

- `MessageAdded.backlog`：为 true 时是积压 / 补拉到的消息，平台不应弹实时横幅。
- `MessageAdded.mentions_me`（v3）：收到的消息提及了我；会话免打扰时平台也应通知。
- `LoggedOut.reason`：`logged_in_elsewhere`（4001，在别处登录）、`session_expired`（任何接口或 WS 握手 401，登录已过期）、或服务端 `logged_out` 帧带的原因。
- 接口 v2 的全部变化见 CORE_NOTES 第 11 节，v3（删除会话、转发、草稿、@提及、统一排序）见第 12 节。

### 典型用法（Kotlin）

```kotlin
val client = HushClient(filesDir.path + "/hush", dbKey /* 32 字节，Keystore 包裹 */, "android", Build.MODEL)
client.setListener(object : EventListener { override fun onEvent(event: CoreEvent) { /* 切到主线程刷新 UI */ } })
if (!client.isLoggedIn()) {
    client.requestCode("http://10.0.2.2:8080", email)
    client.verifyCode("http://10.0.2.2:8080", email, code)
    client.listFriendRequests()          // 登录后全量同步（服务端会清空旧队列）
}
client.start()
client.sendText(client.directConversationId(friendId), "你好", null)
```

## 构建

宿主机开发与测试用 PATH 里的 cargo（Homebrew）即可：

```bash
cd core
# 建议用独立的目标目录，避免替换正在运行的 bot 等可执行文件
export CARGO_TARGET_DIR=/tmp/hushcore-target
cargo test                    # 单元测试 + e2e（自动 go build ../server 并以随机端口启动）
cargo test --lib              # 只跑单元测试
cargo test --test e2e -- --nocapture   # 查看 e2e 每一步的输出
```

e2e 测试需要本机装有 `go`，并且存在 `../server/cmd/hush-server`。两者缺一时测试会打印原因并跳过。

### 交叉编译

交叉编译要用 rustup 工具链（Homebrew 的 rust 只带宿主 target）。rustup 的 `stable` 工具链已经装好 4 个 target。**注意**：本机 `~/.cargo/bin` 下没有 rustup 代理，所以 `rustup run stable cargo` 仍可能调到 Homebrew 的 `rustc`，报 `can't find crate for core`。需要显式指定 `RUSTC`：

```bash
RU=/opt/homebrew/opt/rustup/bin/rustup
export RUSTC="$($RU which rustc)"
CARGO="$($RU which cargo)"

# iOS
IPHONEOS_DEPLOYMENT_TARGET=17.0 $CARGO build --release --lib --target aarch64-apple-ios-sim
IPHONEOS_DEPLOYMENT_TARGET=17.0 $CARGO build --release --lib --target aarch64-apple-ios

# Android（需要 NDK 与 cargo-ndk）
export PATH="$(dirname "$CARGO"):$HOME/.cargo/bin:$PATH"
export ANDROID_NDK_HOME=$HOME/Library/Android/sdk/ndk/30.0.16248370
cargo ndk -t arm64-v8a -t x86_64 -P 26 -o ../android/app/src/main/jniLibs build --release --lib
```

仓库里的 `scripts/build-core-ios.sh` 和 `scripts/build-core-android.sh` 会完成上面这些步骤，并生成 xcframework 与绑定。

### 生成绑定

```bash
cargo build --lib
cargo run --bin uniffi-bindgen generate --library target/debug/libhushcore.dylib --language kotlin --out-dir out/kotlin
cargo run --bin uniffi-bindgen generate --library target/debug/libhushcore.dylib --language swift  --out-dir out/swift
```

Kotlin 包名使用默认的 `uniffi.hushcore`。Swift 会输出 `hushcore.swift`、`hushcoreFFI.h` 和 `hushcoreFFI.modulemap`。
