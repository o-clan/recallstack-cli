#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
cli_root="$(cd "$script_dir/.." && pwd)"
source_version="$(tr -d '[:space:]' < "$cli_root/VERSION")"
version="${RECALLSTACK_VERSION:-$source_version}"
out_dir="${RECALLSTACK_CLI_DIST:-$cli_root/dist}"
targets="${RECALLSTACK_CLI_TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"

cd "$cli_root"
bash "$script_dir/check-version.sh"
test "$version" = "$source_version" || {
  echo "recallstack release: requested version $version does not match VERSION $source_version" >&2
  exit 1
}
command -v go >/dev/null 2>&1 || { echo "recallstack release: go is required" >&2; exit 1; }

rm -rf "$out_dir"
mkdir -p "$out_dir"

artifacts=()
for target in $targets; do
  os="${target%/*}"
  arch="${target#*/}"
  extension=""
  if [ "$os" = "windows" ]; then
    extension=".exe"
  fi
  artifact="recallstack_${version}_${os}_${arch}${extension}"
  echo "Building $artifact"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -trimpath \
    -ldflags "-s -w -X main.version=$version" \
    -o "$out_dir/$artifact" \
    ./cmd/recallstack
  if [ "$os" != "windows" ]; then
    chmod 0755 "$out_dir/$artifact"
  fi
  artifacts+=("$artifact")
done

checksum_file="$out_dir/SHA256SUMS"
: > "$checksum_file"
for artifact in "${artifacts[@]}"; do
  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$out_dir" && sha256sum "$artifact") >> "$checksum_file"
  elif command -v shasum >/dev/null 2>&1; then
    (cd "$out_dir" && shasum -a 256 "$artifact") >> "$checksum_file"
  else
    echo "recallstack release: sha256sum or shasum is required" >&2
    exit 1
  fi
done

cp "$script_dir/install.sh" "$out_dir/install.sh"
cp "$script_dir/install.ps1" "$out_dir/install.ps1"
chmod 0755 "$out_dir/install.sh"

echo "Wrote Recallstack CLI $version release assets to $out_dir"
