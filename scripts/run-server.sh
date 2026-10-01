#!/usr/bin/env bash
# 构建 hush-server 并以开发模式启动：数据目录 server/data。
#
# 默认只监听回环地址 127.0.0.1:8080，此时开发验证码 HUSH_DEV_CODE（默认 123456）生效：
#   - Android 模拟器经 10.0.2.2 访问宿主机回环，iOS 模拟器直接访问 127.0.0.1，均不受影响。
# 真机局域网调试时用 HUSH_ADDR 覆盖，例如：
#   HUSH_ADDR=0.0.0.0:8080 ./scripts/run-server.sh
# 管理后台默认监听 127.0.0.1:8081（HUSH_ADMIN_ADDR 覆盖，设为空串表示不启用）。后台没有默认账号，首次使用先创建管理员：
#   HUSH_ADMIN_PASSWORD='至少12位的密码' server/bin/hush-server admin create --data server/data --username <用户名>
# 监听非回环地址时服务端会忽略 HUSH_DEV_CODE（防止局域网内任何人用固定验证码登录任意账号），
# 需要配置 SMTP（HUSH_SMTP_*），或者在服务端日志里查看每次生成的验证码（--dev 下会打印）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_DIR="$ROOT/server"
ADDR="${HUSH_ADDR:-127.0.0.1:8080}"
ADMIN_ADDR="${HUSH_ADMIN_ADDR-127.0.0.1:8081}"

echo "==> 构建 hush-server"
(cd "$SERVER_DIR" && go build -o bin/hush-server ./cmd/hush-server)

mkdir -p "$SERVER_DIR/data"
export HUSH_DEV_CODE="${HUSH_DEV_CODE:-123456}"

case "$ADDR" in
  127.*|localhost:*|\[::1\]:*)
    echo "==> 启动：http://$ADDR（开发验证码 $HUSH_DEV_CODE，数据目录 $SERVER_DIR/data）" ;;
  *)
    echo "==> 启动：http://$ADDR（非回环地址：开发验证码不生效，请看日志中的验证码或配置 SMTP；数据目录 $SERVER_DIR/data）" ;;
esac
exec "$SERVER_DIR/bin/hush-server" \
  --addr "$ADDR" \
  --admin-addr "$ADMIN_ADDR" \
  --data "$SERVER_DIR/data" \
  --turn-port "${HUSH_TURN_PORT:-3478}" \
  --public-ip "${HUSH_PUBLIC_IP:-127.0.0.1}" \
  --dev "$@"
