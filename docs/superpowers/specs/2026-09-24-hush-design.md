# Hush（密语）设计文档

- 日期：2026-09-24
- 定位：内部/小范围使用的加密社交 App ——「Signal 级端到端加密聊天 + 微信式朋友圈」
- 目标完善度：功能与体验对标 Signal 约 80%
- 交付形态：Go 服务端本地部署 + Android 原生（Kotlin/Compose）+ iOS 原生（Swift/SwiftUI）
- 自研声明：架构参考 Signal 公开文档与开源项目的**公开结构与功能**，未复制、未改写 Signal 源码；
  端到端加密使用 [vodozemac](https://github.com/matrix-org/vodozemac)（Apache-2.0，Olm/Megolm 实现）。
  服务端结构参考唐僧叨叨/WuKongIM（Apache-2.0）的思路。所有代码为本项目自研。

## 1. 总体架构

```
Android (Kotlin + Compose)            iOS (Swift + SwiftUI)
   UI / 通知 / 相机麦克风 / WebRTC        UI / 通知 / 相机麦克风 / WebRTC
            └──────────────┬──────────────────┘
          hushcore（Rust，UniFFI 生成 Kotlin/Swift 绑定）
          · 网络：HTTPS(REST) + WebSocket（自动重连）
          · 加密：Olm(1:1) / Megolm(群) / 附件 AES-256-GCM
          · 本地库：SQLite（消息正文字段级 AES-GCM 加密，库密钥存 Keystore/Keychain）
          · 业务：会话、消息、回执、群、好友、朋友圈、通话信令
                           │
                 Go 单体服务端 hush-server
   账号 │ 预密钥仓库 │ 消息中继+离线队列 │ 好友/群成员 │ 朋友圈(明文) │ 附件(密文 blob) │ 内嵌 TURN
                SQLite(WAL) + 本地文件目录 data/
```

关键原则：**客户端一切业务逻辑放在 Rust 核心里写一次**，两端原生代码只做 UI 与平台能力（通知、媒体采集、WebRTC、密钥安全存储）。

服务器可见：账号邮箱、昵称头像、好友关系、群成员、朋友圈内容、消息元数据（谁、何时、发给谁）。
服务器不可见：聊天文字/图片/语音/文件内容、回执、正在输入、通话信令（SDP/ICE）。

## 2. 功能范围（对标 Signal 的 80%）

| 模块 | 包含 | 不包含（剩余 20%） |
|---|---|---|
| 账号 | 邮箱验证码注册登录、昵称/头像/签名、退出登录 | 手机号、PIN/SVR、多设备关联 |
| 联系人 | 邮箱搜索、二维码名片与扫码、好友申请/同意/拒绝、删除、拉黑 | 通讯录匹配 |
| 单聊 | E2EE 文字/图片/语音/文件、引用回复、表情回应、撤回(全员删除)、本地删除、送达/已读回执、正在输入、阅后即焚定时器、安全码核验 | 贴纸、支付、Sealed Sender |
| 群聊 | E2EE（Megolm）、建群、改名/头像/公告、加人踢人、管理员、退群、群内回执 | 群链接邀请、群通话 |
| 会话列表 | 未读数、置顶、免打扰、归档、本地消息搜索 | 故事(Stories) |
| 朋友圈 | 图文动态、点赞评论（仅共同好友可见）、可见范围（全部/仅部分/排除部分）、个人相册页、互动通知 | 视频动态、加密朋友圈 |
| 通话 | 1:1 语音/视频（WebRTC，信令走 E2EE 通道，内嵌 TURN）| 群通话 |
| 设置 | 已读回执开关、输入状态开关、屏幕锁（生物识别）、防截屏（Android）、通知开关、默认阅后即焚、服务器地址 | 备份恢复 |
| 通知 | Android 前台服务保持长连接 + 本地通知；iOS 前台/回前台同步 + 本地通知 | FCM/APNs（仅保留服务端接口，本地部署不接） |

## 3. 服务端接口契约（hush-server）

通用约定：
- Base URL：`http://<host>:8080`。JSON UTF-8。时间戳均为 Unix 毫秒。二进制字段 base64（标准，带填充）。
- 认证：`Authorization: Bearer <token>`（WebSocket 也可用 `?token=`）。
- 错误：HTTP 4xx/5xx + `{"error":"<code>","message":"<人类可读>"}`。
  常用 code：`bad_request` `unauthorized` `forbidden` `not_found` `not_friends` `blocked` `rate_limited` `conflict` `too_large`。
- ID：user_id / group_id / moment_id / attachment_id / message id 均为 UUIDv4 字符串（服务端生成的除外，message 服务端 id 也是 UUID）。

### 3.1 账号
| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| POST | /v1/auth/code | `{email}` | `{}`；开发模式把验证码打印到日志；配置 `HUSH_DEV_CODE` 时该固定码永远有效 |
| POST | /v1/auth/verify | `{email, code, device_name, platform}` platform=android/ios | `{user_id, token, is_new}` |
| POST | /v1/auth/logout | – | `{}`（作废 token） |
| GET | /v1/me | – | `Profile` + `{email}` |
| PUT | /v1/me/profile | `{name?, about?, avatar_id?}` | `Profile` |
| GET | /v1/users/{id} | – | `Profile` |
| GET | /v1/users/search?email= | 精确匹配 | `Profile` 或 404 |

`Profile = {id, name, about, avatar_id, updated_at}`。

单设备语义：`verify` 成功即生成新 token、**作废旧 token**、清空该用户旧预密钥与未投递消息、`device_generation+1`；
若旧连接在线则发 `{"t":"event","d":{"type":"logged_out","reason":"logged_in_elsewhere"}}` 后关闭。
同时向其所有好友推送事件 `key_changed{user_id}`。

### 3.2 预密钥（vodozemac Olm）
| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| PUT | /v1/keys | `{identity_key, signing_key, one_time_keys:[{key_id,key,signature}], fallback_key?:{key_id,key,signature}}` | `{one_time_key_count}` |
| GET | /v1/keys/count | – | `{one_time_key_count}` |
| POST | /v1/keys/claim/{user_id} | – | `{user_id, identity_key, signing_key, device_generation, one_time_key:{key_id,key,signature,fallback:bool}}` |
| GET | /v1/keys/identity/{user_id} | – | `{user_id, identity_key, signing_key, device_generation}` |

- identity_key=Curve25519 公钥 base64（vodozemac `to_base64()`），signing_key=Ed25519 公钥 base64。
- signature = Ed25519(`key` 字符串的 UTF-8 字节) 的 base64，由客户端用 Account 签名；对端 claim 后必须验签。
- identity_key/signing_key 在同一 device_generation 内首次上传后不可变（再传不同值返回 `conflict`）。
- claim 优先消耗一个 one-time key（取出即删除）；耗尽时返回 fallback key（fallback=true，不删除）。
- 客户端在 count < 20 时补充到 100 个。

### 3.3 消息投递
Olm（点对点，含 1:1 聊天内容、回执、输入状态、通话信令、群密钥分发）：

`POST /v1/messages`
```json
{"messages":[{"to":"<user_id>","type":0,"body":"<b64 olm ciphertext>","client_id":"<uuid>","urgent":true}]}
```
- type：0=PreKey 消息，1=Normal 消息（与 vodozemac OlmMessage 类型一致）。
- 校验：双方为好友，或共同在至少一个群里；且未互相拉黑。否则该条结果为错误。
- `urgent=false` 用于回执/输入状态，不触发推送。
- 响应：`{"results":[{"to","client_id","id","server_ts","error?"}]}`

Megolm（群）：`POST /v1/groups/{gid}/messages`
```json
{"session_id":"<megolm session id>","body":"<b64 megolm ciphertext>","client_id":"<uuid>","urgent":true}
```
服务端校验发送者是群成员，扇出给除发送者外全部成员。响应 `{id, server_ts}`。

信封（服务端 → 客户端）：
```json
{"id":"<uuid>","kind":"olm|megolm","from":"<user_id>","type":0,"group_id":null,"session_id":null,"body":"<b64>","server_ts":1727000000000}
```
- `GET /v1/messages?limit=100` 拉取未确认信封（轮询兜底）。
- `POST /v1/messages/ack {"ids":[...]}` 确认后删除。

### 3.4 WebSocket `GET /v1/ws`
- 服务端 → 客户端帧：
  - `{"t":"envelope","d":<信封>}`
  - `{"t":"queue_empty"}` 连接建立后积压消息推送完毕
  - `{"t":"event","d":{"type":...}}` 服务端事件（明文，不含聊天内容），type 取值：
    `friend_request{request}` `friend_accepted{user_id}` `friend_removed{user_id}` `profile_updated{profile}`
    `group_updated{group}` `group_removed{group_id}` `key_changed{user_id}` `moment_notification{notification}` `logged_out{reason}`
  - `{"t":"pong"}`
- 客户端 → 服务端帧：`{"t":"ack","ids":[...]}`、`{"t":"ping"}`（每 25s）。
- 事件也持久化：离线时排队，与信封一起在重连时推送（事件 id 同样需 ack）。事件帧格式为 `{"t":"event","d":{"id":"<uuid>","type":...,...}}`。

### 3.5 好友与拉黑
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /v1/friends/requests | `{to_user_id, message}` → `FriendRequest`；对方已发申请给我则直接成为好友 |
| GET | /v1/friends/requests | `{incoming:[...], outgoing:[...]}`（pending） |
| POST | /v1/friends/requests/{id}/accept | → `{}` |
| POST | /v1/friends/requests/{id}/reject | → `{}` |
| GET | /v1/friends | `[Profile]` |
| DELETE | /v1/friends/{user_id} | 双向删除 |
| POST/DELETE | /v1/blocks/{user_id} | 拉黑/取消；GET /v1/blocks → `[user_id]` |

`FriendRequest = {id, from: Profile, to: Profile, message, status, created_at}`。

### 3.6 群组（服务端维护成员表）
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /v1/groups | `{name, member_ids}`（成员须为创建者好友）→ `Group` |
| GET | /v1/groups | `[Group]` |
| GET | /v1/groups/{id} | `Group` |
| PUT | /v1/groups/{id} | `{name?, avatar_id?, announcement?}`（管理员） |
| POST | /v1/groups/{id}/members | `{user_ids}`（管理员；须为操作者好友） |
| DELETE | /v1/groups/{id}/members/{uid} | 管理员踢人，或 uid=自己即退群 |
| PUT | /v1/groups/{id}/admins/{uid} | `{admin:true/false}`（仅群主） |

`Group = {id, name, avatar_id, announcement, owner_id, members:[{user_id, role: owner|admin|member, joined_at}], updated_at}`。
任何变更向全体（含被移除者）推送 `group_updated` / `group_removed`。

### 3.7 附件
- `POST /v1/attachments`，body 为原始字节（`application/octet-stream`，上限 100MB）→ `{id, size}`
- `GET /v1/attachments/{id}` → 原始字节
- 聊天附件上传的是**客户端加密后的密文**；朋友圈与头像上传明文。

### 3.8 朋友圈
| 方法 | 路径 | 说明 |
|---|---|---|
| POST | /v1/moments | `{text, media:[{attachment_id, width, height}], visibility:{mode:"all|include|exclude", user_ids:[]}}` → `Moment` |
| GET | /v1/moments/feed?before=&limit=20 | 自己 + 好友中对我可见的动态 |
| GET | /v1/moments/user/{uid}?before=&limit=20 | 某人的动态（非好友只能看自己的） |
| DELETE | /v1/moments/{id} | 作者删除 |
| POST/DELETE | /v1/moments/{id}/like | 点赞/取消 |
| POST | /v1/moments/{id}/comments | `{text, reply_to_user_id?}` → `Comment` |
| DELETE | /v1/moments/comments/{cid} | 评论作者或动态作者删除 |
| GET | /v1/moments/notifications?before= | 与我相关的点赞/评论 |

`Moment = {id, author: Profile, text, media:[...], visibility?(仅作者可见此字段), likes:[Profile], comments:[Comment], created_at}`
`Comment = {id, author: Profile, reply_to: Profile?, text, created_at}`
可见性规则（微信式）：点赞与评论仅返回「查看者的好友或查看者本人」产生的条目。
通知：有人点赞/评论我的动态、或回复我的评论时，推送 `moment_notification{id, kind: like|comment, moment_id, from: Profile, text?, created_at}`。

### 3.9 通话
- `GET /v1/calls/turn` → `{urls:["stun:<host>:3478","turn:<host>:3478?transport=udp"], username, credential, ttl}`
  使用 TURN REST 时限凭证：`username="<expiry>:<user_id>"`，`credential=base64(HMAC-SHA1(secret, username))`。
- 服务端内嵌 pion/turn（UDP 3478），`--public-ip` 配置中继地址。
- 信令全部走 Olm 加密内容消息（见 4.2 `call`）。

### 3.10 部署
- `hush-server --addr :8080 --data ./data --turn-port 3478 --public-ip 127.0.0.1 --dev`
- 环境变量：`HUSH_DEV_CODE`（固定验证码）、`HUSH_SMTP_HOST/PORT/USER/PASS/FROM`（配了就真发邮件）、`HUSH_TURN_SECRET`。
- Android 模拟器访问宿主机：`http://10.0.2.2:8080`；iOS 模拟器：`http://127.0.0.1:8080`；真机填局域网 IP。

## 4. 端到端加密设计

### 4.1 会话建立（1:1）
1. 注册后客户端生成 vodozemac `Account`，上传 identity/signing key、100 个 one-time key、1 个 fallback key。
2. A 首次给 B 发消息：`claim` B 的密钥 → 验 OTK 签名 → `create_outbound_session(SessionConfig::version_2(), B.identity, B.otk)` → 加密得 PreKey 消息（type 0）。
3. B 收到 type 0 且无可用会话：`create_inbound_session(A.identity, prekey)` → 得明文并保存会话，移除已用 OTK。
4. 之后双方用 Olm 双棘轮会话互发 type 1 消息。
5. 身份变更：收到 `key_changed` 或 claim 发现 identity_key 与本地记录不同 → 丢弃旧会话、在会话里插入系统提示「安全码已变更」、已核验标记清除。
6. 解密失败：该消息显示为「无法解密的消息」，并向对方发送 `session_reset` 以便其下次重建会话（对方收到后丢弃会话，下一条消息重新 claim）。

### 4.2 明文内容格式（加密前的 JSON，`Content`）
```json
{"v":1,"id":"<消息uuid>","ts":1727000000000,"group_id":null,"expire_secs":0,
 "body":{"type":"text","text":"你好","quote":{"id":"...","author":"<uid>","text":"..."}}}
```
`body.type` 枚举：
| type | 字段 | 说明 |
|---|---|---|
| text | text, quote? | 文字 |
| attachment | attachments:[{id,key,digest,size,mime,file_name,kind:image|voice|video|file,width,height,duration_ms,thumbnail?}], caption?, quote? | key=32 字节 AES-256-GCM 密钥 b64；密文格式 `nonce(12)‖ciphertext‖tag(16)`；digest=密文 SHA-256；thumbnail≤10KB JPEG b64 |
| reaction | target_id, target_author, emoji, remove | 表情回应 |
| delete | target_id | 撤回（全员删除，发送者 24 小时内） |
| receipt | kind: delivered|read, ids:[...] | 回执（不再回执） |
| typing | action: started|stopped | 正在输入 |
| timer | expire_secs | 修改阅后即焚（0=关闭） |
| call | call_id, action: offer|answer|ice|hangup|busy|reject, video, sdp?, candidates?:[{sdp,sdp_mid,sdp_mline_index}] | 通话信令 |
| room_key | group_id, session_id, session_key | Megolm 会话密钥分发（仅经 Olm） |
| session_reset | – | 请对方重建 Olm 会话 |
| profile_key | – | 预留 |
| group_update | – | 预留（群变更以服务端事件为准） |

### 4.3 群聊（Megolm）
- 发送方每个群维护一个出站 `GroupSession`；首次发送/成员变更（有人被移除或退出）/已发 100 条/超过 7 天 → 轮换新会话。
- 新会话或新成员加入时，通过 Olm 向每个（尚未拥有该会话的）成员发送 `room_key`，然后再发群消息。
- 接收方按 `(sender, session_id)` 存入站会话；缺密钥的消息先存「待解密」，收到 room_key 后重试。
- 群内回执：已读/送达回执经 Olm 单独发给原发送者；输入状态经 Megolm 发群。
- 已知取舍：成员关系由服务器决定（服务器理论上可加人），适合小范围可信部署。

### 4.4 安全码
每方指纹 = 对 `0x00 0x00 ‖ identity_key 原始 32 字节 ‖ user_id UTF-8` 迭代 SHA-512 5200 次（每轮输入 = 上轮哈希 ‖ identity_key），
取前 30 字节按 5 字节一组转成 5 位十进制（大端整数 mod 100000），得 30 位数字；
安全码 = 两方 30 位数字按 user_id 字典序拼接 = 60 位，按 12 组×5 位展示。二维码内容 `hush-sn:v1:<60位>`。

### 4.5 本地存储
- Rust 核心用 SQLite（rusqlite bundled）。消息正文与附件密钥等敏感字段用 32 字节库密钥 AES-256-GCM 加密存储。
- vodozemac 的 Account / Session / GroupSession 以 pickle 形式经库密钥加密后存储。
- 库密钥由平台生成：Android 用 Keystore AES 密钥包裹后存 SharedPreferences；iOS 存 Keychain（`kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`）。
- 阅后即焚：消息 `expire_secs>0`，发送方发送即开始计时，接收方已读开始计时；核心每秒扫描删除到期消息并通知 UI。

## 5. Rust 核心 API（hushcore，UniFFI proc-macro）

```text
object HushClient
  constructor new(data_dir: String, db_key: Vec<u8>, platform: String, device_name: String)
  set_listener(listener: EventListener)
  // 账号
  async request_code(server_url, email)
  async verify_code(server_url, email, code) -> MyAccount
  is_logged_in() -> bool
  my_account() -> Option<MyAccount>
  async logout()
  // 连接
  start()  // 启动 WS 循环（自动重连、指数退避）、补充 OTK、定时器
  stop()
  connection_state() -> ConnectionState
  // 资料
  async update_profile(name: Option<String>, about: Option<String>, avatar_path: Option<String>) -> Profile
  async get_user(user_id) -> Profile ; async search_user_by_email(email) -> Option<Profile>
  async fetch_avatar(avatar_id) -> String(本地文件路径，缓存)
  // 好友
  async send_friend_request(user_id, message) ; async list_friend_requests() -> FriendRequests
  async accept_friend_request(id) ; async reject_friend_request(id)
  async refresh_friends() -> Vec<Profile> ; friends() -> Vec<Profile>
  async remove_friend(user_id) ; async block(user_id) ; async unblock(user_id) ; blocked() -> Vec<String>
  my_qr_payload() -> String  // "hush-user:v1:<user_id>"
  // 会话与消息
  conversations(archived: bool) -> Vec<ConversationSummary>
  conversation(conv_id) -> Option<ConversationSummary>
  direct_conversation_id(user_id) -> String   // "u:<user_id>"；群为 "g:<group_id>"
  messages(conv_id, before_ts: Option<i64>, limit: u32) -> Vec<MessageItem>
  search_messages(query) -> Vec<MessageItem>
  async send_text(conv_id, text, quote_id: Option<String>) -> MessageItem
  async send_attachment(conv_id, file_path, mime, kind: AttachmentKind, caption, meta: AttachmentMeta) -> MessageItem
  async download_attachment(message_id, index: u32) -> String  // 解密后本地路径
  async react(message_id, emoji, remove: bool)
  async delete_for_everyone(message_id) ; delete_local(message_id) ; clear_conversation(conv_id)
  async mark_read(conv_id) ; async set_typing(conv_id, typing: bool)
  async set_disappearing(conv_id, secs: u32)
  set_pinned(conv_id, bool) ; set_muted(conv_id, bool) ; set_archived(conv_id, bool)
  // 群
  async create_group(name, member_ids) -> GroupInfo ; async refresh_groups()
  group_info(group_id) -> Option<GroupInfo>
  async update_group(group_id, name?, announcement?, avatar_path?)
  async add_members(group_id, ids) ; async remove_member(group_id, uid) ; async leave_group(group_id)
  async set_admin(group_id, uid, admin: bool)
  // 安全码
  async safety_number(user_id) -> SafetyNumber{digits, qr_payload, verified}
  set_verified(user_id, verified: bool) ; verify_safety_qr(user_id, payload) -> bool
  // 朋友圈
  async moments_feed(before: Option<i64>) -> Vec<Moment> ; async user_moments(uid, before) -> Vec<Moment>
  async post_moment(text, image_paths: Vec<String>, visibility: Visibility) -> Moment
  async delete_moment(id) ; async like_moment(id, like: bool) ; async comment_moment(id, text, reply_to: Option<String>) -> Comment
  async delete_comment(id) ; async moment_notifications() -> Vec<MomentNotification>
  async fetch_media(attachment_id) -> String  // 明文附件缓存路径
  // 通话
  async turn_credentials() -> TurnCredentials
  async send_call_signal(user_id, signal: CallSignal)
  // 设置
  settings() -> Settings ; set_settings(Settings)   // read_receipts, typing_indicators, default_disappearing_secs, notifications

callback interface EventListener
  on_event(event: CoreEvent)

enum CoreEvent
  ConnectionChanged{state} | ConversationsChanged | MessageAdded{conv_id, message} | MessageUpdated{conv_id, message}
  | MessageRemoved{conv_id, message_id} | Typing{conv_id, user_id, typing} | FriendRequestReceived{request}
  | FriendsChanged | GroupsChanged | CallSignalReceived{from_user_id, signal} | MomentNotification{notification}
  | KeyChanged{user_id} | LoggedOut{reason}
```

`MessageItem = {id, conv_id, sender_id, is_outgoing, sent_at, server_ts, status: sending|sent|delivered|read|failed, body: MessageBody, quote?, reactions:[{emoji, user_ids}], expire_secs, expires_at?, deleted: bool}`
`MessageBody = Text{text} | Attachments{items, caption} | System{text} | Undecryptable | CallLog{video, duration_secs, missed, outgoing}`

## 6. 客户端 UI（两端一致，Signal 视觉语言 + 微信式四个 Tab）

- 首次启动：服务器地址（预填模拟器默认值）→ 邮箱 → 验证码 → 设置昵称/头像。
- 底部 Tab：**聊天**（会话列表，置顶/免打扰/归档/搜索）、**通讯录**（好友申请、群组、好友列表、添加好友：邮箱搜索/扫码/我的二维码）、**朋友圈**（封面 + 动态流 + 发布）、**我**（资料、设置）。
- 聊天页：气泡（本人蓝色右侧、对方灰色左侧）、状态勾（发送中/已发送/送达/已读）、引用回复、长按菜单（回复、回应、复制、撤回、删除）、图片预览、语音按住录音与播放、文件、正在输入、阅后即焚计时标识、顶部语音/视频通话按钮。
- 会话设置：安全码（数字 + 二维码 + 扫码核验）、阅后即焚、免打扰、群成员管理、清空记录。
- 通话页：来电全屏（接听/拒绝）、通话中（静音、扬声器、切摄像头、挂断、计时）。
- 设置：隐私（已读回执、输入状态、屏幕锁、防截屏）、通知、默认阅后即焚、关于、退出登录。

## 7. 仓库结构

```
server/   Go 服务端（go test 覆盖接口与投递）
core/     Rust 核心 hushcore（cargo test 覆盖加密、存储、与真服务端的双客户端集成测试）
android/  Android 工程（app 模块，jniLibs 由 scripts/build-core-android.sh 产出）
ios/      iOS 工程（XcodeGen project.yml，HushCore.xcframework 由 scripts/build-core-ios.sh 产出）
scripts/  构建与本地部署脚本（run-server.sh、build-core-*.sh）
docs/     设计与计划
```

## 8. 测试与验收
1. 服务端：`go test ./...` 覆盖注册、密钥 claim、投递/ack、好友、群扇出、朋友圈可见性。
2. 核心：`cargo test` 单元测试 + 起真服务端的集成测试（两客户端完成加好友 → Olm 单聊 → 建群 Megolm 群聊 → 附件加解密 → 回执）。
3. 端到端：Android 模拟器与 iOS 模拟器分别登录两个账号，互加好友，互发文字/图片/语音、建群、朋友圈互动、发起语音通话。
4. 验收证据：截图保存在 `docs/screenshots/`。

## 9. 错误处理原则
- 网络：WS 断线指数退避（1s→30s）；发送失败的消息标记 failed，可点击重发。
- 加密：解密失败显示占位并触发 session_reset；缺少群密钥的消息挂起等待 room_key。
- 服务端：所有输入校验，统一错误体；投递按接收方持久化，ack 才删除（至少一次投递，客户端按消息 id 去重）。
