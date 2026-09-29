# hush-server

Hush（密语）的 Go 单体服务端：账号、预密钥仓库、消息中继与离线队列、好友/群成员、朋友圈、附件、内嵌 TURN。
存储为 SQLite（纯 Go 驱动 `modernc.org/sqlite`，WAL 模式，启动时自动建表迁移），附件存放在 `data/attachments/<id>`。

## 快速开始

```bash
# 仓库根目录：构建到 server/bin/hush-server 并以开发模式启动，默认监听 127.0.0.1:8080（验证码固定 123456）
./scripts/run-server.sh

# 真机局域网调试：监听全部网卡（此时固定验证码不生效，见下文）
HUSH_ADDR=0.0.0.0:8080 ./scripts/run-server.sh

# 或手动运行
cd server
HUSH_DEV_CODE=123456 go run ./cmd/hush-server --dev --addr 127.0.0.1:8080 --data ./data
```

**开发验证码 `HUSH_DEV_CODE` 只在同时满足「加了 `--dev`」与「`--addr` 是回环地址（`127.0.0.1` / `::1` / `localhost`）」时生效**，
否则启动时打印一条警告并忽略它。原因：固定验证码对任意邮箱永远有效，一旦监听到局域网或公网，任何人都能登录任何账号。
监听非回环地址调试时，可以配置 SMTP，或者在服务端日志里查看验证码（`--dev` 下每次生成的验证码都会打印，邮箱打码显示）。

冒烟验证：

```bash
curl -s -XPOST localhost:8080/v1/auth/verify \
  -d '{"email":"me@example.com","code":"123456","device_name":"curl","platform":"android"}'
# → {"is_new":true,"token":"…","user_id":"…"}
curl -s localhost:8080/v1/me -H "Authorization: Bearer <token>"
```

