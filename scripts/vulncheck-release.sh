#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
cli_root="$(cd "$script_dir/.." && pwd)"
out_dir="${RECALLSTACK_CLI_DIST:-$cli_root/dist}"
scanner_dir="$(mktemp -d)"
trap 'rm -rf "$scanner_dir"' EXIT

GOBIN="$scanner_dir" go install golang.org/x/vuln/cmd/govulncheck@v1.6.0

found=false
for artifact in "$out_dir"/recallstack_*; do
  test -f "$artifact" || continue
  found=true
  "$scanner_dir/govulncheck" -mode=binary "$artifact"
done

test "$found" = true || {
  echo "Recallstack CLI vulnerability scan found no release binaries in $out_dir." >&2
  exit 1
}

echo "Recallstack CLI release binaries passed vulnerability scanning."
