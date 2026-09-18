#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_NAME="opensqt_market_maker"

cd "$ROOT_DIR"

extract_version() {
  grep -E '^var Version = "[^"]+"' main.go | sed -E 's/^var Version = "([^"]+)"$/\1/'
}

normalize_goos() {
  case "$1" in
    MacOS|macOS|macos|darwin)
      echo "darwin"
      ;;
    Windows|windows)
      echo "windows"
      ;;
    Linux|linux)
      echo "linux"
      ;;
    *)
      echo "$1"
      ;;
  esac
}

normalize_goarch() {
  case "$1" in
    x86_64)
      echo "amd64"
      ;;
    aarch64)
      echo "arm64"
      ;;
    *)
      echo "$1"
      ;;
  esac
}

SOURCE_VERSION="$(extract_version || true)"
if [[ ! "$SOURCE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "main.go 的 Version 必须为 v主版本.次版本.修订号，且各段不能包含多余的前导零" >&2
  exit 1
fi
VERSION="${VERSION:-$SOURCE_VERSION}"
if [[ "$VERSION" != "$SOURCE_VERSION" ]]; then
  echo "指定版本 $VERSION 与 main.go 版本 $SOURCE_VERSION 不一致" >&2
  exit 1
fi
TARGET_OS="${TARGET_OS:-${GOOS:-linux}}"
TARGET_ARCH="${TARGET_ARCH:-${GOARCH:-amd64}}"
GOOS="$(normalize_goos "$TARGET_OS")"
GOARCH="$(normalize_goarch "$TARGET_ARCH")"
DIST_DIR="$ROOT_DIR/dist"

package_os_name() {
  case "$GOOS" in
    darwin)
      echo "MacOS"
      ;;
    *)
      echo "$GOOS"
      ;;
  esac
}

PACKAGE_OS_NAME="${PACKAGE_OS_NAME:-$(package_os_name)}"
PACKAGE_BASENAME="${APP_NAME}_${VERSION}_${PACKAGE_OS_NAME}_${GOARCH}"
STAGE_DIR="$DIST_DIR/$PACKAGE_BASENAME"

archive_extension() {
  case "$GOOS" in
    windows)
      echo "zip"
      ;;
    *)
      echo "tar.gz"
      ;;
  esac
}

binary_name() {
  case "$GOOS" in
    windows)
      echo "${APP_NAME}.exe"
      ;;
    *)
      echo "$APP_NAME"
      ;;
  esac
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1"
    return
  fi

  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1"
    return
  fi

  echo "缺少 sha256sum 或 shasum，无法生成校验文件" >&2
  exit 1
}

create_archive() {
  case "$ARCHIVE_EXT" in
    zip)
      python3 "$ROOT_DIR/scripts/package_zip.py" "$STAGE_DIR" "$ARCHIVE_PATH"
      ;;
    tar.gz)
      tar -C "$DIST_DIR" -czf "$ARCHIVE_PATH" "$PACKAGE_BASENAME"
      ;;
    *)
      echo "不支持的压缩格式: $ARCHIVE_EXT" >&2
      exit 1
      ;;
  esac
}

ARCHIVE_EXT="$(archive_extension)"
ARCHIVE_PATH="$DIST_DIR/${PACKAGE_BASENAME}.${ARCHIVE_EXT}"
CHECKSUM_PATH="$ARCHIVE_PATH.sha256"
BIN_PATH="$STAGE_DIR/$(binary_name)"

if [[ "$ARCHIVE_EXT" == "zip" ]] && ! command -v python3 >/dev/null 2>&1; then
  echo "Windows 发行包需要 Python 3，以正确写入中文路径的 UTF-8 标记" >&2
  exit 1
fi

rm -rf "$STAGE_DIR" "$ARCHIVE_PATH" "$CHECKSUM_PATH"
mkdir -p "$STAGE_DIR"

echo "==> 构建 $APP_NAME $VERSION ($TARGET_OS/$TARGET_ARCH -> $GOOS/$GOARCH)"
CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags="-s -w" -o "$BIN_PATH" .

echo "==> 准备发布包内容"
cp README.md "$STAGE_DIR/README.md"
cp ARCHITECTURE.md "$STAGE_DIR/ARCHITECTURE.md"
cp config.example.yaml "$STAGE_DIR/config.example.yaml"
cp config.example.yaml "$STAGE_DIR/config.yaml"
cp .env.example "$STAGE_DIR/.env.example"

# 仅复制版本库中明确跟踪的演示文件，避免把本机忽略文件、临时文件或凭据带入公开发行包。
while IFS= read -r -d '' source_path; do
  target_path="$STAGE_DIR/$source_path"
  mkdir -p "$(dirname "$target_path")"
  cp "$source_path" "$target_path"
done < <(git ls-files -z -- live_server)

echo "==> 打包归档"
create_archive
(
  cd "$DIST_DIR"
  sha256_file "$(basename "$ARCHIVE_PATH")"
) > "$CHECKSUM_PATH"

echo "==> 发布包已生成"
echo "$ARCHIVE_PATH"
echo "$CHECKSUM_PATH"
