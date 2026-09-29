# Android 客户端发现的问题（核心库 / 服务端 / 环境）

## 1. 生成的 Kotlin 绑定无法编译：HushException 变体的 `message` 重复声明（核心库）（已解决：scripts/build-core-android.sh 自动修补）

- 复现：`bash scripts/build-core-android.sh` 生成 `android/app/src/main/java/uniffi/hushcore/hushcore.kt` 后 `./gradlew assembleDebug`。
- 现象：Kotlin 2.4（K2）报 `REDECLARATION: Conflicting declarations: val message: String`（hushcore.kt 约 5732 行）。
  UniFFI 为 `HushError` 的每个带 `message` 字段的变体生成了构造属性 `val message: String`，同时又生成
  `override val message get() = "message=..."`，两者同名冲突。
- 原因：错误枚举字段名 `message` 与 `kotlin.Exception.message` 冲突（UniFFI 已知问题）。
- 建议修复（核心侧）：把 `core/src/error.rs` 中各变体字段改名（如 `msg` / `detail`），或对 `HushError` 使用 `#[uniffi(flat_error)]`。
- App 侧规避：`android/app/build.gradle.kts` 中的 `patchUniffiBindings` 任务在每次构建前幂等地把构造参数改为
  `override val message` 并删去重复 getter；绑定重新生成后会自动再次修补，无需手工处理。

## 2. `MessageItem` 未暴露分页键 `sort_ts`（核心库，API 小缺口）（已在接口 v2 解决：C-A / P-A）

- `messages(conv_id, before_ts, limit)` 的 `before_ts` 与内部 `sort_ts` 比较；`sort_ts` 对收到的消息是 `server_ts`，
  对发出的消息是 `sent_at`（content.ts），系统消息为插入时刻。`MessageItem` 只有 `sent_at` / `server_ts`。
- App 侧规避：分页键取 `if (!isOutgoing && serverTs > 0) serverTs else sentAt`。当多条消息 `sort_ts` 完全相同时，
  严格小于比较可能跳过同一毫秒的消息（极少见）。
- 建议：在 `MessageItem` 中增加 `sort_ts` 字段，或让 `messages` 接受“最早一条消息 id”作为游标。

## 3. `Profile` 不含邮箱（核心库 / 契约）（已在接口 v2 解决：S-B / C-C / P-C）

- ui-guide 2.4 要求单聊设置页显示“大头像 + 名称 + 邮箱”，但 `Profile = {id, name, about, avatar_id, updated_at}` 没有邮箱，
  除了本人（`MyAccount.email`）外拿不到对方邮箱。App 侧改为显示对方签名（about）。

## 4. 已修复（核心）：`syncNow()` 与 WS 并发处理积压可能重复处理同一信封

- 核心修复版已重建。App 侧：`start()` 在进程内只调用一次（`HushCore.startSession` 内有 started 标记）；
  回前台且未连接时调用 `syncNow()` 补拉；`onEvent` 回调整体 try/catch、只做 `tryEmit`，不阻塞、不抛异常。
- 核心在被同账号新连接替换（4000）两次后保持登录但停止 WS 循环：App 的连接看门狗在前台、已登录且连续 45 秒处于
  Disconnected 时重新调用 `start()`（正常重连退避最长 30 秒必然经过 Connecting，不会误触发）。

## 5. 环境：`emulator-5554` 不是 Pixel_API36（环境）

- 任务说明要求使用 `adb -s emulator-5554`（AVD Pixel_API36）。实际检查时 Pixel_API36 并未运行，`emulator-5554`
  对应的是 MuMu 模拟器实例（型号 PGEM10，Android 12 / SDK 32，监听 5555 端口，adb 因此把它标成 emulator-5554）。
- 处理：为避免干扰该设备上的其他工作，另行以 `-ports 5580,5581` 启动 AVD Pixel_API36（Android 16 / SDK 36，arm64），
  序列号为 `emulator-5580`，全部安装、验收与截图均在该设备上完成。

## 6. （核心已将心跳改为 10 秒、连接池空闲 8 秒）模拟器网络：空闲约 20–25 秒的 TCP 连接会被断开，导致 WS 每 25 秒掉线、HTTP 连接池首个请求失败（环境 + 核心参数）