客户端地址：Android 模拟器用 `http://10.0.2.2:8080`（模拟器把它转发到宿主机回环），iOS 模拟器用 `http://127.0.0.1:8080`，
这两种都能访问只监听 `127.0.0.1` 的服务端。真机需要服务端监听局域网（`HUSH_ADDR=0.0.0.0:8080`），客户端填本机局域网 IP。

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--addr` | `:8080` | HTTP 监听地址 |
| `--data` | `./data` | 数据目录（`hush.db`、`attachments/`、`turn_secret`） |
| `--turn-port` | `3478` | 内嵌 TURN/STUN 的 UDP 端口；`0` 表示不启动 |
| `--public-ip` | `127.0.0.1` | TURN 中继地址（其他设备可达的本机 IP） |
| `--token-ttl` | `2160h`（90 天） | 登录令牌滑动过期：超过这么久未使用即失效（401，需重新登录）；每次使用刷新，写库节流为每小时一次；`0` 表示永不过期（测试用） |
| `--dev` | 关 | 开发模式：验证码打印到日志；允许 `HUSH_DEV_CODE`（仅回环监听时）；TURN 允许中继到回环/私有地址（生产模式禁止） |

## 环境变量

| 变量 | 说明 |
|---|---|
| `HUSH_DEV_CODE` | 固定验证码，对任意邮箱永远有效。**仅在 `--dev` 且监听回环地址时生效**，否则忽略并告警 |
| `HUSH_SMTP_HOST` / `HUSH_SMTP_PORT` | 配置后通过 SMTP 真实发送验证码邮件；端口默认 587（STARTTLS），465 为隐式 TLS |
| `HUSH_SMTP_USER` / `HUSH_SMTP_PASS` | SMTP 认证（PLAIN） |
| `HUSH_SMTP_FROM` | 发件人地址，缺省使用 `HUSH_SMTP_USER` |
| `HUSH_TURN_SECRET` | TURN REST 凭证的共享密钥；未设置时随机生成并持久化到 `data/turn_secret` |

未配置 SMTP、也没有加 `--dev` 时，申请验证码会返回错误，此时无法登录（`HUSH_DEV_CODE` 在非 `--dev` 下也不生效）。

## 接口

完整接口契约见设计文档第 3 节：[docs/superpowers/specs/2026-09-24-hush-design.md](../docs/superpowers/specs/2026-09-24-hush-design.md#3-服务端接口契约hush-server)。
契约没写到、由服务端自行确定的细节（错误码、边界行为、`GET /v1/messages` 响应格式等）见 [API_NOTES.md](API_NOTES.md)。

| 模块 | 接口 |
|---|---|
| 账号 | `POST /v1/auth/code` `POST /v1/auth/verify` `POST /v1/auth/logout` `GET /v1/me` `PUT /v1/me/profile` `GET /v1/users/{id}` `GET /v1/users/search?email=` |
| 预密钥 | `PUT /v1/keys` `GET /v1/keys/count` `POST /v1/keys/claim/{user_id}` `GET /v1/keys/identity/{user_id}` |
| 消息 | `POST /v1/messages` `GET /v1/messages` `POST /v1/messages/ack` `POST /v1/groups/{id}/messages` `GET /v1/ws` |
| 好友 | `POST/GET /v1/friends/requests` `POST /v1/friends/requests/{id}/accept\|reject` `GET /v1/friends` `DELETE /v1/friends/{user_id}` `GET /v1/blocks` `POST/DELETE /v1/blocks/{user_id}` |
| 群组 | `POST/GET /v1/groups` `GET/PUT /v1/groups/{id}` `POST /v1/groups/{id}/members` `DELETE /v1/groups/{id}/members/{uid}` `PUT /v1/groups/{id}/admins/{uid}` |
| 附件 | `POST /v1/attachments` `GET /v1/attachments/{id}` |
| 朋友圈 | `POST /v1/moments` `GET /v1/moments/feed` `GET /v1/moments/user/{uid}` `GET/DELETE /v1/moments/{id}` `POST/DELETE /v1/moments/{id}/like` `POST /v1/moments/{id}/comments` `DELETE /v1/moments/comments/{cid}` `GET /v1/moments/notifications` |
| 通话 | `GET /v1/calls/turn` |
| 其他 | `GET /healthz` |

## 代码结构

```
cmd/hush-server/     入口：参数、环境变量、启动 HTTP 与 TURN
internal/store/      SQLite 数据访问与迁移（users、tokens、keys、queue、friends、groups、moments…）；store.Tx 是业务事务（outbox）
internal/api/        HTTP handlers（按契约章节分文件）、鉴权、统一错误体、请求日志
internal/ws/         在线连接管理与投递：按接收方持久化队列 → 在线即推送 → ack 删除
internal/turn/       内嵌 pion/turn（UDP）与 TURN REST 时限凭证
internal/mail/       验证码邮件（日志 / SMTP）
internal/push/       离线推送接口 Pusher（本地部署只有日志实现）
```

投递语义：信封与事件都按接收方写入 `queue` 表；事件与产生它的业务变更在同一个数据库事务里写入（outbox），提交后才唤醒在线连接。WebSocket 连接建立后，先按顺序推送积压，再发 `queue_empty`，之后新入队的数据会唤醒连接实时推送。客户端 ack 后才删除，属于至少一次投递。token 为 32 字节随机数，库中只存 SHA-256。

## 测试

```bash
cd server
go vet ./... && go test ./...
# 查看测试时的服务端日志：HUSH_TEST_LOG=1 go test ./... -v
```

测试用 `httptest` 起真实服务，并连接真实 WebSocket。覆盖：注册登录与单设备踢下线、预密钥上传/count/claim（OTK 消耗与 fallback）、好友申请流程与非好友消息被拒、Olm 投递与 ack、WS 积压推送与 `queue_empty`、群建立/加人/踢人/扇出、附件上传下载、朋友圈可见性（include/exclude、评论过滤）、TURN 凭证格式；接口 v2：单条动态、邮箱可见性、令牌滑动过期、outbox（提交后推送前崩溃事件不丢、回滚不留事件）、迁移回填（`moment_media`、`last_used_at`）、Olm 批量关系校验。
