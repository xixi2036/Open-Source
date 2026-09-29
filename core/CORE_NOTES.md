# hushcore 实现说明：契约补充、与 §5 的差异、已知限制

本文以 `docs/superpowers/specs/2026-09-24-hush-design.md` 为准，只记录契约没有写到、由客户端自行决定的细节。
服务端的补充说明见 `server/API_NOTES.md`，本库已按它对接。

## 1. 编码与加密参数

| 项 | 取值 |
|---|---|
| base64 | 本库**发出**的所有二进制字段都用标准字母表、带填充的 base64：信封 body、identity_key、signing_key、OTK 的 key、signature、附件 key 与 digest、thumbnail。**解码**时带不带填充都接受。设计 3.2 写 identity_key 用 vodozemac 的 `to_base64()`（无填充），本库改为带填充，以符合第 3 节的通用约定。服务端把这些字段当作不透明字符串，所以不影响对接。 |
| key_id | vodozemac `KeyId::to_base64()` 的原样输出（无填充，只作不透明 id 使用）。 |
| OTK 签名 | 签名对象是 `key` 字段字符串（带填充 base64）的 UTF-8 字节。claim 之后先用对端的 signing_key 验签，验签失败就拒绝建立会话。 |
| Olm | `SessionConfig::version_2()`，需要开启 vodozemac 的 `experimental-session-config` feature。`create_inbound_session` 同样传入 v2。 |
| Olm body | `base64(OlmMessage::to_parts().1)`，信封的 `type` 取 `to_parts().0`（0=PreKey，1=Normal）。 |
| Megolm | `SessionConfig::version_2()`。body 是 `base64(MegolmMessage::to_bytes())`。room_key 的 `session_key` 用 vodozemac `SessionKey::to_base64()`（无填充，在 Olm 加密的明文内部传递）。 |
| client_id | 等于 `Content.id`。 |
| 附件 | 每个附件随机生成 32 字节密钥，`digest=SHA-256(整个密文文件)`，下载后先校验 digest 再解密。密文格式由附件元数据 `format` 区分（缺省为 1）：**v1**（旧版本发出，只解密）`nonce(12)‖ct‖tag`，整文件一次 GCM；**v2**（C-I，当前发送格式）分块 GCM：`prefix(8 字节随机)‖chunk_0‖…‖chunk_last`，每块 `ct‖tag(16)` 对应 64 KiB 明文（最后一块 0–64 KiB，空文件也有一个空的最后一块），`nonce_i = prefix‖u32_be(i)`，`AAD = "hush-att-v2"‖0x00`（中间块）/ `0x01`（最后一块）。截断、删掉最后一块、末尾追加、调换 / 删除中间块、篡改任一字节都会解密失败。 |
| 安全码 | 初始值 `h = 0x00 0x00 ‖ ik(32B) ‖ user_id`，然后迭代 5200 次 `h = SHA-512(h ‖ ik)`（初始值本身不先做一次哈希）。取前 30 字节，每 5 字节按大端整数 mod 100000 转成 5 位数字。两方结果按 user_id 字典序拼接。`digits` 返回 60 位连续数字，由 UI 分组展示。 |

## 2. 会话与身份

- **身份记录（TOFU）**：第一次需要对端身份时（claim、收到 PreKey、查安全码）从服务端取回并保存。之后如果 claim 返回的 identity 与本地记录不同，或者 PreKey 携带的身份与本地不同而与服务端最新记录一致，就按「身份变更」处理：丢弃 Olm 会话，清除已核验标记，从所有 Megolm 出站会话的已分发名单中移除该用户（下次发送时重新发 room_key），在单聊中插入系统消息「与 X 的安全码已变更」，并发出 `KeyChanged` 事件。
- 收到服务端事件 `key_changed{user_id}` 时处理方式相同。由于此时还不知道新身份，本地身份记录会被删除，下次 claim 时重新按首次信任处理。
- **会话选择**：每个对端最多保留 5 个 Olm 会话，超出时按最近使用淘汰。加密时用最近使用的会话。解密 PreKey 时，先找 `session_keys` 相同的已有会话，找不到再 `create_inbound_session`。解密 Normal 时按最近使用顺序逐个尝试，vodozemac 解密失败不会改变会话状态。双方同时发起会话导致两条会话并存时，也能按这个规则收敛。
- **解密失败**：该消息存为 `Undecryptable` 占位（消息 id = 信封 id），然后向对方发送 `session_reset`（同一用户 60 秒内最多发一次）。发送前会先删掉本地与该用户的全部 Olm 会话，再 claim 新密钥，所以 reset 本身以 PreKey 消息送达。
- **收到 `session_reset`**：只保留承载该消息的那条会话（它是双方刚建立的新会话），丢弃其余会话。契约原文是「丢弃会话」，这样做既满足重建的目的，又不会反复消耗 OTK，也避免双方互发 reset 形成循环。
- **登录**：每次 `verify_code` 都生成全新的 vodozemac Account，上传 100 个 OTK 和 1 个 fallback key（服务端会清空旧预密钥，并把 device_generation 加 1）。同一用户重新登录时保留本地消息，只清空加密状态。换成另一个用户时清空全部本地数据。
- **OTK 补充**：在以下时机检查 `GET /v1/keys/count`，数量小于 20 时补到 100：每次 WS 连上、收到 `queue_empty`、每次通过 PreKey 新建入站会话之后。
- **密钥上传顺序**（登录与补充相同）：持 `crypto_lock` 生成新 OTK / fallback 并**先保存账户**（此时仍是「未发布」），释放锁后 `PUT /v1/keys`，成功后再持锁**重新读取**账户、`mark_keys_as_published` 并写 `fallback_meta`。上传超时（服务端其实已落库）或进程被杀时私钥不会丢；下次补充检查发现账户里仍有未发布的密钥，就原样重传（服务端按 `key_id` 覆盖写入），不再另生成一批。已知残留风险：若服务端已收到那次上传、且在重传前某个 OTK 已被 claim，重传会把它再放回服务端，第二个使用者的 PreKey 将无法解密并触发 session_reset 自愈。上传流程由单独的 `key_upload` 锁串行化；HTTP 请求期间不持有 `crypto_lock`。
- **fallback key 轮换**：在上述同一时机检查，fallback 已被使用过（对方 PreKey 消息里的一次性密钥等于我们发布的 fallback 公钥）或已发布满 7 天时，生成新 fallback 并随 `PUT /v1/keys` 上传。vodozemac 会保留上一个 fallback 的私钥，所以用旧 fallback 建立、仍在途中的会话照样能解密。已发布 fallback 的公钥与时间加密记在 kv `fallback_meta`；没有该记录的旧数据会在首次检查时轮换一次。

