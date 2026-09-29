# Hush 客户端 UI 与平台规范（Android / iOS 共用）

目标：两端界面结构、文案、交互一致；视觉语言接近 Signal（简洁、蓝色强调、圆角气泡、大量留白），导航结构用微信式四 Tab。
**clean-room**：只参考 Signal/微信公开可见的界面与交互，不看、不抄任何源码与资源（图标用系统图标库：Android Material Icons，iOS SF Symbols）。

## 1. 设计令牌

| 令牌 | 浅色 | 深色 | 用途 |
|---|---|---|---|
| accent | #2C6BED | #5B8DF5 | 主色：本人气泡、按钮、选中 Tab、链接 |
| onAccent | #FFFFFF | #FFFFFF | 主色上的文字 |
| background | #FFFFFF | #121212 | 页面背景 |
| surface | #F6F6F6 | #1E1E1E | 列表分组背景、输入框 |
| incomingBubble | #E9E9EB | #2C2C2E | 对方气泡 |
| textPrimary | #1B1B1D | #E9E9E9 | 正文 |
| textSecondary | #6B6B70 | #A0A0A5 | 次要文字、时间 |
| divider | #E3E3E3 | #2C2C2E | 分割线 |
| danger | #E0413B | #FF6961 | 删除、拒绝、挂断 |
| success | #1FA95F | #34C77B | 接听、已核验 |

- 圆角：气泡 18，卡片 12，头像圆形。间距基数 4（常用 8/12/16/24）。
- 字号：标题 20 semibold，正文 16，次要 13，时间戳 11。
- 头像：有图片显示图片；否则按 user_id 哈希从 8 种柔和色中取底色 + 昵称首字（中文取第一个字，英文取首字母大写）。
- 支持深色模式跟随系统。

## 2. 页面结构

```
启动
 ├─ 未登录 → 欢迎页（Logo + “开始使用”）→ 服务器地址页 → 邮箱页 → 验证码页 → 资料设置页（头像+昵称，新用户）→ 主界面
 └─ 已登录 → 屏幕锁（若开启）→ 主界面

主界面：底部 4 Tab
 1. 聊天     会话列表
 2. 通讯录   好友申请入口(红点) / 群聊 / 好友列表(按拼音或首字母分组可选，至少按名称排序)
 3. 朋友圈   动态流
 4. 我       个人卡片 / 设置
```

### 2.1 登录流程
- 服务器地址页：输入框预填默认值（Android 模拟器 `http://10.0.2.2:8080`，iOS 模拟器 `http://127.0.0.1:8080`），说明文字“本地部署的 Hush 服务器地址”。
- 邮箱页：“输入你的邮箱” + 下一步；调用 `request_code`。
- 验证码页：6 位数字输入（自动聚焦、输满自动提交），“重新发送（60s）”倒计时；调用 `verify_code`。开发环境提示“开发模式验证码见服务器日志（默认 123456）”。
- 资料页（`is_new` 或昵称为空时）：圆形头像选择（相册）、昵称必填、“完成”。

### 2.2 聊天 Tab（会话列表）
- 顶部：大标题“聊天”，右上角：新建（弹出菜单：发起单聊→选好友、新建群聊→多选好友+群名）、搜索图标（搜索会话名与消息内容，调用 `search_messages`）。
- 连接状态条：未连接时顶部显示“连接中…”/“未连接”细条。
- 行：头像 56、标题（名称/群名）、最后消息预览（图片显示“[图片]”，语音“[语音]”，文件“[文件] 名称”，撤回“你撤回了一条消息”，草稿可不做）、时间（今天显示 HH:mm，昨天“昨天”，一周内星期，其余 yyyy/M/d）、未读数徽章（accent 圆形，免打扰时灰色小点）、置顶行背景 surface、免打扰图标。
- 左/右滑或长按菜单：置顶/取消置顶、免打扰、归档、删除（清空本地）。
- 列表底部“已归档的会话 (n)”入口。
- 空状态：插图 + “还没有会话，去通讯录找朋友聊天吧”。

### 2.3 聊天页
- 顶栏：返回、头像+名称（群显示“群名 (人数)”）、副标题“正在输入…”/阅后即焚计时（如“⏱ 1 小时”）；右侧：视频通话、语音通话（仅单聊）、更多（进入会话设置）。
- 消息列表（倒序加载，下拉/上滑加载更早）：
  - 本人气泡靠右 accent 背景白字；对方靠左 incomingBubble；群聊对方气泡上方显示发送者昵称（按 user_id 着色），左侧小头像。
  - 气泡内右下角：时间 + 状态图标（本人：发送中=时钟，已发送=单勾，已送达=双勾，已读=实心双勾，失败=红色感叹号可点重发）。
  - 日期分隔条（“今天”“昨天”“9月20日”）。
  - 引用：气泡顶部浅色块显示被引用人名 + 摘要，点击滚动到原消息。
  - 表情回应：气泡下方小胶囊“👍 2”。
  - 图片：圆角缩略图（先显示 thumbnail 模糊图，点击下载并全屏查看，支持缩放、保存到相册可选）。
  - 语音：播放按钮 + 波形/进度条 + 时长。
  - 文件：文件图标 + 文件名 + 大小，点击下载后用系统分享/打开。
  - 系统消息居中灰字（“安全码已变更”“xx 设置了阅后即焚 1 小时”“xx 加入了群聊”）。
  - 无法解密：灰色斜体“⚠️ 无法解密的消息”。
  - 通话记录：“📞 语音通话 3:21”/“未接视频通话”。
  - 阅后即焚消息：气泡角落显示小计时图标，到期消失（动画淡出）。
