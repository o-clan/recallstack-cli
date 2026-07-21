#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
cli_root="$(cd "$script_dir/.." && pwd)"
version="${RECALLSTACK_VERSION:-$(tr -d '[:space:]' < "$cli_root/VERSION")}"
out_dir="${RECALLSTACK_CLI_DIST:-$cli_root/dist}"
expected=(
  "recallstack_${version}_linux_amd64"
  "recallstack_${version}_linux_arm64"
  "recallstack_${version}_darwin_amd64"
  "recallstack_${version}_darwin_arm64"
  "recallstack_${version}_windows_amd64.exe"
  "recallstack_${version}_windows_arm64.exe"
)

bash "$script_dir/package-cli.sh"

for artifact in "${expected[@]}"; do
  test -s "$out_dir/$artifact"
  file "$out_dir/$artifact"
done
test -s "$out_dir/SHA256SUMS"
test -x "$out_dir/install.sh"
test -s "$out_dir/install.ps1"

(
  cd "$out_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c SHA256SUMS
  else
    shasum -a 256 -c SHA256SUMS
  fi
)

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
host_binary="$out_dir/recallstack_${version}_${host_os}_${host_arch}"
if [ "$host_os" = "windows" ]; then
  host_binary="${host_binary}.exe"
fi

host_version="$("$host_binary" --version)"
case "$host_version" in
  *"$version"*) ;;
  *) echo "Unexpected host binary version: $host_version" >&2; exit 1 ;;
esac
"$host_binary" --help >/dev/null

if [ "$host_os" = "linux" ] || [ "$host_os" = "darwin" ]; then
  install_dir="$(mktemp -d)"
  trap 'rm -rf "$install_dir"' EXIT
  RECALLSTACK_INSTALL_SOURCE_DIR="$out_dir" \
    RECALLSTACK_INSTALL_DIR="$install_dir" \
    "$out_dir/install.sh" >/dev/null
  installed_version="$("$install_dir/recallstack" --version)"
  case "$installed_version" in
    *"$version"*) ;;
    *) echo "Unexpected installed version: $installed_version" >&2; exit 1 ;;
  esac
fi

echo "Recallstack CLI release smoke passed for $version."