## 3. 投递、去重、事件

- WS 地址为 `ws(s)://<host>/v1/ws?token=…`，同时带 `Authorization` 头。每 10 秒发一次 `ping`，35 秒收不到任何帧就主动重连（需短于 Android 模拟器等 NAT 约 20 秒的空闲回收）。建连的 TCP、TLS、WS 握手三步各有 15 秒超时。重连采用指数退避，从 1 秒开始、上限 30 秒，每次等待加 ±12.5% 的抖动。
- 一条信封处理完（解密、去重、落库、发事件）后才会 ack。遇到**临时错误**时不 ack、不记 seen，等待重投：网络错误、未登录，以及服务端 5xx（含 `internal`、无 JSON 错误体的 `http_5xx`）、`rate_limited` / 429、`unauthorized`（见 `HushError::is_transient`）。其他错误（格式错误、无法解密、取对方身份得到 404 等永久错误）先存成「无法解密」占位再 ack，避免同一条消息反复失败。事件同样处理完才 ack，分类规则相同。
- **未 ack 的数据不会等到下次重连**：服务端在同一连接上不会重发，所以出现网络类失败时核心会在 2 秒后主动 HTTP 拉取一次（同一时间只安排一次；连续失败时延迟翻倍，最长 60 秒，处理成功后复位）。HTTP 拉取本身失败时要求当前 WS 连接立即断开重连，由服务端重发（用 `notify_waiters`，没有活动连接时不留存信号，不会让下一条健康连接一建立就被关掉）。
- 关闭码 4001（在别处登录）、收到 `logged_out` 帧，按「已登出」处理：清除 token 和加密状态（保留消息），停止后台任务，发出 `LoggedOut{reason}` 事件，不自动重连。
- **401（C-G）**：任何 REST 接口（经 `Inner::api()` 发出、带 token 的请求）或 WS 握手返回 401，同样处理，`reason = "session_expired"`（服务端令牌 90 天未使用即失效，见 server/API_NOTES.md）。只有返回 401 的请求所用 token 仍是当前登录态时才登出：旧登录态的请求晚到的 401 不会登出新的登录；登录前的接口（没有 token）和 `logout()` 自己的登出请求不触发。多个请求同时 401 只发一次 `LoggedOut`。已知：换设备登录时，旧设备若在收到 4001 之前恰好有 REST 请求被拒，原因会是 `session_expired` 而不是 `logged_in_elsewhere`（服务端对两者都只返回 401）。
- 关闭码 **4000**（同一账号建立了新连接，本连接被替换）**不是登出**：不清密钥、不发 `LoggedOut`，只发 `ConnectionChanged(Disconnected)`，约 3 秒后重试一次；如果重试的连接再次收到 4000，就停止重连（保持登录，状态 `Disconnected`），等平台再调用 `start()`。这样两个进程共用同一账号时不会互相无限挤占，也不会因为一次竞态丢掉加密状态。
- WS 连续失败 3 次后，每次重试前用 `GET /v1/messages` 拉取一次作为兜底，处理后调用 `POST /v1/messages/ack`。平台层也可以主动调用 `sync_now()`。
- **读帧上限**为 `1MB + 64KB`，与服务端推送上限一致。收到超限帧时连接已不可用，而且重连后服务端会重发同一帧，所以不直接重连，而是先用 HTTP 连续拉取（最多 20 页）把积压处理掉并 ack，再按退避重连。
- HTTP 连续拉取（`drain_http`）直到拿到**空页**或某页没有任何 ack 为止（最多 20 页）。服务端每页还受 8MB 负载上限约束，条数少于 `limit` 的短页不代表队列已空。
- HTTP 拉取时信封**逐条解析**：某一条格式错误不会让整页失败；能取到 `id` 的直接 ack 并记日志，取不到 `id` 的忽略。WS 上格式错误的信封同样处理。
- 每次 WS 连上后都会后台同步好友、群、黑名单，并检查 OTK。**好友申请和朋友圈通知不落本地库**，服务端在重新登录后会清空积压事件，所以 UI 登录后应调用 `list_friend_requests()` / `moment_notifications()` 做一次全量同步。
- **去重**分两级，都记在 `seen` 表里，保留 30 天（后台扫描任务启动时清理一次，之后每小时一次）：
  - **信封级**：以 `env:<信封 id>` 为键，在解密**之前**检查。服务端是至少一次投递，ack 丢失时会重投同一信封；Olm 棘轮的消息密钥用过即删，重复解密必然失败，如果不在解密前拦截，就会出现一条「无法解密」占位，还会误触发 session_reset。信封只有在确定 ack 时才标记为已见，网络类临时失败不标记，以便下次重投时重试。
  - **内容级**：按 `(发送者, Content.id)` 去重（键 `msg:<发送者>:<id>`；升级前写入的裸 id 键在 30 天保留期内一并检查）。
  - **消息 id 冲突**：`Content.id` 由发送方决定，不同发送者可能用同一个 id（恶意抢占或巧合）。`MessageItem.id` 通常就是 `Content.id`；若收到的消息与已有的**其他发送者**的消息 id 相同，这条改存为 `<发送者>:<Content.id>`，两条都保留、互不覆盖。平台层对 `react` / `delete_for_everyone` / `download_attachment` / `resend` / `delete_local` / 引用等调用照常传 `MessageItem.id` 即可，核心在发协议消息时自动换回内容 id；收到的回应、撤回、回执按 (内容 id, 作者/发送者) 定位，不会作用到别人的同 id 消息上。收到的 `Content.id` 必须是 1–128 个 `[A-Za-z0-9_-]` 字符（本库生成 UUID），否则丢弃，所以派生 id 无法伪造。
  - **收件箱（C-H，关闭了原来的崩溃窗口）**：收到的信封（WS 或 HTTP）在去重检查之后先原样写入 `inbox` 表；解密成功时，推进后的棘轮状态（Olm 会话、新建会话时的账户、Megolm 入站会话）与明文（库密钥字段加密，AAD `inbox.plaintext`）在**同一个 SQLite 事务**写入。处理完成（或确定为永久失败）后，在一个事务内记 `env:` seen 并删除该行，然后才 ack。处理某个信封时若收件箱里已有它的明文，就直接用明文继续、不再解密。于是进程在「解密之后、落成消息之前」被杀（或任务被 `stop()` 中止）时：`start()` 在建立 WS 之前先处理残留的收件箱行（按积压处理）；即使服务端先重投、或平台先调了 `sync_now()`，按信封 id 加锁后同样找到保存的明文。不会再出现「无法解密」与误发的 session_reset。ack 语义不变：仍是处理完成才 ack，临时错误不 ack、等服务端重投（已解密的明文留在收件箱，重投时直接使用）。处理本身（`handle_incoming`）不在那个事务里，重放时依赖已有的幂等性（消息按 (发送者, id) 去重、回应 / 回执 / room_key 可重复应用）。`wipe_all` 清空收件箱；`wipe_crypto`（被挤下线、同账号重新登录）只删除尚未解密的行（它们已无法用新的加密状态解开），已解密的行保留、登录后继续处理。
  - **并发**：WS 与 `sync_now()` 可能同时拿到同一信封。处理按信封 id 加锁，「查 seen → 解密处理 → 记 seen」在同一临界区内；后到者等前者处理完，看到已处理就直接 ack，不会重复解密（重复解密会失败并误触发 session_reset）。
  - **事件**同样按事件 id 加锁并记入 `seen`（键 `evt:<id>`），重投或 WS/HTTP 同时拿到时只处理一次（避免重复插入「X 加入了群聊」之类的系统消息）。网络类临时错误不记 seen。