- 长按消息菜单：回复、回应（快捷 6 个表情：👍 ❤️ 😂 😮 😢 🙏）、复制（文字）、撤回（本人 24h 内）、删除（本地）、详情可选。
- 输入栏：左侧“+”（相册图片、拍照可选、文件）、文本框（多行，最大 5 行）、右侧：有文字时显示发送按钮，否则显示麦克风（按住录音，上滑取消，松开发送；显示录音时长）。
- 正在输入：文本变化时 `set_typing(true)`，停止 3 秒或发送后 `set_typing(false)`。
- 进入页面与收到新消息时（页面可见）调用 `mark_read`。

### 2.4 会话设置
- 单聊：大头像+名称+邮箱；按钮行：语音/视频/静音；列表：阅后即焚（关闭/30 秒/5 分钟/1 小时/8 小时/1 天/1 周）、免打扰、置顶、**查看安全码**、清空聊天记录、拉黑/取消拉黑、删除好友（红色）。
- 安全码页：60 位数字（12 组 × 5 位，3 列排列）、二维码（内容 `qr_payload`）、“扫码核验”按钮（扫描对方二维码，调用 `verify_safety_qr`，成功显示绿色“已核验”）、“标记为已核验/取消核验”。已核验联系人在聊天页标题旁显示小盾牌。
- 群聊：群头像+群名（管理员可编辑）、群公告、成员网格（头像+名称，“+”添加，管理员“-”移除；点成员可设为管理员（群主））、阅后即焚、免打扰、置顶、清空、退出群聊（红色）。

### 2.5 通讯录 Tab
- 顶部：“通讯录”，右上角“添加好友”（进入添加页）。
- 固定项：新的朋友（红点数量 = 待处理申请）、群聊。
- 好友列表：头像+昵称，点击进入个人资料页（按钮：发消息、语音通话、视频通话、查看朋友圈）。
- 添加好友页：邮箱搜索框（精确匹配，调用 `search_user_by_email`，结果卡片 + “添加到通讯录”→ 填写验证消息）、“扫一扫”、“我的二维码”（展示 `my_qr_payload` 的二维码 + 头像昵称）。扫描到 `hush-user:v1:<id>` → 打开资料页可加好友。
- 新的朋友页：申请列表（头像、昵称、验证消息、接受/拒绝按钮；已处理显示状态）。

### 2.6 朋友圈 Tab
- 顶部封面区（高 ~280，背景为 accent 渐变或默认图）、右下角本人头像+昵称；右上角相机图标发布动态。
- 动态卡片：头像、昵称（accent 色）、正文、图片九宫格（1 张大图保持比例，2–9 张 3 列方格）、时间（“x 分钟前”“x 小时前”“昨天”“M月d日”）、自己的动态显示“删除”、右侧“··”按钮弹出“赞/评论”。
- 互动区（浅灰背景）：❤️ 点赞者昵称列表；评论列表“A 回复 B：内容”，点击评论可回复该人，长按自己的评论可删除。
- 下拉刷新、上滑加载更多（before=最后一条 created_at）。
- 顶部新消息提示条：“n 条新消息”（来自 MomentNotification 事件），点击进入通知列表页。
- 发布页：多行文本、添加图片（最多 9 张，相册多选，发布前压缩到长边 ≤ 2048、JPEG 质量 0.8）、“谁可以看”（公开给所有好友 / 部分可见（选择好友）/ 不给谁看（选择好友））、发布。
- 个人相册页（从资料页“朋友圈”进入）：该用户的动态列表。

### 2.7 我 Tab 与设置
- 个人卡片：头像、昵称、邮箱、签名，点击进入编辑资料（头像/昵称/签名）；右侧二维码图标 → 我的二维码。
- 设置分组：
  - 隐私：已读回执（开关）、正在输入提示（开关）、默认阅后即焚、屏幕锁（开关，生物识别/设备密码）、屏幕安全（Android：禁止截屏 FLAG_SECURE；iOS：切到后台时模糊遮罩）、黑名单。
  - 通知：消息通知（开关）、显示内容（仅显示“你有新消息”，固定，说明文案）。
  - 连接：服务器地址（只读显示）、连接状态。
  - 关于：版本号、“端到端加密由 vodozemac（Olm/Megolm）提供”、开源许可说明。
  - 退出登录（红色，二次确认：“退出后本机聊天记录将被清除”）。

