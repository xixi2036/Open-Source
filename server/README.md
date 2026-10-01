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
| `--admin-addr` | `127.0.0.1:8081` | 管理后台监听地址，空字符串表示不启用。默认只监听回环；端口被占用时只告警，主服务照常运行 |
| `--dev` | 关 | 开发模式：验证码打印到日志；允许 `HUSH_DEV_CODE`（仅回环监听时）；TURN 允许中继到回环/私有地址（生产模式禁止） |
| `--tls-cert` / `--tls-key` | 空 | PEM 证书链与私钥；两者都指定后 `--addr` 直接提供 HTTPS / WSS |
| `--trusted-proxy` | 空 | 可信反向代理的地址段（逗号分隔的 CIDR 或 IP，如 `127.0.0.1,10.0.0.0/8`）。只有配置后才采信它们写的 `X-Forwarded-For`，否则所有限速都按代理的 IP 计 |
| `--log-level` | `info` | `debug` 才记录逐请求、逐条投递与社交关系明细（谁拉黑谁、谁给哪个群发消息）；默认不记录这些元数据。日志异步缓冲写出 |
| `--db-readers` | `0`（自动） | 只读数据库连接数；自动为 CPU 数，介于 4 与 16 之间。写入始终只有一条连接 |
| `--max-conns` | `20000` | WebSocket 连接数上限，超出返回 503 |
| `--max-conns-per-ip` | `-1`（自动） | 每个客户端 IP 的 WebSocket 连接数上限；自动为 200，只监听回环且没有可信代理时不限 |
| `--max-inflight` | `1024` | 同时处理的普通请求数上限（不含 WebSocket 与附件），超出返回 503 |
| `--max-uploads` | `32` | 同时进行的附件上传数上限，超出返回 503 |
| `--shutdown-timeout` | `15s` | 优雅停机的最长等待时间 |
| `--sync-full` | 关 | SQLite `synchronous=FULL`：每次提交都落盘，掉电也不丢已提交的数据（写入变慢）；默认 `NORMAL`（进程崩溃不丢，掉电可能丢最近的提交） |
| `--turn-ttl` | `6h` | TURN 凭证有效期；凭证同时与登录态绑定，退出登录或被停用后立即失效 |

## 部署要点

- **传输安全**：公网部署必须使用 HTTPS（客户端核心库对公网地址拒绝 `http://`）。两种方式任选：
  - 服务端直接提供 TLS：`--tls-cert fullchain.pem --tls-key privkey.pem --addr :443`；
  - 放在反向代理（nginx、Caddy 等）后面终结 TLS：代理需透传 WebSocket 升级头与 `Host`，并配置 `--trusted-proxy <代理地址>`，否则所有用户共用代理的 IP 计算限速。
- **管理后台**不要直接暴露到公网；需要远程访问时放在 TLS 反向代理之后，并设置 `HUSH_ADMIN_SECURE_COOKIE=1`。
- **数据库迁移**：启动时自动升级，升级前把完整副本保存到 `data/premigrate/`；数据库版本高于本程序认识的版本时拒绝启动（防止回滚版本后静默运行）。
- **优雅停机**：收到 SIGTERM / SIGINT 后停止接受新连接，等待在途请求结束，向全部 WebSocket 发送关闭帧（1001），排空写队列后再关闭数据库；超过 `--shutdown-timeout` 强制退出。客户端会自动重连。
- **健康检查**：`/healthz` 只表示进程存活；`/readyz` 会访问数据库，可用于就绪探针。
- **剖析**：`/debug/pprof/` 只挂在管理后台地址上，需要 owner 会话或 `HUSH_METRICS_TOKEN`。

## 环境变量

| 变量 | 说明 |
|---|---|
| `HUSH_DEV_CODE` | 固定验证码，对任意邮箱永远有效。**仅在 `--dev` 且监听回环地址时生效**，否则忽略并告警 |
| `HUSH_SMTP_HOST` / `HUSH_SMTP_PORT` | 配置后通过 SMTP 真实发送验证码邮件；端口默认 587（STARTTLS），465 为隐式 TLS |
| `HUSH_SMTP_USER` / `HUSH_SMTP_PASS` | SMTP 认证（PLAIN） |
| `HUSH_SMTP_FROM` | 发件人地址，缺省使用 `HUSH_SMTP_USER` |
| `HUSH_ADMIN_PASSWORD` | `hush-server admin create` / `admin passwd` 读取的密码（未设置时从标准输入读一行） |
| `HUSH_ADMIN_SECURE_COOKIE` | 设为 `1` 时后台会话 Cookie 强制带 `Secure`（TLS 由反向代理终结时使用；服务端自己提供 TLS 时自动带） |
| `HUSH_METRICS_TOKEN` | 设置后 `/metrics` 也接受 `Authorization: Bearer <token>`（供 Prometheus 抓取）；未设置时只能用后台会话访问 |
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
| 举报与公告 | `POST /v1/reports` `GET /v1/announcements`（见 API_NOTES「管理后台相关」） |
| 其他 | `GET /healthz` |