- **会话归属**：经 Megolm 收到的内容属于该群。经 Olm 收到的内容如果带 `group_id`，并且发送者是该群成员，就归入群会话（例如群消息的回执），否则归入单聊。

## 4. 消息语义

- **状态**：发出的消息依次经过 `Sending` → `Sent`（服务端接受）→ `Delivered` → `Read`，失败为 `Failed`，可以调用 `resend()` 重发。构造 `HushClient` 时，上次进程残留的 `Sending` 消息一律改为 `Failed`（进程被杀或被系统挂起时发送不可能还在进行），以便重发。收到的消息为 `Delivered`，调用 `mark_read` 后变为 `Read`。**群消息**只要任一成员回执就更新状态（取最高者），不要求全员。
- **回执**：每收到一条可见消息（文字或附件，单聊群聊都算），都自动经 Olm 向原发送者发一条 `delivered`（`urgent=false`）。`mark_read` 按发送者分组发送 `read`，只有 `settings.read_receipts=true` 时才发。
- **输入状态**：`typing_indicators=false` 时既不发送，也不向 UI 转发别人的输入状态。群内的输入状态经 Megolm 发送。
- **撤回**：只能撤回自己发出、24 小时内的消息。接收方校验撤回者就是原发送者，时间差允许 5 分钟时钟误差。撤回后消息保留一条占位：`deleted=true`，body 变为 `System{"此消息已撤回"}`，同时删除本地附件文件。
- **回应**：每人对一条消息最多一个回应，新的覆盖旧的。收到的回应挂在目标所在的本地行上（id 冲突时是派生 id）。`remove=true` 时只有 emoji 与已有回应相同才会删除。
- **引用**：`quote.text` 取被引用消息的预览文本，最长 200 字。附件消息显示为「[图片]」「[文件] 名称」这类文本。
- **阅后即焚**：`set_disappearing` 发送 `timer` 内容（`Content.expire_secs` 与 `body.expire_secs` 相同），双方都插入一条系统消息。发送方从发出时开始计时，接收方从 `mark_read` 开始计时。会话设置按**后写者胜**同步：本地在 kv `timer_at:<会话>` 记下设置最后一次变更的时间；发出的内容带扩展字段 `Content.expire_set_at`（设计文档之外的可选字段，为 0 时不序列化，旧版本忽略）。收到 `timer` 时只采用不早于本地的设置（旧版本发来的 `timer` 以 `Content.ts` 为准）；单聊收到的可见消息只有在 `expire_set_at` 比本地新、且值不同时才同步，并插入系统消息「已与 X 同步阅后即焚设置」。双方同时修改时，对方尚未收到新设置时发来的消息不会再把计时器悄悄改回去。依赖设备时钟大致准确。新建的单聊采用 `settings.default_disappearing_secs`（只影响本地，不会主动通知对方；非 0 时以建会话时间作为设置时间参与同步）。后台每秒扫描一次，删除到期消息，并发出 `MessageRemoved` 事件。
- **附件**：发送时，先把文件复制到 `files/` 作为明文副本 `local_path`（不整文件读入内存），然后插入一条 `Sending` 状态的消息，再按格式 v2 分块流式加密到临时文件 `files/.enc-*`（阻塞线程池上执行，内存只有两块 64 KiB 缓冲），以流式请求体上传（显式带 `Content-Length`，服务端据此预占配额），最后发送。接收方调用 `download_attachment` 时把密文流式写入 `files/.dl-*` 并同时计算 SHA-256，digest 相符后流式解密到 `files/.dec-*`，成功后改名为 `files/<sha256(消息id) 前 16 字节的十六进制>_<序号>_<文件名>`（任何一步失败都不留下明文文件；临时文件出错即删，进程被杀遗留的由每小时清理删除）。v1 旧附件整文件读入内存解密。缓存文件名用哈希而不是替换非法字符后的 id，派生 id `bob:X` 与别人构造的内容 id `bob_X` 不会映射到同一个缓存文件。头像和朋友圈图片是明文，缓存在 `files/media/<附件id>`。
- **系统消息**的 `sender_id` 为空串。通话记录消息的 `sender_id` 是主叫方。
- 某条消息正文无法解密（坏扇区、写到一半）时，该条显示为「无法解密」，同页其他消息、会话列表与搜索不受影响。
- **设置**：kv 中的设置记录无法读取（损坏、格式变化）时按隐私优先处理：已读回执与输入状态视为关闭，其余取默认值。

