# Recallstack CLI

Recallstack keeps your goals, decisions and taste, and briefs every coding agent with them. This is its native CLI: sign-in, connecting agents, the hooks those agents run, and the commands agents use to record and settle thoughts.

## Install

Recallstack is distributed only as native release binaries. npm and npx installs are no longer supported or published.

### macOS and Linux

```bash
curl -fsSL https://github.com/o-clan/recallstack-cli/releases/latest/download/install.sh | bash
```

The installer selects the correct binary, verifies it against `SHA256SUMS`, and installs it to `~/.local/bin` by default.

### Windows PowerShell

```powershell
irm https://github.com/o-clan/recallstack-cli/releases/latest/download/install.ps1 | iex
```

The installer verifies the binary checksum, installs it for the current user, and adds its directory to the user `PATH`.

### Migrating from npm

The [historical npm package](https://www.npmjs.com/package/recallstack) is retired and remains available only to direct existing users to the native CLI. Remove the global npm installation, then use the installer above. Existing Recallstack configuration and project state are preserved.

## Verify a manual download

Each release contains binaries for Linux, macOS, and Windows on amd64 and arm64, plus `SHA256SUMS`.

```bash
sha256sum -c SHA256SUMS
```

On macOS, use `shasum -a 256 -c SHA256SUMS`.

Release binaries are currently checksummed but are not code-signed or notarized. macOS Gatekeeper or Windows SmartScreen may show a warning for a manual download.

## Common commands

```bash
recallstack login                 # opens the browser and shows a code to match
recallstack agent install         # connects every agent on this computer
recallstack brief                 # exactly what the next session will be told
recallstack import                # turns rules you wrote for agents into thoughts
recallstack why "<topic or ref>"  # why something is the way it is
recallstack agent status
```

Agents run `recallstack note`, `hold`, `resolve`, `adjust`, `drop` and `undo` themselves; the installed skill teaches them when.

Everything the CLI keeps lives in `~/.recallstack` (set `RECALLSTACK_HOME` to move it). Nothing is written into your repos, except Cursor's brief rule (`.cursor/rules/recallstack-brief.mdc`, kept out of git through `.git/info/exclude`). Transcripts never leave the computer: only the extracted thoughts, each with the person's exact words, after secrets are stripped.

Sign-in tokens use the platform secure store by default: Keychain on macOS, Secret Service on Linux, and Credential Manager on Windows. If it's unavailable, Recallstack falls back to its permission-restricted config file. Set `RECALLSTACK_TOKEN_STORE=file` to use file storage explicitly.

## Release assets

Each native release includes:

- Linux amd64 and arm64 binaries
- macOS amd64 and arm64 binaries
- Windows amd64 and arm64 binaries
- `SHA256SUMS`
- POSIX and PowerShell installers

The public repository is the auditable source mirror for the native CLI.