## 管理后台

与服务端同一个二进制，独立监听 `--admin-addr`（默认 `127.0.0.1:8081`）。页面是 `go:embed` 内嵌的原生 HTML/CSS/JS（没有构建步骤，不加载任何外部资源），
接口在 `/admin/api/*`，Prometheus 指标在 `/metrics`——这些路径**只存在于管理监听地址上**，对客户端的 `--addr` 上没有。
截图见 [`docs/screenshots/admin/`](../docs/screenshots/admin/)。

**隐私边界**：聊天内容端到端加密，后台看不到、也没有任何查看聊天内容的入口。后台只展示服务端本来就持有的数据：账号资料、
好友 / 群成员关系、朋友圈（明文）、队列与附件的**数量和大小**、连接状态。凡涉及消息的地方只有条数与字节数，没有任何接口返回队列里的信封；
图片预览接口只能读取头像与朋友圈图片，读不到聊天附件。

### 创建管理员与登录

后台**没有默认账号、默认密码**。首个管理员用子命令创建（密码至少 12 位，从环境变量或标准输入读取）：

```bash
cd server
HUSH_ADMIN_PASSWORD='一个至少 12 位的密码' go run ./cmd/hush-server admin create --data ./data --username root
# 或者交互输入（不设环境变量时从标准输入读一行）
./bin/hush-server admin create --data ./data --username alice --role moderator

./bin/hush-server admin list   --data ./data                     # 列出管理员
./bin/hush-server admin passwd --data ./data --username root     # 重置密码并解除锁定（忘记密码 / 被锁定时用）
```

然后浏览器打开 <http://127.0.0.1:8081/> 登录。服务器在远端时用 SSH 隧道访问：`ssh -L 8081:127.0.0.1:8081 <服务器>`。
需要直接对外提供时，请放在 TLS 反向代理之后（`--admin-addr` 仍监听回环），并设置 `HUSH_ADMIN_SECURE_COOKIE=1`；**不要把后台端口明文暴露到公网**。
反向代理必须原样转发 `Host` 头（nginx：`proxy_set_header Host $host;`），否则写操作会因 `Origin` 与 `Host` 不一致被拒绝（403 `csrf`）。

### 角色

| 角色 | 权限 |
|---|---|
| `owner`（所有者） | 全部权限：另含管理员管理、系统设置、手动清理、备份与下载、审计日志 |
| `moderator`（审核员） | 用户与内容管理：停用 / 启用 / 强制下线 / 重置资料 / 删除账号，群组（解散、移除成员、转让群主），朋友圈与评论删除，举报处理，邀请码，系统公告 |
| `viewer`（只读） | 只能查看（概览、监控、用户、群组、朋友圈、举报、公告、设置、存储）；邀请码只显示前 4 位 |

每个接口都在服务端校验会话、角色与 CSRF（`internal/admin/admin.go` 的路由表声明了每条接口的最低角色，测试逐条验证）。
最后一个可用的 owner 不能被停用、降级或删除。

### 安全机制

- **会话**：登录后下发随机会话令牌，库里只存 SHA-256 哈希；Cookie 为 `HttpOnly; SameSite=Strict`（https 下加 `Secure`），12 小时无操作过期。
  修改角色、停用、重置密码都会让该管理员的会话立即失效。
- **CSRF**：所有写操作必须带请求头 `X-CSRF-Token`（登录响应与 `GET /admin/api/me` 下发，由会话令牌派生）；同时校验 `Origin` 与 `Host` 一致。
- **登录限速**：同一 IP 每分钟 5 次；同一用户名连续 10 次失败锁定 15 分钟（期间密码正确也拒绝）。密码用 bcrypt（cost 12）存储。
  限速按对端 IP 计（不信任 `X-Forwarded-For`）：放在反向代理之后时，所有请求都来自代理的 IP，共用这一个额度。
- **审计日志**：每次写操作（含登录、退出、备份下载）都记一条：时间、管理员、动作、对象、参数摘要、来源 IP。只能追加——
  后台没有删除接口，数据库里的触发器也拒绝对 `admin_audit` 的 `UPDATE` / `DELETE`。密码、邀请码不会写进审计。