## 5. 群（Megolm）

- **群信息应用**：`apply_group` / `on_group_removed` 按群加锁串行执行；`updated_at` 早于本地记录的群快照直接丢弃（连接时的 `refresh_groups` 与 WS 上的 `group_updated` 可能并发到达），不会让被移出的成员回来，也不会重复插入「X 加入/离开了群聊」。
- 以下情况会轮换出站会话：首次发送、有成员被移除或退出（对比 `group_updated` 前后的成员表，或本端执行 `remove_member` 时立即轮换）、已发 100 条、会话创建超过 7 天。
- 删除出站会话（`group_updated` 成员减少、`group_removed`、`remove_member`）与发送一样在 `crypto_lock` 下进行，不会被正在发送的 `send_group` 写回。另外 `send_group` 在锁内读取成员表，**只要出站会话的已分发名单里有人已不在成员表中就强制轮换**，作为兜底。
- 每次发送前，先给还没拿到当前会话密钥的成员经 Olm 发 `room_key`（`urgent=false`），发送成功的才记为已分发。其中还没有 Olm 会话的成员，在 `crypto_lock` 之外并发（最多 8 路）预先 claim 密钥，锁内只建会话与加密；单聊的 `send_olm_batch` 同样如此。之后再发群消息。新成员只能解密加入之后的消息，因为导出的 session_key 从当前棘轮位置开始。
- room_key 对某成员加密（claim 失败）或发送失败时，**立即用同一个 session_key 重新加密并重试一次**；仍失败就记一条警告日志（含消息 id 与成员），这些成员暂时解不开该条消息（在对方那里挂起），他们也不会记入已分发名单，下一次发送时会再发 room_key。
- 收到 room_key 后**逐条**处理对应的挂起消息：单条出错不影响其余；网络类错误保留挂起记录，其他错误（例如密文损坏）写成「无法解密」占位并删除挂起记录。
- 入站会话按 `(sender, session_id)` 存储。只有当 room_key 的发送者是群成员时才接受：按本地群信息不是成员（本地可能过时，例如 room_key 先于 `group_updated` 到达）时先刷新一次群信息再判断；服务端返回 404/403（我们不在群里）时拒绝；本地没有该群且刷新遇到临时错误时先接受（服务端只转发成员发出的群消息，非成员注入的会话没有可解的消息）。刷新失败不会让信封不 ack，因为此时 Olm 棘轮已推进，重投将无法解密。缺少密钥的群消息先挂起并 ack；收到对应的 room_key 后立即重试，成功后删除挂起记录。
- 同一会话再次收到 room_key（发送方重试、并发分发）时，**首个已知序号更小的密钥胜出**（vodozemac `merge`，要求是同一棘轮），不再先到先得。群消息序号早于本地密钥起点（`UnknownMessageIndex`）时也按「缺密钥」挂起，而不是写成无法解密，等更早位置的 room_key 到达后再解。
- 同一群的 `send_group` 串行执行（按群加锁），room_key 分发与群消息按调用顺序进行。
- 群内的回执经 Olm 单独发给原发送者，输入状态经 Megolm 发给全群。
- 本端被移出或群解散时，群标记为 `active=false`，出站会话删除，并插入系统消息「你已不在此群聊中」。会话本身保留。

## 6. 通话

