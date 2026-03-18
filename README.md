# Recallstack CLI

Recallstack CLI is the installable command-line interface for Recallstack.

It provides:

- authentication against the Recallstack API
- project and workspace targeting
- memory ingest and retrieval commands
- agent hook installers for Codex, Claude Code, Cursor, and GitHub Copilot

## Install

```bash
npm install -g recallstack
```

Or run it without a global install:

```bash
npx recallstack@latest --help
```

## Requirements

- Node.js 20 or later

## Common Commands

```bash
recallstack --help
recallstack login
recallstack project list
recallstack memory query --query "What changed?"
recallstack agent install codex
```

## Auth Storage

The CLI stores its auth profile on the local machine so it can reuse your session across commands. Review the source before use if you want to audit or modify that behavior.

## Public Source

This repository is the public source mirror for the Recallstack CLI package published to npm. The packaged CLI is intended to be auditable and forkable.