- **响应头**：`Content-Security-Policy` 限制为同源（没有 `unsafe-inline`）、`X-Frame-Options: DENY`、`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、`Cache-Control: no-store`。

### 功能

| 页面 | 内容 |
|---|---|
| 概览 | 注册用户、今日新增、在线连接、今日活跃、群组、动态、离线队列条数与字节数、附件数量与占用、运行时长与版本；最近 30 天注册 / 活跃趋势；最近 24 小时每小时入队信封数 |
| 实时监控 | 每 5 秒刷新：在线连接、每秒请求数、各接口 P50/P95/P99、数据库写队列等待、队列积压、协程数与内存、最近的 5xx 与 panic |
| 用户 | 搜索（邮箱 / 昵称 / ID）、分页、排序；详情；停用 / 启用、强制下线、重置资料、删除账号（需输入邮箱确认） |
| 群组 | 列表与搜索、成员与角色、消息**条数**；解散群、移除成员、转让群主 |
| 朋友圈审核 | 动态列表（被举报的优先，可按作者筛选）、图片缩略图、评论；删除动态 / 评论 |
| 举报 | 待处理 / 已处理队列；处理动作：忽略、删除内容、停用账号，可写备注 |
| 注册控制 | 邀请码生成（数量、次数上限、过期时间）、使用记录、作废；注册模式与邮箱域名白名单在「系统设置」里改 |
| 系统公告 | 发布（标题、正文、可选截止时间）并推送给全部用户；撤下 |
| 系统设置 | owner 专属，写入 `settings` 表，**保存后立即生效、无需重启**：限速参数、队列配额、上传配额、令牌有效期、注册模式、邮箱域名白名单、备份保留份数。启动参数是默认值，后台的修改覆盖它，可逐项「恢复默认」 |
| 存储与维护 | 附件占用排行、手动触发清理、在线备份 |
| 管理员 / 审计日志 | owner 专属：管理员增删改、重置密码、停用；审计日志按管理员、动作、时间筛选 |

**停用账号**：令牌全部作废、`POST /v1/auth/verify` 返回 403 `account_disabled`、在线连接收到 `logged_out{reason:"account_disabled"}` 后以 4001 断开；数据保留，可随时启用。
**删除账号**：在一个事务里级联清理好友关系、好友申请、拉黑、群成员（群主自动转让，空群解散）、朋友圈（含点赞、评论、通知）、离线队列、密钥与令牌，
好友收到 `friend_removed`，所在群的其余成员收到 `group_updated`；头像与朋友圈图片文件随后删除。该用户上传的聊天附件（密文）留给 90 天闲置清理。

### 备份

「存储与维护 → 创建备份」用 SQLite `VACUUM INTO` 在线备份到 `data/backups/hush-YYYYMMDD-HHMMSS.db`（权限 0600），只保留最近 N 份（默认 7，可在系统设置里改）。
备份期间其他读写会排队等待（库很大时会有短暂停顿）。owner 可以在页面上下载备份，每次下载都记入审计。
备份包含账号资料、关系、朋友圈、队列里的服务端事件、管理员的密码哈希等，请按最高敏感级别保管。
**备份不含任何消息**：生成备份时，离线队列里尚未投递的端到端加密信封会从副本中删除并重新整理文件（不留在空闲页里）——后台没有任何拿到消息内容的途径，密文也不例外。
代价是用这种备份恢复后，当时尚未投递的消息不会再投递（发送方已拿到成功响应，接收方永远收不到）。需要连未投递消息一起的完整副本时，在服务器上直接执行
`sqlite3 data/hush.db ".backup '/path/to/full.db'"`（需要主机的文件权限，不经过后台）。附件文件（`data/attachments/`）与 `data/turn_secret` 不在备份里，需要另行复制。
恢复：停掉服务，用备份文件替换 `data/hush.db`（并删除旧的 `hush.db-wal` / `hush.db-shm`），再启动。

### 监控

`GET /metrics`（只在管理监听地址上）输出 Prometheus 文本格式：`hush_http_requests_total{route,code}`、`hush_http_request_duration_seconds` 直方图（按路由模式，不含 WebSocket 长连接）、
`hush_ws_connections`、`hush_users`、`hush_queue_items` / `hush_queue_bytes`、`hush_db_wait_count` / `hush_db_wait_seconds`、`hush_envelopes_enqueued_total`、`hush_panics_total`、`go_goroutines`、`go_memstats_*`。
访问需要后台会话，或者设置 `HUSH_METRICS_TOKEN` 后用 `Authorization: Bearer <token>`：

```yaml
scrape_configs:
  - job_name: hush
    static_configs: [{ targets: ["127.0.0.1:8081"] }]
    authorization: { credentials: "<HUSH_METRICS_TOKEN 的值>" }