- **结束原因（C-B）**：通话记录带 `reason: CallEndReason`。已接通后结束 → `Completed`；未接通时：对方忙线（呼出收到 `busy`）→ `Busy`；`reject`（任一方）→ `Rejected`；`hangup`（接通前）：呼出一侧（本端取消或被叫响铃超时）→ `Cancelled`，呼入一侧（主叫取消或本端响铃超时）→ `Missed`；明确拒接只由 `reject` 信令表达；呼入时本端忙线自动回 `busy` → `Missed`；呼叫 `offer` 本身发送失败 → 记一条 `Failed`（呼出）。`missed` 字段与之保持一致（`reason == Missed`，只有它计未读；本端拒接的来电不再算未接）。旧记录没有 reason：v3 迁移按「duration>0 → Completed，missed → Missed，其余 → Cancelled」写回，读取时同一规则兜底。
- `send_call_signal` 经 Olm 发送 `call` 内容。`ice` 信令 `urgent=false`，其余为 `true`。收到信令后发出 `CallSignalReceived` 事件。
- **过期来电**：`offer` 的服务端时间戳（缺失时用发送方时间）距今超过 60 秒（例如离线期间积压、重连后才送达）时不发 `CallSignalReceived`，不会响铃；该通话仍被跟踪，对方随后的 `hangup` 会生成一条未接来电记录。依赖设备时钟大致准确。
- 核心在内存里跟踪 `call_id`：收发 `offer` 时开始记录，收发 `answer` 时记下接通时间，收发 `hangup` / `busy` / `reject` 时结束，并在双方的单聊中各写一条 `CallLog` 消息。只有该通话的对端发来的 `answer` / `hangup` / `busy` / `reject` 才会改变跟踪状态（第三方即使知道 `call_id` 也不能结束别人的通话）。开始超过 2 小时仍未结束的跟踪会被清理，不再写通话记录。`duration_secs` 从接通算起。`missed` 表示呼入且未接通，此时会话未读数加 1。
- TURN 凭证原样透传：`ttl` 与 `username` 里的过期时间单位为秒（见 server/API_NOTES.md）。

## 7. 本地存储

- 数据库文件是 `data_dir/hush.sqlite`（WAL 模式）。库密钥必须是 32 字节。首次打开时写入一个校验值，以后用错误的密钥打开会直接报错，避免写乱数据。
- **表结构迁移**按 `PRAGMA user_version` 进行（`store/mod.rs` 的 `MIGRATIONS`，下标 + 1 即版本号，只允许在末尾追加）。迁移分两种：`Sql`（不需要库密钥）和 `Data`（要解密 / 重新加密已有数据）。打开顺序：v0 的库先执行 v1 建表 → **校验库密钥**（不符返回 `KeyMismatch`，此时不执行任何后续迁移、不改写数据）→ 执行其余迁移。当前版本为 **4**：
  - v1：引入迁移框架前的完整表结构（全部 `IF NOT EXISTS`），旧库（`user_version=0`）打开时无损升级。
  - v2：`inbox(envelope_id, envelope, plaintext, received_at)`（C-H）。
  - v3（数据迁移）：旧通话记录补上结束原因（C-B，规则见第 6 节）；正文无法解密的行跳过（读取时兜底）。这一步要把每条消息正文解密一次（逐行进行，内存只保留要改写的通话记录），升级后首次打开会慢一些，消息很多时平台应在后台线程打开库。
  - v4（v3 收尾，K3/K5/K6）：`messages.mentions_me` 列与部分索引 `messages_mentions`（未读提及计数）、`drafts(conv_id, text, updated_at)` 表（草稿字段加密，AAD `drafts.text`）；已发送消息（`outgoing=1 AND server_ts>0`）的 `sort_ts` 回填为 `server_ts`；然后逐会话把重复的 `sort_ts` 依次错开 1ms，使同一会话内 `sort_ts` 唯一。不解密正文，很快。
  - 附件元数据的 `format`（C-I）不需要改库：缺省值 1 由反序列化补上。
  每一步迁移在一个事务内执行，失败则回滚、版本不变。库版本高于核心支持的版本时拒绝打开（不支持降级）。
- **库密钥不符（C-E）**：`HushClient::new` 返回 `HushError::KeyMismatch { message: "db_key 与已有数据库不匹配" }`。其他存储错误（包括单个字段解密失败）仍是 `Storage`，平台层只应在 `KeyMismatch` 时考虑重置本地数据。
- **资料缓存与邮箱（C-C / K2）**：资料以 JSON 明文缓存在 `profiles` 表，可能包含好友的邮箱。邮箱只对本人和互为好友可见，而很多来源按契约不带邮箱。写缓存的路径分两类：
  - **权威**（邮箱以其结果为准，没有就清掉）：`/v1/users/{id}`（`get_user`，包括收到陌生人消息后的补取、`friend_accepted`）、`/v1/me`（登录）、`/v1/friends`（`refresh_friends`，不再是好友的用户清掉邮箱）、删除好友 / 收到 `friend_removed`（清掉）。
  - **非权威**（`Store::put_profile`：邮箱**一律沿用缓存**，既不清掉也不改写，其余字段照常更新）：邮箱搜索、好友申请（发送、列表、`friend_request` 事件）、`profile_updated` 事件、朋友圈作者 / 点赞者 / 评论者、朋友圈通知、修改资料（`PUT /v1/me/profile`）的返回。
  - `search_user_by_email` 返回合并后的资料（带上缓存里的邮箱，与 `cached_profile` 一致）。群成员列表只有 user_id，不写资料缓存。
- 加密的字段：消息正文与引用（整条 JSON，其中含附件 key 和 digest），用 AES-256-GCM 加密，AAD 为字段标签；登录态（token）和设置，同样 AES-GCM 加密；vodozemac 的 Account、Session、GroupSession、InboundGroupSession pickle，用 vodozemac 自带的 pickle 加密，密钥也是库密钥。
- 明文存储的字段：会话元数据（时间、未读数、置顶等）、资料缓存、群信息、回应 emoji、消息时间戳与状态、身份公钥。
- `logout()` 会调用服务端登出、停止后台任务、清空全部本地数据和 `files/`。

## 8. 与设计文档第 5 节的差异

