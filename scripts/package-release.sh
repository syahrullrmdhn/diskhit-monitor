#!/usr/bin/env bash
set -euo pipefail

# Build a deterministic Windows release package.
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GO_BIN=${GO_BIN:-/usr/local/go/bin/go}
OUTPUT_DIR=${1:-"$ROOT_DIR/dist"}
VERSION=${VERSION:-dev}
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-0}

if [[ ! -x "$GO_BIN" ]]; then
  echo "Go compiler not found or not executable: $GO_BIN" >&2
  exit 1
fi
for required in config.yaml README.md update-diskhist.bat; do
  [[ -f "$ROOT_DIR/$required" ]] || { echo "Missing release file: $required" >&2; exit 1; }
done

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/diskhist-release.XXXXXX")
cleanup() { rm -rf "$work_dir"; }
trap cleanup EXIT

package_dir="$work_dir/diskhist-windows-amd64"
mkdir -p "$package_dir"

printf 'Building Windows amd64 binary...\n'
(
  cd "$ROOT_DIR"
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build \
    -trimpath -buildvcs=false -ldflags='-buildid=' \
    -o "$package_dir/diskhist.exe" ./cmd/diskhist
)
cp "$ROOT_DIR/config.yaml" "$package_dir/config.yaml"
cp "$ROOT_DIR/README.md" "$package_dir/README.md"
cp "$ROOT_DIR/update-diskhist.bat" "$package_dir/update-diskhist.bat"

# Normalize metadata so identical source/build inputs produce identical archives.
find "$package_dir" -type f -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +
(
  cd "$package_dir"
  sha256sum diskhist.exe > SHA256SUMS.txt
)
touch -d "@$SOURCE_DATE_EPOCH" "$package_dir/SHA256SUMS.txt"

archive="$OUTPUT_DIR/diskhist-windows-amd64-${VERSION}.zip"
(
  cd "$work_dir"
  # -X removes host-specific extra attributes; sorted input fixes entry order.
  LC_ALL=C zip -X -q -9 "$archive" \
    -j \
    "diskhist-windows-amd64/diskhist.exe" \
    "diskhist-windows-amd64/config.yaml" \
    "diskhist-windows-amd64/README.md" \
    "diskhist-windows-amd64/update-diskhist.bat" \
    "diskhist-windows-amd64/SHA256SUMS.txt"
)

# Also provide the release checksum beside the archive for transport verification.
(cd "$OUTPUT_DIR" && sha256sum "$(basename "$archive")" > SHA256SUMS-ARCHIVE.txt)
printf 'Created %s\n' "$archive"
