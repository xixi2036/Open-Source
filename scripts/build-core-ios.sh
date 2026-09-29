#!/usr/bin/env bash
# 编译 hushcore 为 iOS xcframework（真机 arm64 + 模拟器 arm64）并生成 Swift 绑定
set -euo pipefail
source "$(dirname "$0")/env.sh"
PROFILE="${PROFILE:-release}"
OUT="$ROOT_DIR/ios/Frameworks"
GEN="$ROOT_DIR/ios/Hush/Core/Generated"
cd "$CORE_DIR"
TARGETS=(aarch64-apple-ios aarch64-apple-ios-sim)
for t in "${TARGETS[@]}"; do
  IPHONEOS_DEPLOYMENT_TARGET=17.0 cargo_rs build --lib --profile "$PROFILE" --target "$t"
done
cargo_rs build --lib
TMP="$(mktemp -d)"
cargo_rs run --bin uniffi-bindgen -- generate --library target/debug/libhushcore.dylib \
  --language swift --out-dir "$TMP" --no-format
mkdir -p "$GEN" "$TMP/headers"
cp "$TMP"/hushcore.swift "$GEN/"
cp "$TMP"/hushcoreFFI.h "$TMP/headers/"
# 模块映射名必须为 module.modulemap，模块名 hushcoreFFI
sed 's/^module /module /' "$TMP"/hushcoreFFI.modulemap > "$TMP/headers/module.modulemap"
DIR="$PROFILE"; [ "$PROFILE" = "dev" ] && DIR=debug
rm -rf "$OUT/HushCore.xcframework"; mkdir -p "$OUT"
xcodebuild -create-xcframework \
  -library "target/aarch64-apple-ios/$DIR/libhushcore.a" -headers "$TMP/headers" \
  -library "target/aarch64-apple-ios-sim/$DIR/libhushcore.a" -headers "$TMP/headers" \
  -output "$OUT/HushCore.xcframework" >/dev/null
rm -rf "$TMP"
echo "✅ iOS HushCore.xcframework 与 Swift 绑定已更新"