1. 构造函数 `new(...)` 返回 `Result`：db_key 不是 32 字节、密钥与已有库不匹配、目录不可写时都会报错。
2. 所有可能失败的方法都返回 `Result<_, HushError>`，包括 §5 里写成同步、没写返回类型的 `friends`、`blocked`、`conversations`、`conversation`、`messages`、`search_messages`、`delete_local`、`clear_conversation`、`set_pinned`、`set_muted`、`set_archived`、`group_info`、`set_verified`、`set_settings`。
3. `start()` 返回 `Result`，未登录时返回 `NotLoggedIn`。已启动且 WS 任务仍在运行时是空操作；WS 任务已退出（例如被同账号新连接两次挤下线后停止重连）时会重新拉起全部后台任务。
4. `refresh_groups()` 返回 `Vec<GroupInfo>`（§5 无返回值）。
5. `update_group`、`add_members`、`set_admin` 返回最新的 `GroupInfo`（§5 无返回值，服务端本来就返回 Group）。
6. `verify_safety_qr` 返回 `Result<bool>`。它只使用本地已有的对端身份，所以要先调用过 `safety_number` 或与对方通信过，否则返回 `NotFound`。核验一致时会**自动**把对方标记为已核验。
7. `set_verified` 在本地没有对端身份时返回 `NotFound`。
8. `my_qr_payload()` 未登录时返回空串。
9. `send_attachment` 的 `caption` 类型为 `Option<String>`。`AttachmentMeta = {file_name?, width, height, duration_ms, thumbnail?: bytes}`，`file_name` 为空时取文件路径的文件名。
10. 新增方法：`sync_now() -> u32`（HTTP 拉取一次）、`resend(message_id) -> MessageItem`（重发失败消息）、`groups() -> Vec<GroupInfo>`（本地群列表，含已退出的群）、`cached_profile(user_id) -> Option<Profile>`（只读本地缓存）。
11. `EventListener` 使用 `#[uniffi::export(foreign)]` 导出。在 UniFFI 0.32 中，`with_foreign` 是 `rust, foreign` 的已废弃别名，而这里只需要平台层实现。
12. §5 没有定义的类型，本库定义如下：
    - `ConnectionState = Disconnected | Connecting | Connected`
    - `MyAccount = {user_id, email, server_url, profile, is_new}`
    - `FriendRequests = {incoming, outgoing}`，`FriendRequest` 与契约相同
    - `ConversationSummary = {id, kind: Direct|Group, peer_user_id?, group_id?, title, avatar_id?, last_message?, last_activity_ts, unread_count, pinned, muted, archived, disappearing_secs, active, draft?, unread_mentions}`（后两项 v3）
    - `GroupInfo` = 契约中的 `Group` 加上本地字段 `active`
    - `SafetyNumber = {digits, qr_payload, verified}`
    - `Visibility = {mode: All|Include|Exclude, user_ids}`
    - `CallSignal = {call_id, action: Offer|Answer|Ice|Hangup|Busy|Reject, video, sdp?, candidates: [IceCandidate{sdp, sdp_mid?, sdp_mline_index?: i32}]}`
    - `Settings = {read_receipts, typing_indicators, default_disappearing_secs, notifications}`
    - `AttachmentItem = {id, mime, file_name, kind, size, width, height, duration_ms, thumbnail?, local_path?}`：不暴露附件密钥和 digest
    - `ReactionGroup = {emoji, user_ids}`
13. `MessageItem.server_ts` 是 `i64`，值未知时为 0，不是 Option。`MessageItem.status` 使用 `MessageStatus` 枚举。`MessageItem.sort_ts`（v2）是本地排序时间戳：收到的消息取服务端时间（缺失时取收到时间），发出的消息在服务端接受后取服务端返回的 `server_ts`（v3 / K3；发送中和失败的取发送时间），系统提示取插入时间；同一会话内 `sort_ts` 唯一（同一毫秒的依次错开 1ms）。`messages(conv, before_ts, limit)` 返回 `sort_ts < before_ts` 的最近 `limit` 条，翻页时传上一页最早一条的 `sort_ts`，不漏不重。
14. `moment_notifications()` 与 §5 一致，不带分页参数（内部支持 `before`，暂未导出）。
15. `moments_feed(before)` / `user_moments(uid, before)` 的签名不变。平台翻页时传上一页最后一条的 `created_at`；核心记得上一页最后一条的 id，若 `before` 与它的时间相同就自动附带服务端的 `before_id` 复合游标，同一毫秒的动态跨页时不会漏。

## 9. 已知限制

- 只支持单设备（与契约一致）。
- 挂起的群消息如果一直收不到 room_key，就一直挂起，UI 不可见，也没有超时占位。
- 群消息状态只要任一成员回执就会更新。每个成员各自的已读情况没有对外暴露。
- 本地搜索会逐条解密后做子串匹配，消息量很大时较慢。
- 通话跟踪只存在内存里，进程重启后进行中的通话不会生成通话记录。
- 下载后的附件以明文缓存在 `files/`，依赖平台的文件保护（Android 应用私有目录、iOS Data Protection）。
- 附件上传和下载没有进度回调。v2 附件流式处理，内存与大小无关；v1 旧附件下载时仍整文件解密（约一份文件大小）。附件收发需要约两份文件大小的临时磁盘空间（明文副本 / 缓存 + 密文临时文件）。
- `default_disappearing_secs` 只作用于本地新建的单聊，不会自动向对方发送 `timer` 内容。
- **事件回调**：核心内部 `emit` 只把事件放入队列，由专门线程 `hushcore-events` 按顺序在所有内部锁之外调用 `on_event`，所以回调里调用核心方法（包括同步等待 async 方法）不会死锁，但会阻塞后续事件的投递，仍建议尽快返回。回调抛异常（UniFFI 转成 Rust panic）被 `catch_unwind` 兜住，只记日志。因为是异步投递，事件可能比对应的本地状态变化稍晚到达。
- 交叉编译环境：本机 `~/.cargo/bin` 下没有 rustup 代理，`rustup run stable cargo` 仍会用到 Homebrew 的 rustc。需要设置 `RUSTC=$(rustup which rustc)`，见 README。
- **后台任务**（session_reset、送达回执、重投拉取、连接后的初始同步、OTK 补充）派生时记下当前登录态的代数（token 每变化一次加 1），执行前与每个网络步骤前确认仍是同一次登录；`stop()` 不会中止已在进行中的单个 HTTP 请求。

