#!/bin/bash
# 为 HushUITests/A14AuditFixTests 准备独立的测试环境：专用服务端 + 两个机器人。
# 不影响 8080 上的开发服务端。用法：
#   ios/scripts/run-ui-host.sh [端口=8092] [TURN 端口=3480] [数据目录=/tmp/hush-uitest]
# 然后运行 UI 测试时传入：
#   TEST_RUNNER_HUSH_SERVER=http://127.0.0.1:8092 TEST_RUNNER_HUSH_EMAIL=erin@hush.local TEST_RUNNER_HUSH_NAME=Erin \
#   TEST_RUNNER_HUSH_BOT_EMAIL=ibot@hush.local TEST_RUNNER_HUSH_BOT_NAME=测试助手 \
#   TEST_RUNNER_HUSH_BOT2_EMAIL=ibot2@hush.local TEST_RUNNER_HUSH_BOT2_NAME=小美 TEST_RUNNER_HUSH_SHOT_PREFIX=audit-fix-
# 先跑 A01LoginTests、A02FriendChatTests/test01AddFriends，再跑 A14AuditFixTests。
set -euo pipefail
PORT="${1:-8092}"; TURN="${2:-3480}"; DATA="${3:-/tmp/hush-uitest}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TARGET="${CARGO_TARGET_DIR:-/tmp/hushcore-uitest-bot}"
mkdir -p "$DATA"

(cd "$ROOT/server" && go build -o "$DATA/hush-server" ./cmd/hush-server)
# auditbot = core/examples/bot.rs + 「延迟来电 N」「响铃取消 N M」「连发 N」「链接」指令；源码放在 ios/scripts 下，
# 构建时临时复制到 core/examples（构建完即删除，不改动核心库的文件）
cp "$ROOT/ios/scripts/auditbot.rs" "$ROOT/core/examples/auditbot.rs"
trap 'rm -f "$ROOT/core/examples/auditbot.rs"' EXIT
(cd "$ROOT/core" && CARGO_TARGET_DIR="$TARGET" cargo build --example bot --example auditbot)
rm -f "$ROOT/core/examples/auditbot.rs"

HUSH_DEV_CODE=123456 "$DATA/hush-server" --addr "127.0.0.1:$PORT" --admin-addr "" --data "$DATA/server" --turn-port "$TURN" --dev > "$DATA/server.log" 2>&1 &
echo "server pid $!"
sleep 2
"$TARGET/debug/examples/auditbot" --server "http://127.0.0.1:$PORT" --email ibot@hush.local --name 测试助手 --data "$DATA/ibot" > "$DATA/ibot.log" 2>&1 &
echo "auditbot pid $!"
"$TARGET/debug/examples/bot" --server "http://127.0.0.1:$PORT" --email ibot2@hush.local --name 小美 --data "$DATA/ibot2" > "$DATA/ibot2.log" 2>&1 &
echo "bot pid $!"
echo "日志在 $DATA/*.log；结束时 kill 上面三个进程"
wait
