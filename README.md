# Recallstack CLI

Recallstack is a standalone native CLI for authentication, workspace and project targeting, memory retrieval and ingestion, and agent memory integrations.

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
recallstack --help
recallstack login
recallstack workspace use
recallstack project use
recallstack memory query --query "What changed?"
recallstack agent install codex
```

The CLI stores its existing configuration, authentication profiles, pending writes, and project state in the same locations and formats used by earlier releases.

Authentication tokens and API keys use the platform secure store by default: Keychain on macOS, Secret Service on Linux, and Credential Manager on Windows. If the secure store is unavailable while saving credentials, Recallstack falls back to its permission-restricted local configuration file. Set `RECALLSTACK_TOKEN_STORE=file` to explicitly use file storage.

Agent integrations now invoke the native CLI directly. Re-run `recallstack agent install <target>` after upgrading an existing Node-based installation so old hook commands are replaced.

## Release assets

Each native release includes:

- Linux amd64 and arm64 binaries
- macOS amd64 and arm64 binaries
- Windows amd64 and arm64 binaries
- `SHA256SUMS`
- POSIX and PowerShell installers

The public repository is the auditable source mirror for the native CLI.