## 10. 代码组织与测试开关

- `client/`：`mod.rs`（状态与通用工具）、`account.rs`（登录、资料、好友）、`keys.rs`（账户密钥上传、OTK、fallback）、`identity.rs`（TOFU 身份、安全码）、`secure.rs`（Olm / Megolm 收发与 room_key）、`messaging.rs`（发送侧）、`receive.rs`（接收处理）、`attachments.rs`、`groups.rs`、`social.rs`、`calls.rs`、`sync.rs`（WS 循环、HTTP 拉取、重投）、`hooks.rs`（测试钩子）。`store/`：`mod.rs`（表结构、迁移、类型、kv）、`sessions.rs`、`social.rs`、`messages.rs`。
- Rust 侧只公开 `HushClient`、`types`、`error`，以及 e2e 测试用到的 `api` 与 `ws`；其余模块为 crate 内可见。
- cargo 特性 **`test-hooks`**：`HushClient::debug_*`、`DebugStats`、故障注入、最近信封记录、可调读帧上限只在该特性（或单元测试）下编译。`Cargo.toml` 用自引用 dev-dependency 让 `cargo test` 与示例构建自动启用；`scripts/build-core-*.sh` 的 `cargo build --lib` 不启用，发布产物不含这些接口。

## 11. 接口 v2 变更（2026-09-24，`docs/superpowers/specs/2026-09-24-api-v2.md` 第 2 节）

两端需要同步适配的 UniFFI 导出变化（只有新增，没有改动或删除已有导出）：

| 编号 | 导出变化 | 说明 |
|---|---|---|
| C-A | `MessageItem.sort_ts: i64` | 与 `messages(before_ts)` 游标同一口径（见第 8 节 13）。 |
| C-B | `enum CallEndReason { Completed, Missed, Rejected, Busy, Cancelled, Failed }`；`MessageBody::CallLog` 新增 `reason` | 规则见第 6 节；旧记录经 v3 迁移补齐。 |
| C-C | `Profile.email: Option<String>` | 仅本人 / 互为好友时有值；缓存规则见第 7 节。`verify_code` 返回的 `MyAccount.profile.email` 也有值。 |
| C-D | `async fn moment(id) -> Moment` | `GET /v1/moments/{id}`；已删除 / 不可见（404）→ `HushError::NotFound`。作者、点赞、评论者资料进缓存。 |
| C-E | `HushError::KeyMismatch { message }` | 只在 `HushClient::new` 库密钥与已有库不符时返回。 |
| C-F | `CoreEvent::MessageAdded.backlog: bool` | 收到的消息（含「无法解密」占位、room_key 到达后解开的挂起群消息）：WS 连接建立后、`queue_empty` 之前处理的，以及 HTTP 补拉（`sync_now`、重投拉取、兜底轮询）和启动时处理的残留收件箱为 `true`；之后实时推送的为 `false`。本地发出的消息、系统提示、通话记录一律为 `false`。 |
| C-G | 行为 | REST / WS 握手 401 → `LoggedOut { reason: "session_expired" }`（第 3 节）。 |
| C-H | 内部 | 收件箱（第 3 节）。 |
| C-I | 内部 | 附件格式 v2（第 1 节、第 4 节）；`Content` 附件项新增 `format`（缺省 1）。`AttachmentItem` 不变。 |

测试：`store/tests.rs`（构造 v1 旧库 → 打开 → 数据完好、reason 已写回、format=1；错误密钥 → `KeyMismatch` 且不迁移）、`client/sync.rs`（注入「解密后崩溃」→ 重开同一数据目录 → 重投 / 启动处理残留均得到原消息，另有对照用例证明没有保存明文时会变成「无法解密」）、`crypto/attachment.rs`（20MB 文件往返、篡改 / 截断 / 删最后一块 / 追加 / 调换块失败、v1 兼容）、`tests/e2e.rs`（真实服务端：邮箱可见性、单条动态、20MB 附件、backlog、`--token-ttl 6s` 下 REST 与 WS 握手 401）。测试注入 `debug_crash_after_decrypt(n)`、`debug_inbox_count()` 只在 `test-hooks` 特性下编译。

## 12. v3 变更（2026-09-25，`docs/superpowers/specs/2026-09-25-v3-polish.md` 第 1 节）

两端需要同步适配的 UniFFI 导出变化（只有新增；已有导出只在 Record / 事件末尾追加字段）：

