#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
cli_root="$(cd "$script_dir/.." && pwd)"
version="$(tr -d '[:space:]' < "$cli_root/VERSION")"

case "$version" in
  ''|*[!0-9.]*|.*|*.|*..*)
    echo "VERSION must contain a semantic version, received '$version'." >&2
    exit 1
    ;;
esac

IFS=. read -r major minor patch extra <<EOF
$version
EOF
if [ -n "${extra:-}" ] || [ -z "${major:-}" ] || [ -z "${minor:-}" ] || [ -z "${patch:-}" ]; then
  echo "VERSION must contain a semantic version, received '$version'." >&2
  exit 1
fi

grep -F "RECALLSTACK_VERSION:-$version" "$script_dir/install.sh" >/dev/null || {
  echo "CLI version $version is out of sync with scripts/install.sh." >&2
  exit 1
}
grep -F "DefaultVersion = \"$version\"" "$script_dir/install.ps1" >/dev/null || {
  echo "CLI version $version is out of sync with scripts/install.ps1." >&2
  exit 1
}

echo "Recallstack CLI version $version is synchronized."
