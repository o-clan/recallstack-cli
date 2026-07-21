#!/usr/bin/env bash
set -euo pipefail

VERSION="${RECALLSTACK_VERSION:-1.2.2}"
INSTALL_DIR="${RECALLSTACK_INSTALL_DIR:-$HOME/.local/bin}"
SOURCE_DIR="${RECALLSTACK_INSTALL_SOURCE_DIR:-}"
BASE_URL="${RECALLSTACK_RELEASE_BASE_URL:-https://github.com/o-clan/recallstack-cli/releases/download/v${VERSION}}"

usage() {
  echo "Install Recallstack CLI $VERSION"
  echo
  echo "Environment:"
  echo "  RECALLSTACK_VERSION             Version to install"
  echo "  RECALLSTACK_INSTALL_DIR         Destination directory (default: ~/.local/bin)"
  echo "  RECALLSTACK_INSTALL_SOURCE_DIR  Local release directory for offline installs"
  echo "  RECALLSTACK_RELEASE_BASE_URL    Release asset base URL"
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
esac

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "recallstack installer: $1 is required" >&2; exit 1; }
}

need uname
need mkdir
need chmod

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$os" in
  linux|darwin) ;;
  *) echo "recallstack installer: unsupported OS $os; use install.ps1 on Windows" >&2; exit 1 ;;
esac
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "recallstack installer: unsupported architecture $arch" >&2; exit 1 ;;
esac

artifact="recallstack_${VERSION}_${os}_${arch}"
temporary_dir=""
temporary_destination=""

cleanup() {
  if [ -n "$temporary_dir" ]; then
    rm -rf "$temporary_dir"
  fi
  if [ -n "$temporary_destination" ]; then
    rm -f "$temporary_destination"
  fi
}
trap cleanup EXIT

if [ -n "$SOURCE_DIR" ]; then
  artifact_path="$SOURCE_DIR/$artifact"
  checksum_path="$SOURCE_DIR/SHA256SUMS"
else
  temporary_dir="$(mktemp -d)"
  artifact_path="$temporary_dir/$artifact"
  checksum_path="$temporary_dir/SHA256SUMS"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$BASE_URL/$artifact" -o "$artifact_path"
    curl -fsSL "$BASE_URL/SHA256SUMS" -o "$checksum_path"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$artifact_path" "$BASE_URL/$artifact"
    wget -qO "$checksum_path" "$BASE_URL/SHA256SUMS"
  else
    echo "recallstack installer: curl or wget is required" >&2
    exit 1
  fi
fi

test -s "$artifact_path" || { echo "recallstack installer: missing $artifact" >&2; exit 1; }
test -s "$checksum_path" || { echo "recallstack installer: missing SHA256SUMS" >&2; exit 1; }

checksum_line="$(awk -v name="$artifact" '$NF == name || $NF == "*" name { print; exit }' "$checksum_path")"
test -n "$checksum_line" || { echo "recallstack installer: checksum missing for $artifact" >&2; exit 1; }
expected="$(printf '%s\n' "$checksum_line" | awk '{ print $1 }')"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$artifact_path" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$artifact_path" | awk '{ print $1 }')"
else
  echo "recallstack installer: sha256sum or shasum is required" >&2
  exit 1
fi
test "$actual" = "$expected" || { echo "recallstack installer: checksum verification failed" >&2; exit 1; }

mkdir -p "$INSTALL_DIR"
destination="$INSTALL_DIR/recallstack"
temporary_destination="$INSTALL_DIR/.recallstack-install.$$"
cp "$artifact_path" "$temporary_destination"
chmod 0755 "$temporary_destination"
installed_version="$("$temporary_destination" --version)"
case "$installed_version" in
  *"$VERSION"*) ;;
  *) echo "recallstack installer: installed version check failed: $installed_version" >&2; exit 1 ;;
esac
mv -f "$temporary_destination" "$destination"
temporary_destination=""

echo "Recallstack CLI $VERSION installed to $destination"
echo "Version: $installed_version"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) echo "Run: recallstack --help" ;;
  *) echo "Add $INSTALL_DIR to PATH, then run: recallstack --help" ;;
esac