| 编号 | 导出变化 | 说明 |
|---|---|---|
| K1 | `fn delete_conversation(conv_id)` | 删除该会话的全部本地消息、回应、附件缓存文件（`files/` 下的明文副本）、草稿与会话行，发 `ConversationsChanged`（不逐条发 `MessageRemoved`）。群会话不退群，群信息与群密钥保留；删除时还在等 room_key 的该群挂起消息一并丢弃（否则密钥到达后旧消息会让会话带着旧内容重新出现）；之后收到新消息（含系统提示）时会话重新出现，只有新消息。会话 id 非法报 `InvalidInput`，会话不存在是空操作。补充：删除前设为免打扰的会话，重新出现时沿用免打扰（kv `muted_hint:<会话>`）；置顶、归档、阅后即焚设置不保留（阅后即焚按第 4 节规则与对方重新同步）。 |
| K2 | 行为 | 邮箱缓存规则见第 7 节：只有 `/v1/users/{id}`、`/v1/me`、`/v1/friends` 与删好友能改写缓存邮箱，其余来源一律沿用。 |
| K3 | 行为 | 发出的消息被服务端接受后，`sort_ts` 改为服务端返回的 `server_ts`（单聊取 `POST /v1/messages` 的结果，群聊取 `POST /v1/groups/{gid}/messages` 的 `server_ts`），发 `MessageUpdated` 与 `ConversationsChanged`；发送中 / 失败的保持本地时间，重发成功后同样改为服务端时间。这几列只由 `Store::mark_sent` 修改：`update_message` 不写 server_ts / sort_ts，所以并发的回执、已读、下载写回整行时不会把旧的排序时间写回去。同一会话内 sort_ts 唯一（见第 8 节 13），v4 迁移回填并去重。 |
| K4 | `async fn forward_message(message_id, conv_ids: Vec<String>) -> Vec<MessageItem>`；`MessageItem.forwarded: bool`；`Content.forwarded`（可选，缺省 false 时不序列化） | 源消息须是未撤回的文字或已上传的附件，否则 `InvalidInput`（不存在为 `NotFound`）；任一目标会话 id 非法时整体报 `InvalidInput`，不发送。目标按出现顺序去重，每个目标一条新消息（新 id，用目标会话的阅后即焚设置），返回顺序与去重后的 `conv_ids` 一致。附件**复用原密文的 id / key / digest / format**，不重新加密、不重新上传；原消息有本地明文副本时硬链接（失败则复制）到新消息自己的缓存路径，否则留空，需要时 `download_attachment`。引用、回应、@提及不转发，附件说明保留。先在本地插入全部新消息，再依次发送：单个目标失败（例如已不在该群、网络错误）只把那一条置为 `failed`（可 `resend`），不影响其他目标，函数本身仍返回 `Ok`。转发不清空目标会话的草稿。接收方下载的是同一个服务端附件：服务端按「最后访问时间」90 天回收附件（`AttachmentIdleTTL`），源附件已被回收时，转发出去的消息在接收方下载会得到 `NotFound`。 |
| K5 | `fn set_draft(conv_id, text: Option<String>)`；`ConversationSummary.draft: Option<String>` | 草稿存在 `drafts` 表，用库密钥 AES-GCM 字段加密（AAD `drafts.text`）。`None` 或空白清除；内容没变时不刷新时间、不发事件，否则发 `ConversationsChanged`；保存草稿时会话不存在则创建。`send_text` / `send_text_with_mentions` / `send_attachment` 在插入消息之前清空该会话的草稿（发送失败也已清空）。排序：置顶优先，其余按「最后消息时间」与「草稿更新时间」中较晚者；`last_activity_ts` 取同一个值，与排序一致。草稿密文损坏时视为没有草稿。 |
| K6 | `record Mention { user_id, start: u32, length: u32 }`；`async fn send_text_with_mentions(conv_id, text, quote_id?, mentions)`；`MessageItem.mentions: Vec<Mention>`；`ConversationSummary.unread_mentions: u32`；`CoreEvent::MessageAdded.mentions_me: bool` | `send_text` 不变（等价于 mentions 为空）。偏移单位是 **UTF-16 码元**。线上格式：文字内容的 `body.mentions: [{user_id, start, length}]`（为空时不序列化）。发送和接收都校验：`user_id` 为空、`length=0`、`start+length` 超过文本 UTF-16 长度、起止切开代理对的项**直接丢弃，不报错**；同一区间重复的只保留一个，结果按 `start` 排序。收到的消息提及了我时记 `messages.mentions_me=1`，`MessageAdded.mentions_me=true`（免打扰会话是否通知由平台决定，契约要求通知）。`unread_mentions` 是该会话里未读、未撤回、提及我的收到消息数（实时统计），`mark_read` 后为 0，阅后即焚删除、撤回、清空 / 删除会话后自然减少。自己发出的消息 `mentions_me` 恒为 false。只有文字消息带提及，附件说明不带。 |

兼容性：`Content` / `ContentBody` 都没有 `deny_unknown_fields`，内部标签枚举也忽略未知字段，所以旧版本收到带 `forwarded` / `mentions` 的内容时照常解析为普通文字或附件（`content.rs` 的 `optional_v3_fields_and_unknown_fields_tolerated` 覆盖）。

测试：`store/tests.rs`（构造 v3 旧库 → 打开 → sort_ts 回填、重复值错开、失败消息不动、数据完好；mark_sent 之后逐页翻完不漏不重；草稿加密与排序；删除会话）、`content.rs`（UTF-16 区间校验、未知字段）、`client/messaging.rs`（删除群会话后附件文件被删、群有新消息时重新出现；草稿；提及计数与 mark_read 清零；发送侧区间校验；转发复用密文、独立缓存路径、失败语义）、`client/sync.rs`（邮箱缓存不被搜索和事件改写）、`tests/e2e.rs::e2e_v3_polish`（真实服务端：K2；单聊双方、群三方顺序一致且 sort_ts = server_ts；@提及计数与 `mentions_me`；转发附件复用密文 id、接收方下载解密成功、不存在的群那一条为 failed；草稿；删除群会话后重新出现）。

示例机器人（`examples/bot.rs`）：群消息 `mentions_me` 为 true 时回复「@对方 收到@」并提及对方，便于两端联调 @提及。
