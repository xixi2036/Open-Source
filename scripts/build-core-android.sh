#!/usr/bin/env bash
# 编译 hushcore 为 Android .so（arm64-v8a 真机 + x86_64 模拟器）并生成 Kotlin 绑定
set -euo pipefail
source "$(dirname "$0")/env.sh"
PROFILE="${PROFILE:-release}"
APP="$ROOT_DIR/android/app"
cd "$CORE_DIR"
cargo_rs ndk -t arm64-v8a -t x86_64 -P 26 -o "$APP/src/main/jniLibs" build --lib --profile "$PROFILE"
# 绑定生成读取宿主库的元数据
cargo_rs build --lib
cargo_rs run --bin uniffi-bindgen -- generate --library target/debug/libhushcore.dylib \
  --language kotlin --out-dir "$APP/src/main/java" --no-format
# UniFFI 生成的错误类型字段 `message` 与 Throwable.message 冲突（Kotlin 2.4 报错），改为直接 override
python3 - "$APP/src/main/java/uniffi/hushcore/hushcore.kt" <<'PY'
import re, sys
p = sys.argv[1]; s = open(p).read()
s = re.sub(r"\n\s*override val message\n\s*get\(\) = \"[^\n]*`message`[^\n]*\"", "", s)
s = s.replace("val `message`: kotlin.String", "override val `message`: kotlin.String").replace("override override", "override")
open(p, "w").write(s)
PY
echo "✅ Android 核心库与 Kotlin 绑定已更新"