- 现象：服务端日志中模拟器客户端（alice、carol）的 WS 每次恰好在 25.5 秒（或其整数倍）时“下线”，随后 2–20 秒重连；
  同期 App 调用 HTTP 接口时偶发 `HushException.Network: error sending request for url (...)`，立即重试即成功。
  宿主机上运行的机器人 WS 可稳定保持 1 小时以上。
- 复现（与 Hush 无关的纯 TCP 测试）：宿主机起一个回显服务，模拟器内执行
  `(echo a; sleep 26; echo b; sleep 3) | nc 10.0.2.2 <port>`：空闲 15s / 20s 正常回显，空闲 26s 后再发送，连接立刻被关闭。
  说明模拟器到宿主机的 TCP 在空闲约 20–25 秒后被网络路径（模拟器 NAT / 宿主机代理 TUN，抓包中可见 198.18.x.x fake-ip）回收。
- 与核心参数的关系：核心 WS 心跳间隔 `PING_INTERVAL = 25s` 恰好超过该阈值，每次心跳都撞上已被回收的连接；
  reqwest `pool_idle_timeout(60s)` 使空闲 20–60 秒的旧连接被复用后失败。
- 更严重的情形：被回收的旧连接有时不是立即报错，而是请求一直挂起到超时（约 30 秒）。从服务端日志观察到：一次视频呼叫的
  OFFER、ICE、HANGUP 三条信令在发起约 30 秒后于 0.6 秒内先后到达（推测各请求都拿到了被回收的连接、同时卡到超时后才重连发出；
  已确认核心 `send_olm_batch` 在 HTTP 发送前就释放了 `crypto_lock`，并非锁导致的串行）——对方因此只看到一个“未接来电”。
  双模拟器（alice ↔ carol）实测：语音通话成功接通并通话 1 分钟（截图 44/45/46），但在网络回收连接后几次视频通话因此未接通。
- 建议（核心）：心跳间隔改为 ≤15 秒；HTTP 连接池空闲超时 ≤15 秒（`pool_idle_timeout`），为 JSON 请求设置较短的
  超时（如 10 秒）并对幂等请求在连接级错误时自动重试一次；避免请求长时间挂在失效连接上。
- App 侧规避：`util/Net.kt` 的 `net { }` 对幂等的网络调用（搜索、好友申请处理、点赞/删除、群成员与群资料修改、资料、附件与媒体下载、TURN、通话信令等；
  建群、发动态、发评论等非幂等请求不重试，避免重复创建）
  在 `Network` 错误时 300ms 后重试一次；WS 掉线期间的消息在重连后自动补齐，回前台未连接时调用 `syncNow()` 补拉；
  通话建立期间每 1.5 秒、WS 频繁掉线时每 3 秒用 `syncNow()` 兜底拉取；TURN 凭证登录后预取并缓存（按 ttl），发起通话不再等网络。

## 7. 验证过程中发现并已修复的 App 问题（记录）

- 新用户验证码通过后直接进入主界面、跳过资料设置页：`verify()` 过早刷新账号导致根界面切换。已修复（资料页截图 06）。
- 非幂等请求（建群）在网络挂起后被重试，产生了一个重复的群；已将建群/发动态/发评论移出重试范围。

## 8. 核心库没有“删除会话”接口（待办）

- 会话列表的「删除」目前只能清空消息，App 侧在本地隐藏该会话行，收到新消息时再出现。建议核心新增 `delete_conversation(conv_id)`。

## 9. 发出消息的排序依赖本机时钟（已知）

- 本端发出消息的 `sort_ts` 为发送时的本机时间，收到的消息为服务端时间；设备时钟与服务端偏差较大时，双方消息的相对顺序可能轻微错位。

## 10. 正式版签名

- `android/keystore.properties`（不入库，已加入 .gitignore）提供 `storeFile/storePassword/keyAlias/keyPassword` 时 release 用它签名，否则回退 debug 签名。R8 keep 规则见 `android/app/proguard-rules.pro`。