### 2.8 通话
- 发起：聊天页或资料页按钮 → 全屏通话页（对方头像/名称/“正在呼叫…”）。
- 来电：全屏来电页（头像、名称、“语音来电/视频来电”、拒绝（红）/接听（绿））；App 在前台时直接弹出；Android 后台时通过前台服务通知（全屏 Intent）拉起。
- 通话中：计时、静音、扬声器、（视频）关闭摄像头/切换前后摄像头、挂断；视频通话对方画面全屏，本地画面右上角小窗。
- 信令（经 `send_call_signal` 与 `CallSignalReceived` 事件）：主叫 offer(sdp) → 被叫 answer(sdp) → 双方 ice(candidates，可批量) → hangup/reject/busy。已在通话中收到 offer 回 busy。30 秒无应答自动挂断记为未接。
- ICE 服务器来自 `turn_credentials()`。

## 3. 平台实现要点

### Android
- Kotlin + Jetpack Compose + Material3（按上表自定义 ColorScheme），Navigation Compose。单 Activity。
- 核心封装：`HushCore` 单例（Application 中创建 `HushClient`，data_dir = `filesDir/hush`；db_key 由 Android Keystore 中的 AES/GCM 密钥加密后存 SharedPreferences；首次随机生成 32 字节）。
- EventListener 实现把 `CoreEvent` 转成 `SharedFlow`，ViewModel 订阅并刷新状态（事件回调在后台线程，切到主线程更新 Compose 状态）。
- 后台保持连接：前台服务 `ConnectionService`（type `dataSync` 或 `remoteMessaging`，常驻低优先级通知“Hush 正在保持连接”），登录后启动；收到 MessageAdded 且对应会话不在前台、未免打扰时发本地通知（渠道：消息、来电）。通知内容固定“你有新消息”（隐私）+ 会话名可选（首版显示“来自 xx 的新消息”即可）。
- 权限：INTERNET、POST_NOTIFICATIONS、FOREGROUND_SERVICE(+类型)、RECORD_AUDIO、CAMERA、USE_BIOMETRIC、MODIFY_AUDIO_SETTINGS、USE_FULL_SCREEN_INTENT。运行时申请。
- 图片：Coil 3 加载本地路径；相册用 `PickVisualMedia`；文件用 `OpenDocument`；语音用 MediaRecorder（AAC in m4a）+ MediaPlayer。
- 二维码：生成用 ZXing core；扫描用 CameraX + ML Kit barcode-scanning（捆绑模型，不依赖 GMS 下载）。
- 通话：`io.github.webrtc-sdk:android`；`CallManager` 管 PeerConnection、音频路由、SurfaceViewRenderer。
- 屏幕锁：androidx.biometric `BiometricPrompt`（允许设备凭据）；FLAG_SECURE 按设置开关。

### iOS
- SwiftUI（iOS 17+，`@Observable`），NavigationStack，TabView。
- 核心封装：`HushCore` 单例（data_dir = Application Support/hush；db_key 存 Keychain `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`）。EventListener 回调在后台线程 → `MainActor` 更新 Observable 模型。
- 通知：UNUserNotificationCenter 本地通知（前台收到消息且不在对应聊天页时横幅提示；后台时 App 进入后台 ~30s 内仍可收到）。回前台自动 `start()` 重连并同步。无 APNs（本地部署）。
- 媒体：PhotosPicker、UIDocumentPicker（fileImporter）、AVAudioRecorder（m4a AAC）/AVAudioPlayer。
- 二维码：CoreImage `CIQRCodeGenerator` 生成；AVCaptureMetadataOutput 扫描。
- 通话：WebRTC（stasel/WebRTC SPM 153.0.0，已验证可编译）；AVAudioSession 配置 playAndRecord + voiceChat；RTCMTLVideoView 渲染。CallKit 可不做。
- 屏幕锁：LocalAuthentication（deviceOwnerAuthentication）；后台遮罩：scenePhase != active 时覆盖模糊视图。
- Info.plist 需加：NSCameraUsageDescription、NSMicrophoneUsageDescription、NSPhotoLibraryUsageDescription、NSFaceIDUsageDescription（中文说明）、UIBackgroundModes: audio（通话）。

## 4. 验收截图清单（保存到 docs/screenshots/<platform>/）
登录流程、会话列表、单聊（含图片/语音/引用/回应/已读勾）、群聊、会话设置、安全码、通讯录、添加好友、我的二维码、朋友圈（含点赞评论）、发布动态、我/设置、语音通话中、来电页。
