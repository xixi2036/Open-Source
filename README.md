# Hush（密语）

自研的端到端加密社交 App：**Signal 级加密聊天 + 微信式朋友圈**。包括本地部署的 Go 服务端、Android 原生客户端（Kotlin/Compose）和 iOS 原生客户端（Swift/SwiftUI）。两端共用一个 Rust 核心库，网络、加密、存储和业务逻辑都只写一遍。

> clean-room 自研：只参考了 Signal 的公开功能和公开架构思路，没有复制或改写 Signal 的源码。
> 端到端加密用的是 [vodozemac](https://github.com/matrix-org/vodozemac)（Apache-2.0，Olm/Megolm）。设计依据见 [设计文档](docs/superpowers/specs/2026-09-24-hush-design.md)。

```
Android (Kotlin + Compose)        iOS (Swift + SwiftUI)        ← UI、通知、相机麦克风、WebRTC
            └──────────── hushcore（Rust，UniFFI）────────────┘  ← 网络、Olm/Megolm、SQLite、全部业务
                                   │ HTTP + WebSocket
                       hush-server（Go，SQLite，内嵌 TURN）       ← 账号、预密钥、消息中继、好友/群、朋友圈
```

| 目录 | 内容 | 规模 |
|---|---|---|
| `server/` | Go 服务端，共 44 个接口，`go test -race` 全绿 | 约 7.3k 行 |
| `core/` | Rust 核心库：50 个单元测试，外加 3 组起真实服务端的端到端测试 | 约 8.1k 行 |
| `android/` | Android App，minSdk 26 | 约 14.6k 行 |
| `ios/` | iOS App（iOS 17+）和 XCUITest 验收测试（A01–A08 单端流程；A09 为跨平台用例，需 Android 端配合，运行时加环境变量 `TEST_RUNNER_HUSH_CROSS=1`） | 约 6.3k 行 |
| `docs/` | 设计文档、UI 规范、问题记录、验收截图（Android 61 张、iOS 57 张、跨平台 7 张） | |

## 本地运行

前置条件：Go 1.26、Rust（rustup，含 4 个移动端 target）、Android SDK + NDK r30、Xcode 27、XcodeGen。

```bash
# 1. 启动服务端（监听 127.0.0.1:8080，开发模式验证码固定 123456，数据在 server/data）
bash scripts/run-server.sh
```

```bash
# 2.（可选）启动测试机器人：真实加密客户端，会自动同意好友申请，发「帮助」查看指令
cd core && cargo run --example bot -- --email bot@hush.local --name 小助手
```

```bash
# 3. 编译核心库并生成绑定（第一次运行，或核心库有改动时）
bash scripts/build-core-android.sh
```

```bash
bash scripts/build-core-ios.sh
```

```bash
# 4. Android：构建并安装到模拟器（App 内服务器地址填 http://10.0.2.2:8080）
cd android && ./gradlew installDebug
```

```bash
# 5. iOS：生成工程后用 Xcode 打开 ios/Hush.xcodeproj 运行（服务器地址填 http://127.0.0.1:8080）
cd ios && xcodegen generate
```

- 登录时任意邮箱都可以，验证码 `123456`。只有在 `--dev` 且监听回环地址时才会生效。
- 真机调试：`HUSH_ADDR=0.0.0.0:8080 bash scripts/run-server.sh`，App 里填电脑的局域网 IP。这时固定验证码不生效，验证码打印在服务端日志里；也可以配置 `HUSH_SMTP_*` 真实发送邮件。
- 本机当前数据目录中已有测试账号：`alice@hush.local`（Android）、`bob@hush.local`（iOS）、`bot@hush.local`（小助手）、`bot2@hush.local`（小美）。新部署从空库开始。
- Android 构建需要设置 `ANDROID_HOME`，或在 `android/local.properties` 写 `sdk.dir=...`（该文件不入库）。

## 功能对标（Signal 约 80%）

| 模块 | 已实现 | 未做（剩余约 20%） |
|---|---|---|
| 账号 | 邮箱验证码注册登录、昵称/头像/签名、单设备登录（换机后旧设备下线，对方收到“安全码已变更”提示） | 手机号、PIN、多设备关联、备份恢复 |
| 联系人 | 邮箱搜索、二维码名片与扫码、好友申请、删除、拉黑 | 通讯录匹配、用户名 |
| 单聊（E2EE） | 文字、图片、语音、文件，引用回复，表情回应，撤回，本地删除，**转发（多选会话，复用加密附件）**，送达/已读回执，正在输入，阅后即焚，安全码核验（数字 + 二维码），失败重发 | 贴纸、GIF、编辑消息、一次性查看媒体、Sealed Sender |
| 群聊（E2EE） | Megolm 加密；建群，改群名/公告，加人、踢人，管理员，退群；踢人后轮换密钥；**@提及（「有人@我」提示，免打扰群里被 @ 仍通知）** | 邀请链接、群通话、@所有人 |
| 会话 | 未读、置顶、免打扰、归档、删除会话、**草稿**、本地全文搜索（点击结果跳转并高亮） | 跨设备草稿同步 |
| 朋友圈 | 图文九宫格、点赞评论（仅共同好友可见）、三种可见范围、互动通知、个人相册 | 视频动态、朋友圈加密 |
| 通话 | 1:1 语音/视频（WebRTC，信令走 E2EE 通道，内嵌 TURN），静音、扬声器、切换摄像头、通话记录 | 群通话、CallKit |
| 隐私设置 | 已读回执与输入状态开关、屏幕锁（生物识别）、防截屏（Android）/ 后台模糊（iOS）、默认阅后即焚、黑名单 | |
| 通知 | Android 前台服务保持长连接 + 本地通知；iOS 前台/回前台同步 + 本地通知（通知只显示“来自 xx 的新消息”，不含内容） | FCM/APNs（服务端只预留了接口） |

## 验收结果

> 验收截图（约 76MB）未随源码发布；运行 `ios/HushUITests` 会在 `docs/screenshots/ios/` 下重新生成 iOS 截图。

- **跨平台互通**：Android（Alice）与 iOS（Bob）之间完成了加好友、双向加密聊天（已读回执正常）、Android→iOS 语音通话 23 秒、iOS↔Android 视频通话双向出画（iOS 模拟器没有摄像头时自动发送标注“模拟摄像头”的合成画面）。
- **Android**：登录、聊天全部功能、群聊、安全码、朋友圈、两台 Android 之间的语音通话、设置、深色模式、防截屏。
- **iOS**：由 XCUITest 驱动完成全部流程，包括通知横幅和深色模式。

## 正式版构建

- Android：`cd android && ./gradlew assembleRelease`（R8 混淆 + 资源压缩）。签名读取 `android/keystore.properties`（不入库），不存在时回退 debug 签名。
- iOS：Release 配置不含测试用的文件共享与明文网络例外；真机需在 Xcode 中设置开发者团队后签名。
- UI 测试可通过 `TEST_RUNNER_HUSH_SERVER`、`TEST_RUNNER_HUSH_EMAIL`、`TEST_RUNNER_HUSH_SHOT_PREFIX` 指向其他服务器、账号与截图前缀。

## 已知限制

详见 [Android 问题记录](docs/issues/android.md)、[iOS 问题记录](docs/issues/ios.md)、[服务端补充说明](server/API_NOTES.md)、[核心库说明](core/CORE_NOTES.md)。

- 发出消息按本机时间排序，设备与服务端时钟偏差较大时，双方消息的相对顺序可能轻微错位。
- 草稿只保存文字，恢复后 @ 提及需重新选择。
- iOS 模拟器（iOS 26.5 运行时）上偶发 XCUITest 启动的 App 卡在系统动态库加载（dyld open）而白屏，属模拟器环境问题，手动启动不受影响。
- 群成员由服务端维护，服务端理论上可以私自往群里加人。这是 Megolm 方案的已知取舍，适合小范围可信部署。
- 本地部署没有接入推送：App 被系统杀掉后收不到消息，Android 靠前台服务保活。
- Android 模拟器的网络会回收空闲约 20 秒的 TCP 连接，核心库已把心跳改为 10 秒来适配。
- iOS 的 `UIFileSharingEnabled` / `LSSupportsOpeningDocumentsInPlace` 只在 Debug 构建中由 `ios/project.yml` 的构建脚本加入（供 UI 测试放入示例文件），Release 的 `Info.plist` 不含这两项，也不再有 `NSAllowsArbitraryLoads`。
