# Recallstack CLI — npm package retired

This npm package is retired and no longer supported. Recallstack CLI is now
distributed as a standalone native binary. Do not install it through npm or
run it through npx.

## Install the native CLI

macOS and Linux:

```bash
curl -fsSL https://github.com/o-clan/recallstack-cli/releases/latest/download/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://github.com/o-clan/recallstack-cli/releases/latest/download/install.ps1 | iex
```

Then confirm the installation:

```bash
recallstack --version
```

If you installed the old npm package globally, remove it first:

```bash
npm uninstall -g recallstack
```

The native CLI preserves compatible Recallstack configuration and project
state from the npm CLI.

## Source and releases

- [Source code](https://github.com/o-clan/recallstack-cli)
- [Native releases](https://github.com/o-clan/recallstack-cli/releases/latest)
