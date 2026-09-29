# iOS 客户端联调发现的问题

记录 iOS App 接入 hushcore / hush-server 时发现的问题、复现步骤与 App 层的规避方式。

## 1. syncNow() 与 WS 并发处理积压，可能重复处理同一信封（已由核心修复）

- 来源：协调方审查告知（2026-09-24）。
- 现象：`start()` 之后再调用 `syncNow()`，WS 循环与 HTTP 拉取同时处理积压，同一信封可能被处理两次，导致「无法解密」占位并误触发 session_reset。
- App 规避：修复版到来前，回前台只调用 `start()`，不调用 `syncNow()`。
- 状态：核心修复版（05:03 重建的 xcframework）已修复，App 已恢复回前台 `start()` + `syncNow()`。
- 同时按修复版说明处理：连接被同账号新连接替换（4000）后核心保持登录但不再重连，App 在前台收到 `ConnectionChanged(Disconnected)` 5 秒后若仍未连接会再次调用 `start()`（幂等）。

## 2. MessageItem 没有导出排序时间 sort_ts，分页需要 App 自行推算（已在接口 v2 解决：C-A / P-A）

- `messages(conv_id, before_ts, limit)` 的 `before_ts` 与库内 `sort_ts` 比较，而 `sort_ts` 规则是：发出的消息取 `sent_at`，收到的消息取 `server_ts`（为 0 时取本地接收时间）。`MessageItem` 只有 `sent_at` / `server_ts`，收到消息且 `server_ts=0` 时 App 无法得到准确的 `sort_ts`。
- App 规避：`sortTs = !isOutgoing && serverTs > 0 ? serverTs : sentAt`，上翻页结果按 id 去重。极端情况下可能漏掉或重复一两条。
- 建议：`MessageItem` 增加 `sort_ts` 字段。

## 3. 通话记录缺少结束原因（已在接口 v2 解决：C-B / P-B）

- 复现：给机器人发「打电话」，在来电页点「拒绝」；或主动呼叫机器人（机器人回 busy）。
- 现象：被叫主动拒绝也记为 `missed=true`（UI 显示「未接语音通话」）；主叫遇到 busy / reject / 自己取消时都是 `duration_secs=0, missed=false`，App 无法区分「对方忙线」「对方已拒绝」「已取消」。
- App 规避：通话页根据信令实时显示「对方忙线中 / 对方已拒绝 / 对方已挂断」；聊天记录里的通话消息统一显示「语音通话 已取消」。
- 建议：`CallLog` 增加 `reason`（busy / rejected / cancelled / timeout / failed）。

## 4. 测试机器人来电的 SDP 不是合法 SDP（仅影响联调）

- 机器人「打电话 / 视频」发出的 offer `sdp` 为 `"v=0\r\n"`。App 可正常弹出来电页；若点击「接听」，`setRemoteDescription` 必然失败，App 会发送 hangup 并提示「通话建立失败」，不会崩溃。
- 因为没有可用的 WebRTC 对端，模拟器上无法验收真正接通后的「通话中」计时与音视频（UI 已实现，见 CallView）。

## 5. 朋友圈通知没有已读状态，也没有按 id 取单条动态的接口（单条动态已在接口 v2 解决：S-A / C-D / P-D；通知已读状态仍由 App 本地记录）

- `moment_notifications()` 只返回列表，App 用本地保存的「已看到的最大 created_at」计算「n 条新消息」。
- 没有 `moment(id)` 接口，通知列表点击后无法跳转到具体动态（目前只展示通知内容）。

## 6. 新账号的默认昵称

- `bob@hush.local` 首次登录时 `is_new=true`，但 `profile.name` 已是邮箱前缀 `bob`。资料页预填了该昵称；自动化测试直接在输入框追加输入时得到了 `bobBob`，已在「编辑资料」里改回 `Bob`。不算缺陷，记录以免误会。

## 7. 环境说明

- iOS 模拟器原生控制工具需要 `sudo xcode-select -s /Applications/Xcode.app/Contents/Developer` 才能使用，本次无法执行，改用 XCUITest（`ios/HushUITests`）驱动完整流程并截图。
- 未签名的模拟器构建若访问 Keychain 失败（-34018），`KeyStore` 会退回到 Application Support 下带文件保护的 `.dbkey` 文件保存数据库密钥。