```

## 代码结构

```
cmd/hush-server/     入口：参数、环境变量、启动 HTTP 与 TURN
internal/store/      SQLite 数据访问与迁移（users、tokens、keys、queue、friends、groups、moments…）；store.Tx 是业务事务（outbox）
internal/api/        HTTP handlers（按契约章节分文件）、鉴权、统一错误体、请求日志；可在线修改的 Settings、统计增量的定时落库
internal/admin/      管理后台：独立监听地址上的页面（static/，go:embed）与 /admin/api/* 接口、会话 / CSRF / 角色、审计、`admin` 子命令
internal/metrics/    轻量指标：按路由的计数与耗时直方图、最近的 5xx / panic，输出 Prometheus 文本格式
internal/ws/         在线连接管理与投递：按接收方持久化队列 → 在线即推送 → ack 删除
internal/turn/       内嵌 pion/turn（UDP）与 TURN REST 时限凭证
internal/mail/       验证码邮件（日志 / SMTP）
internal/push/       离线推送接口 Pusher（本地部署只有日志实现）
```

投递语义：信封与事件都按接收方写入 `queue` 表；事件与产生它的业务变更在同一个数据库事务里写入（outbox）。群消息与多人事件的正文只存一份（`envelopes` 表，队列行引用它）。WebSocket 连接建立后，先按顺序推送积压，再发 `queue_empty`；之后由写协程的提交钩子把刚提交的队列项直接交给在线连接（不再回数据库读），多帧合并写出。客户端 ack 后才删除，属于至少一次投递。token 为 32 字节随机数，库中只存 SHA-256。

数据库访问：只有一条写连接，写操作排队后按批组提交（一个事务里执行多个请求的写入，每个请求有自己的保存点，失败互不影响），保证「提交顺序 = 队列序号顺序」；只读查询走独立的只读连接池（WAL 下并发读）。鉴权结果与好友/拉黑/群成员关系在内存里缓存，相关写入提交后立即失效。

## 压测

`cmd/hush-loadtest` 模拟 N 个用户（注册、上传密钥、建立 WebSocket、小圈子互为好友），按场景发单聊与群消息并 ack，输出吞吐、端到端延迟分位、读接口延迟与错误率。结果与历史对比见 [BENCHMARK.md](BENCHMARK.md)。

```bash
cd server
go build -o /tmp/hush-server ./cmd/hush-server && go build -o /tmp/hush-loadtest ./cmd/hush-loadtest
ulimit -n 65536
HUSH_DEV_CODE=123456 /tmp/hush-server --dev --addr 127.0.0.1:8095 --turn-port 0 --admin-addr "" --data /tmp/hush-bench &
/tmp/hush-loadtest -url http://127.0.0.1:8095 -md /tmp/bench.md          # 全套场景
/tmp/hush-loadtest -url http://127.0.0.1:8095 -scenario group -duration 10s  # 只跑群消息
```

压测工具默认拒绝压 8080 端口（通常是正在使用的开发服务端），需要时加 `-allow-8080`。笔记本上压测前请防止系统睡眠（例如 `caffeinate -i`），否则睡眠期间的延迟会被计入。

## 测试

```bash
cd server
go vet ./... && go test ./...
# 查看测试时的服务端日志：HUSH_TEST_LOG=1 go test ./... -v
```

测试用 `httptest` 起真实服务，并连接真实 WebSocket。覆盖：注册登录与单设备踢下线、预密钥上传/count/claim（OTK 消耗与 fallback）、好友申请流程与非好友消息被拒、Olm 投递与 ack、WS 积压推送与 `queue_empty`、群建立/加人/踢人/扇出、附件上传下载、朋友圈可见性（include/exclude、评论过滤）、TURN 凭证格式；接口 v2：单条动态、邮箱可见性、令牌滑动过期、outbox（提交后推送前崩溃事件不丢、回滚不留事件）、迁移回填（`moment_media`、`last_used_at`）、Olm 批量关系校验；2026-09-30 审查修复（`docs/audit/2026-09-30-server.md` 的 A1–A11 均有先失败后通过的回归测试）：群消息正文单副本、按发送方的子配额与「消息被拒」事件、事件配额与合并、拉黑语义、各写入上限与限速、发送幂等、附件引用归属、滑动过期刷新间隔、清理任务的改名保护、优雅停机、迁移版本保护，以及读写分离、组提交、提交钩子直投下的投递顺序与不丢不重。
