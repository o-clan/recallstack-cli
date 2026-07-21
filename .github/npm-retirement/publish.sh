#!/usr/bin/env bash
set -euo pipefail

readonly source_version="1.1.0"
readonly retirement_version="1.1.1"
readonly source_sha256="7a537a199044311e1a26c5def61402673ee074cf3b112204d4fbc4c23faadb6b"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

cd "$work_dir"
source_tarball="$(npm pack "recallstack@$source_version" --silent)"
printf '%s  %s\n' "$source_sha256" "$source_tarball" | sha256sum --check

mkdir source
tar -xzf "$source_tarball" -C source
cp -R source/package package

find source/package/dist -type f -print0 |
  sort -z |
  xargs -0 sha256sum > source-dist.sha256

RETIREMENT_VERSION="$retirement_version" node <<'NODE'
const fs = require("node:fs");
const packagePath = "package/package.json";
const manifest = JSON.parse(fs.readFileSync(packagePath, "utf8"));
manifest.version = process.env.RETIREMENT_VERSION;
manifest.description =
  "Retired npm package; install the native Recallstack CLI instead";
fs.writeFileSync(packagePath, `${JSON.stringify(manifest, null, 2)}\n`);
NODE

cp "$GITHUB_WORKSPACE/.github/npm-retirement/README.md" package/README.md

find package/dist -type f -print0 |
  sort -z |
  xargs -0 sha256sum |
  sed 's#package/dist/#source/package/dist/#' > retirement-dist.sha256
diff -u source-dist.sha256 retirement-dist.sha256

npm pack ./package --dry-run --json > dry-run.json
RETIREMENT_VERSION="$retirement_version" node <<'NODE'
const fs = require("node:fs");
const parsed = JSON.parse(fs.readFileSync("dry-run.json", "utf8"));
const result = Array.isArray(parsed)
  ? parsed[0]
  : parsed.name
    ? parsed
    : Object.values(parsed)[0];
if (result.name !== "recallstack") throw new Error("Unexpected package name");
if (result.version !== process.env.RETIREMENT_VERSION) {
  throw new Error("Unexpected package version");
}
if (result.entryCount !== 51) throw new Error("Unexpected package contents");
const paths = new Set(result.files.map(({ path }) => path));
for (const required of ["README.md", "LICENSE", "package.json", "dist/index.js"]) {
  if (!paths.has(required)) throw new Error(`Missing ${required}`);
}
NODE

retirement_tarball="$(npm pack ./package --silent)"
mkdir smoke
npm install --prefix smoke --ignore-scripts "./$retirement_tarball"
test "$(smoke/node_modules/.bin/recallstack --version)" = "$retirement_version"
smoke/node_modules/.bin/recallstack --help >/dev/null

if [[ "${DRY_RUN:-0}" == "1" ]]; then
  exit 0
fi

npm publish ./package --access public --tag retirement --provenance
