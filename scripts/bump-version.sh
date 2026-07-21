#!/usr/bin/env bash
set -euo pipefail

bump_type="${1:-}"
case "$bump_type" in
  major|minor|patch) ;;
  *) echo "usage: $0 <major|minor|patch>" >&2; exit 1 ;;
esac

script_dir="$(cd "$(dirname "$0")" && pwd)"
cli_root="$(cd "$script_dir/.." && pwd)"
version_path="$cli_root/VERSION"
current="$(tr -d '[:space:]' < "$version_path")"

case "$current" in
  ''|*[!0-9.]*|.*|*.|*..*)
    echo "VERSION must contain a semantic version, received '$current'." >&2
    exit 1
    ;;
esac
IFS=. read -r major minor patch extra <<EOF
$current
EOF
if [ -n "${extra:-}" ] || [ -z "${major:-}" ] || [ -z "${minor:-}" ] || [ -z "${patch:-}" ]; then
  echo "VERSION must contain a semantic version, received '$current'." >&2
  exit 1
fi

case "$bump_type" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
next="$major.$minor.$patch"

printf '%s\n' "$next" > "$version_path"

shell_tmp="$(mktemp)"
awk -v version="$next" '
  /^VERSION="\$\{RECALLSTACK_VERSION:-[0-9]+\.[0-9]+\.[0-9]+\}"$/ {
    print "VERSION=\"${RECALLSTACK_VERSION:-" version "}\""
    next
  }
  { print }
' "$script_dir/install.sh" > "$shell_tmp"
mv "$shell_tmp" "$script_dir/install.sh"
chmod 0755 "$script_dir/install.sh"

powershell_tmp="$(mktemp)"
awk -v version="$next" '
  /^\$DefaultVersion = "[0-9]+\.[0-9]+\.[0-9]+"$/ {
    print "$DefaultVersion = \"" version "\""
    next
  }
  { print }
' "$script_dir/install.ps1" > "$powershell_tmp"
mv "$powershell_tmp" "$script_dir/install.ps1"

bash "$script_dir/check-version.sh"
echo "Bumped Recallstack CLI from $current to $next ($bump_type)."
