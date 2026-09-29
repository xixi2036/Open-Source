#!/usr/bin/env bash
# 公共环境：rustup 工具链（Homebrew 的 rust 只有宿主 target，交叉编译必须走 rustup）
export RUSTUP_DIST_SERVER="${RUSTUP_DIST_SERVER:-https://mirrors.ustc.edu.cn/rust-static}"
export PATH="/opt/homebrew/opt/rustup/bin:$HOME/.cargo/bin:$PATH"
export ANDROID_HOME="${ANDROID_HOME:-$HOME/Library/Android/sdk}"
export ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-$ANDROID_HOME/ndk/30.0.16248370}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CORE_DIR="${CORE_DIR:-$ROOT_DIR/core}"
cargo_rs() { RUSTC="$(rustup which --toolchain stable rustc)" rustup run stable cargo "$@"; }
