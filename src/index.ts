#!/usr/bin/env node
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import { homedir, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { createInterface } from "node:readline/promises";
import { Command } from "commander";
import {
  DEFAULT_BASE_URL,
  getConfigPath,
  getEffectiveBaseUrl,
  loadConfig,
  resolveConfig,
  saveBaseUrlOverride,
  updateProfileForBaseUrl,
} from "./lib/config.js";
import { authenticatedHttp, loginWithCode, logout } from "./lib/auth.js";
import { formatStructuredQueryForCli } from "./lib/format-structured-query.js";
import {
  authQueueReason,
  flushPendingWrites,
  isAuthRequiredError,
  listPendingWrites,
  queuePendingWrite,
} from "./lib/pending-writes.js";
import { emitCliTelemetry } from "./lib/telemetry.js";
import { withCliSpan } from "./lib/tracing.js";
import { CLI_PACKAGE_VERSION } from "./lib/version.js";
import {
  CLAUDE_HOOK_TEMPLATE,
  CLAUDE_SKILL_TEMPLATE,
  COPILOT_HOOK_TEMPLATE,
  COPILOT_INSTRUCTIONS_TEMPLATE,
  COPILOT_PLUGIN_SKILL_TEMPLATE,
  CODEX_HOOK_TEMPLATE,
  CODEX_SKILL_TEMPLATE,
  CURSOR_HOOK_TEMPLATE,
  CURSOR_RULE_TEMPLATE,
} from "./lib/agent-templates.js";
import { closeTurnSnapshot, proofBytes, startTurnSnapshot } from "./mcp/turn-tracker.js";

type AgentTarget = "codex" | "claude" | "cursor" | "copilot" | "all";
type AgentInstallScope = "local" | "global";
type JsonRecord = Record<string, unknown>;
type WorkerModelTarget = Exclude<AgentTarget, "cursor" | "all">;
type QueryWorkerTarget = WorkerModelTarget;
type AgentSettings = Partial<Record<WorkerModelTarget, { worker_model?: string }>>;
type AgentInstallOptions = { workerModel?: string; scope?: AgentInstallScope };

type WorkspaceRecallstackConfig = {
  workspace_id?: string;
  workspace_slug?: string;
  workspace_name?: string;
  project_id?: string;
  project_slug?: string;
  project_name?: string;
  project_mode?: "GLOBAL" | "PROJECT";
  project?: string;
  workspace_root?: string;
  updated_at?: string;
};

type WorkspaceSummary = {
  id: string;
  slug: string;
  name: string;
};

type ProjectSummary = {
  id: string;
  slug: string;
  name: string;
  mode: "GLOBAL" | "PROJECT";
};

type WorkspacesResponse = {
  activeWorkspaceId?: string | null;
  items?: Array<{
    workspace?: {
      id?: string;
      slug?: string;
      name?: string;
      kind?: string;
    };
    role?: string;
    status?: string;
  }>;
};

type WorkspaceMembershipSummary = {
  id: string;
  slug: string;
  name: string;
  kind: string;
  role: string;
  status: string;
  isActive: boolean;
};

type CanonicalTargetInput = {
  workspaceSlug: string;
  projectSlug: string;
};

type ResolvedTarget = {
  workspaceId: string;
  workspaceSlug: string;
  workspaceName: string;
  projectId: string;
  projectSlug: string;
  projectName: string;
  mode: "GLOBAL" | "PROJECT";
};

const MAX_PROOF_BYTES = 65536;
const CODEX_SKILL_NAME = "recallstack-memory";
const CODEX_SKILL_FILENAME = "SKILL.md";
const CODEX_HOOK_FILENAME = "recallstack.mjs";
const CLAUDE_HOOK_FILENAME = "recallstack.mjs";
const CURSOR_HOOK_FILENAME = "recallstack.mjs";
const CURSOR_HOOKS_FILENAME = "hooks.json";
const COPILOT_HOOK_FILENAME = "recallstack.mjs";
const COPILOT_CLI_HOOKS_FILENAME = "recallstack-copilot-cli.json";
const COPILOT_VSCODE_HOOKS_FILENAME = "recallstack-vscode.json";
const QUERY_SYNTHESIS_SCHEMA = {
  type: "object",
  additionalProperties: false,
  properties: {
    answer: { type: "string" },
    primary_ids: { type: "array", items: { type: "string" } },
    context_ids: { type: "array", items: { type: "string" } },
    caveats: { type: "array", items: { type: "string" } },
  },
  required: ["answer", "primary_ids", "context_ids", "caveats"],
} as const;

function telemetryErrorCode(error: unknown): string {
  if (typeof error === "object" && error !== null) {
    const maybeCode = (error as { code?: unknown }).code;
    if (typeof maybeCode === "string" && maybeCode.trim().length > 0) {
      return maybeCode;
    }
  }
  return error instanceof Error ? (error.name || "Error") : "INTERNAL_ERROR";
}

function cliHasFlag(flag: string): boolean {
  return process.argv.includes(flag);
}

function assertExactlyOneInputSource(options: { file?: string; stdin?: boolean }, commandLabel: string): void {
  if (options.stdin && options.file) {
    throw new Error(`Provide either --file or --stdin for ${commandLabel}, not both.`);
  }
  if (!options.stdin && !options.file) {
    throw new Error(`Provide --file or --stdin for ${commandLabel}.`);
  }
}

function assertValidDiffProofMode(mode: string): asserts mode is "advisory" | "strict" {
  if (mode !== "advisory" && mode !== "strict") {
    throw new Error('Invalid --diff-proof-mode. Use "advisory" or "strict".');
  }
}

function queuedMemoryFooter(queueId: string): string {
  return `Memory updated: pending local sync (reason=AUTH_REQUIRED, queue=${queueId})`;
}

function summarizeAuthState(config: ReturnType<typeof loadConfig>): JsonRecord {
  const pendingWrites = listPendingWrites({ baseUrl: getRuntimeBaseUrl(config) });
  const authBlockedWrites = pendingWrites.filter((record) => (
    /AUTH|LOGIN|UNAUTHORIZED|401/i.test(record.queueReason)
  ));
  const authenticated = Boolean(config.accessToken || config.apiKey);
  return {
    authenticated,
    token_source: config.accessToken ? "jwt" : config.apiKey ? "api_key" : null,
    token_storage: config.tokenStorage || "file",
    status: authenticated
      ? authBlockedWrites.length > 0 ? "authenticated_with_pending_auth_replay" : "authenticated"
      : authBlockedWrites.length > 0 ? "login_required_with_pending_replay" : "login_required",
    pending_writes: pendingWrites.length,
    auth_blocked_pending_writes: authBlockedWrites.length,
    login_required: !authenticated || authBlockedWrites.length > 0,
  };
}

function queueMemoryWrite(input: {
  config: ReturnType<typeof loadConfig>;
  kind: "event" | "source";
  target: Pick<ResolvedTarget, "workspaceId" | "projectSlug">;
  body: JsonRecord;
  error: unknown;
  cwd: string;
  summary: string;
}): JsonRecord {
  const queued = queuePendingWrite({
    kind: input.kind,
    baseUrl: getRuntimeBaseUrl(input.config),
    workspaceId: input.target.workspaceId,
    projectToken: input.target.projectSlug,
    body: input.body,
    origin: "cli",
    cwd: input.cwd,
    queueReason: authQueueReason(input.error),
    lastError: {
      message: input.error instanceof Error ? input.error.message : String(input.error),
      ...(typeof input.error === "object" && input.error !== null && typeof (input.error as { code?: unknown }).code === "string"
        ? { code: (input.error as { code: string }).code }
        : {}),
    },
  });
  return {
    status: "queued_local",
    queued_local: true,
    duplicate: queued.duplicate,
    queue_id: queued.record.id,
    sync_status: "pending_login",
    project_id: input.target.projectSlug,
    message: input.summary,
    memory_footer: queuedMemoryFooter(queued.record.id),
  };
}

async function flushQueuedWritesAfterLogin(config: ReturnType<typeof loadConfig>): Promise<void> {
  const replay = await flushPendingWrites({ config });
  if (replay.replayed > 0) {
    const label = replay.replayed === 1 ? "write" : "writes";
    console.log(`Replayed ${replay.replayed} queued memory ${label}.`);
  }
  if (replay.remaining > 0) {
    const label = replay.remaining === 1 ? "write is" : "writes are";
    console.warn(`${replay.remaining} queued memory ${label} still pending.`);
  }
}

function writeExecutableScript(filePath: string, content: string): void {
  mkdirSync(dirname(filePath), { recursive: true });
  writeFileSync(filePath, content, "utf8");
  chmodSync(filePath, 0o755);
}

function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

function truncate(value: string, limit: number): string {
  const normalized = value.trim();
  if (normalized.length <= limit) return normalized;
  return `${normalized.slice(0, Math.max(0, limit - 3))}...`;
}

function readJsonFile<T>(filePath: string, fallback: T): T {
  if (!existsSync(filePath)) return fallback;
  try {
    return JSON.parse(readFileSync(filePath, "utf8")) as T;
  } catch {
    throw new Error(`Failed to parse JSON file at ${filePath}. Fix the file and retry.`);
  }
}

function readJsonFromString<T>(raw: string, fallback: T): T {
  try {
    return JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}

function writeJsonFile(filePath: string, content: unknown): void {
  mkdirSync(dirname(filePath), { recursive: true });
  writeFileSync(filePath, `${JSON.stringify(content, null, 2)}\n`, "utf8");
}

function writeInstructionFile(filePath: string, content: string): void {
  mkdirSync(dirname(filePath), { recursive: true });
  writeFileSync(filePath, content, "utf8");
}

function removePathIfExists(path: string): boolean {
  if (!existsSync(path)) return false;
  rmSync(path, { recursive: true, force: true });
  return true;
}

function pruneEmptyDirectories(paths: string[], stopAt: string): void {
  const stop = resolve(stopAt);
  for (const path of paths) {
    let cursor = resolve(path);
    while (cursor.startsWith(stop) && cursor !== stop) {
      if (!existsSync(cursor)) {
        cursor = dirname(cursor);
        continue;
      }
      try {
        if (readdirSync(cursor).length > 0) {
          break;
        }
        rmSync(cursor, { recursive: true, force: true });
      } catch {
        break;
      }
      cursor = dirname(cursor);
    }
  }
}

function resolveCodexIntegrationPaths(cwd: string, scope: AgentInstallScope = "local"): {
  skillDir: string;
  skillPath: string;
  hookDir: string;
  hookPath: string;
  hooksConfigPath: string;
  configPath: string;
} {
  const root = scope === "global" ? join(homedir(), ".codex") : join(cwd, ".codex");
  const skillDir = join(root, "skills", CODEX_SKILL_NAME);
  const hookDir = join(root, "hooks");
  return {
    skillDir,
    skillPath: join(skillDir, CODEX_SKILL_FILENAME),
    hookDir,
    hookPath: join(hookDir, CODEX_HOOK_FILENAME),
    hooksConfigPath: join(root, "hooks.json"),
    configPath: join(root, "config.toml"),
  };
}

function resolveClaudeIntegrationPaths(cwd: string, scope: AgentInstallScope = "local"): {
  root: string;
  hookPath: string;
  legacyHookPath: string;
  skillPath: string;
  settingsPath: string;
} {
  const root = scope === "global" ? join(homedir(), ".claude") : join(cwd, ".claude");
  return {
    root,
    hookPath: join(root, "hooks", CLAUDE_HOOK_FILENAME),
    legacyHookPath: join(root, "hooks", "recallstack.sh"),
    skillPath: join(root, "skills", "recallstack-memory.md"),
    settingsPath: join(root, scope === "global" ? "settings.json" : "settings.local.json"),
  };
}

function resolveCursorIntegrationPaths(cwd: string, scope: AgentInstallScope = "local"): {
  root: string;
  rulePath: string;
  hookPath: string;
  hooksConfigPath: string;
} {
  const root = scope === "global" ? join(homedir(), ".cursor") : join(cwd, ".cursor");
  return {
    root,
    rulePath: join(root, "rules", "recallstack-memory.mdc"),
    hookPath: join(root, "hooks", CURSOR_HOOK_FILENAME),
    hooksConfigPath: join(root, CURSOR_HOOKS_FILENAME),
  };
}

function getGlobalRecallstackDir(): string {
  return join(homedir(), ".recallstack");
}

function resolveCopilotLocalIntegrationPaths(cwd: string): {
  instructionsPath: string;
  hookDir: string;
  hookPath: string;
  cliHooksConfigPath: string;
  vscodeHooksConfigPath: string;
} {
  const hookDir = join(cwd, ".github", "hooks");
  return {
    instructionsPath: join(cwd, ".github", "copilot-instructions.md"),
    hookDir,
    hookPath: join(hookDir, COPILOT_HOOK_FILENAME),
    cliHooksConfigPath: join(hookDir, COPILOT_CLI_HOOKS_FILENAME),
    vscodeHooksConfigPath: join(hookDir, COPILOT_VSCODE_HOOKS_FILENAME),
  };
}

function resolveCopilotGlobalIntegrationPaths(): {
  pluginSourceRoot: string;
  pluginManifestPath: string;
  skillDir: string;
  skillPath: string;
  legacySkillPath: string;
  hookDir: string;
  hookPath: string;
  hooksConfigPath: string;
  installedPluginPath: string;
  configDir: string;
} {
  const pluginSourceRoot = join(getGlobalRecallstackDir(), "copilot-plugin");
  const configDir = join(homedir(), ".copilot");
  return {
    pluginSourceRoot,
    pluginManifestPath: join(pluginSourceRoot, "plugin.json"),
    skillDir: join(pluginSourceRoot, "skills", "recallstack-memory"),
    skillPath: join(pluginSourceRoot, "skills", "recallstack-memory", "SKILL.md"),
    legacySkillPath: join(pluginSourceRoot, "skills", "recallstack-memory.md"),
    hookDir: join(pluginSourceRoot, "hooks"),
    hookPath: join(pluginSourceRoot, "hooks", COPILOT_HOOK_FILENAME),
    hooksConfigPath: join(pluginSourceRoot, "hooks.json"),
    installedPluginPath: join(configDir, "installed-plugins", "_direct", "copilot-plugin"),
    configDir,
  };
}

function workerModelForStatus(cwd: string, scope: AgentInstallScope, target: WorkerModelTarget): {
  value: string | null;
  source: AgentInstallScope | null;
} {
  if (scope === "global") {
    const configured = readAgentSettings(cwd, "global")[target]?.worker_model;
    return {
      value: typeof configured === "string" && configured.trim().length ? configured.trim() : null,
      source: typeof configured === "string" && configured.trim().length ? "global" : null,
    };
  }
  return resolvedWorkerModel(cwd, target);
}

function readTextFile(filePath: string): string {
  if (!existsSync(filePath)) return "";
  return readFileSync(filePath, "utf8");
}

function writeTextFile(filePath: string, content: string): void {
  mkdirSync(dirname(filePath), { recursive: true });
  writeFileSync(filePath, content, "utf8");
}

function tomlSectionHeader(sectionName: string): string {
  return `[${sectionName}]`;
}

function findTomlSectionRange(lines: string[], sectionName: string): { start: number; end: number } | undefined {
  const header = tomlSectionHeader(sectionName);
  const start = lines.findIndex((line) => line.trim() === header);
  if (start === -1) return undefined;
  let end = lines.length;
  for (let index = start + 1; index < lines.length; index += 1) {
    if (/^\s*\[.*\]\s*$/.test(lines[index] as string)) {
      end = index;
      break;
    }
  }
  return { start, end };
}

function normalizeToml(lines: string[]): string {
  const next = [...lines];
  while (next.length > 0 && next[0]?.trim() === "") {
    next.shift();
  }
  while (next.length > 0 && next[next.length - 1]?.trim() === "") {
    next.pop();
  }
  return next.length ? `${next.join("\n")}\n` : "";
}

function upsertTomlKey(content: string, sectionName: string, key: string, value: string): string {
  const assignment = `${key} = ${value}`;
  const lines = content.length ? content.replace(/\r\n/g, "\n").split("\n") : [];
  const range = findTomlSectionRange(lines, sectionName);

  if (!range) {
    if (lines.length > 0 && lines[lines.length - 1]?.trim() !== "") {
      lines.push("");
    }
    lines.push(tomlSectionHeader(sectionName));
    lines.push(assignment);
    return normalizeToml(lines);
  }

  for (let index = range.start + 1; index < range.end; index += 1) {
    if (new RegExp(`^\\s*${key}\\s*=`).test(lines[index] as string)) {
      lines[index] = assignment;
      return normalizeToml(lines);
    }
  }

  lines.splice(range.end, 0, assignment);
  return normalizeToml(lines);
}

function removeTomlKey(content: string, sectionName: string, key: string): string {
  const lines = content.length ? content.replace(/\r\n/g, "\n").split("\n") : [];
  const range = findTomlSectionRange(lines, sectionName);
  if (!range) return normalizeToml(lines);

  const filtered = lines.filter((line, index) => {
    if (index <= range.start || index >= range.end) return true;
    return !new RegExp(`^\\s*${key}\\s*=`).test(line);
  });

  const nextRange = findTomlSectionRange(filtered, sectionName);
  if (nextRange) {
    const hasContent = filtered
      .slice(nextRange.start + 1, nextRange.end)
      .some((line) => line.trim().length > 0 && !line.trim().startsWith("#"));
    if (!hasContent) {
      filtered.splice(nextRange.start, nextRange.end - nextRange.start);
    }
  }

  return normalizeToml(filtered);
}

function tomlKeyEnabled(content: string, sectionName: string, key: string, expectedValue: string): boolean {
  const lines = content.length ? content.replace(/\r\n/g, "\n").split("\n") : [];
  const range = findTomlSectionRange(lines, sectionName);
  if (!range) return false;
  return lines
    .slice(range.start + 1, range.end)
    .some((line) => new RegExp(`^\\s*${key}\\s*=\\s*${expectedValue}\\s*$`).test(line));
}

function escapeTomlString(value: string): string {
  return value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

type CodexHookConfig = {
  hooks?: {
    SessionStart?: unknown[];
    Stop?: unknown[];
  };
};

function ensureCodexHooksRoot(raw: CodexHookConfig): Required<CodexHookConfig> {
  const next = typeof raw === "object" && raw !== null ? raw : {};
  return {
    hooks: typeof next.hooks === "object" && next.hooks !== null ? next.hooks : {},
  };
}

function ensureCodexEventHook(config: CodexHookConfig, eventName: "SessionStart" | "Stop", command: string, matcher?: string): void {
  const next = ensureCodexHooksRoot(config);
  const existing = Array.isArray(next.hooks[eventName]) ? next.hooks[eventName] as unknown[] : [];
  const hasCommand = existing.some((entry) => {
    if (typeof entry !== "object" || entry === null) return false;
    const typedEntry = entry as JsonRecord;
    if (matcher && typedEntry.matcher !== matcher) return false;
    if (!matcher && typeof typedEntry.matcher === "string" && typedEntry.matcher.length > 0) return false;
    const hookSet = Array.isArray(typedEntry.hooks) ? typedEntry.hooks : [];
    return hookSet.some((hook) => (
      typeof hook === "object"
      && hook !== null
      && (hook as JsonRecord).type === "command"
      && (hook as JsonRecord).command === command
    ));
  });

  if (!hasCommand) {
    existing.push({
      ...(matcher ? { matcher } : {}),
      hooks: [
        {
          type: "command",
          command,
          timeout: 180,
        },
      ],
    });
  }

  next.hooks[eventName] = existing;
  config.hooks = next.hooks;
}

function removeCodexEventHook(config: CodexHookConfig, eventName: "SessionStart" | "Stop", command: string): boolean {
  const next = ensureCodexHooksRoot(config);
  const existing = Array.isArray(next.hooks[eventName]) ? next.hooks[eventName] as unknown[] : [];
  let changed = false;

  const remaining = existing.flatMap((entry) => {
    if (typeof entry !== "object" || entry === null) return [entry];
    const typedEntry = entry as JsonRecord;
    const hookSet = Array.isArray(typedEntry.hooks) ? typedEntry.hooks as unknown[] : [];
    if (!hookSet.length) return [entry];

    const filteredHooks = hookSet.filter((hook) => {
      if (typeof hook !== "object" || hook === null) return true;
      const typedHook = hook as JsonRecord;
      const matches = typedHook.type === "command" && typedHook.command === command;
      if (matches) changed = true;
      return !matches;
    });

    if (!filteredHooks.length) {
      changed = true;
      return [];
    }

    return [{
      ...typedEntry,
      hooks: filteredHooks,
    }];
  });

  if (remaining.length > 0) {
    next.hooks[eventName] = remaining;
  } else if (existing.length > 0) {
    delete next.hooks[eventName];
    changed = true;
  }

  config.hooks = next.hooks;
  return changed;
}

function codexHooksConfigIsEmpty(config: CodexHookConfig): boolean {
  const next = ensureCodexHooksRoot(config);
  return Object.values(next.hooks).every((value) => !Array.isArray(value) || value.length === 0);
}

function normalizeValue(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  return trimmed.length ? trimmed : undefined;
}

function formatCanonicalTarget(input: CanonicalTargetInput): string {
  return `${input.workspaceSlug}/${input.projectSlug}`;
}

function parseCanonicalTarget(value: string): CanonicalTargetInput {
  const normalized = normalizeValue(value);
  if (!normalized) {
    throw new Error("Target cannot be empty. Use <workspaceSlug>/<projectSlug>.");
  }

  const parts = normalized.split("/").map((part) => part.trim()).filter(Boolean);
  if (parts.length !== 2) {
    throw new Error(`Invalid target "${value}". Use <workspaceSlug>/<projectSlug>.`);
  }

  return {
    workspaceSlug: parts[0] as string,
    projectSlug: parts[1] as string,
  };
}

function parseLegacyProjectReference(value: string): { workspaceToken?: string; projectToken: string } {
  const normalized = normalizeValue(value);
  if (!normalized) {
    throw new Error("Legacy project value is empty.");
  }

  const slash = normalized.indexOf("/");
  if (slash <= 0 || slash === normalized.length - 1) {
    return { projectToken: normalized };
  }

  const workspaceToken = normalizeValue(normalized.slice(0, slash));
  const projectToken = normalizeValue(normalized.slice(slash + 1));
  if (!workspaceToken || !projectToken) {
    throw new Error(`Invalid legacy target "${value}".`);
  }

  return {
    workspaceToken,
    projectToken,
  };
}

function getWorkspaceConfigPath(cwd: string): string {
  return join(cwd, ".recallstack", "workspace.json");
}

function getAgentSettingsPath(cwd: string, scope: AgentInstallScope = "local"): string {
  return scope === "global"
    ? join(getGlobalRecallstackDir(), "agent-settings.json")
    : join(cwd, ".recallstack", "agent-settings.json");
}

function readAgentSettings(cwd: string, scope: AgentInstallScope = "local"): AgentSettings {
  return readJsonFile<AgentSettings>(getAgentSettingsPath(cwd, scope), {});
}

function writeAgentSettings(cwd: string, settings: AgentSettings, scope: AgentInstallScope = "local"): string {
  const path = getAgentSettingsPath(cwd, scope);
  writeJsonFile(path, settings);
  return path;
}

function mergeAgentSettings(globalSettings: AgentSettings, localSettings: AgentSettings): AgentSettings {
  const merged: AgentSettings = { ...globalSettings };
  for (const target of ["codex", "claude", "copilot"] as const) {
    const local = localSettings[target];
    if (local && typeof local === "object") {
      merged[target] = {
        ...(merged[target] || {}),
        ...local,
      };
    }
  }
  return merged;
}

function readResolvedAgentSettings(cwd: string): AgentSettings {
  return mergeAgentSettings(readAgentSettings(cwd, "global"), readAgentSettings(cwd, "local"));
}

function resolvedWorkerModel(cwd: string, target: WorkerModelTarget): { value: string | null; source: AgentInstallScope | null } {
  const localValue = readAgentSettings(cwd, "local")[target]?.worker_model;
  if (typeof localValue === "string" && localValue.trim().length) {
    return { value: localValue.trim(), source: "local" };
  }
  const globalValue = readAgentSettings(cwd, "global")[target]?.worker_model;
  if (typeof globalValue === "string" && globalValue.trim().length) {
    return { value: globalValue.trim(), source: "global" };
  }
  return { value: null, source: null };
}

function targetsForWorkerModel(target: AgentTarget): WorkerModelTarget[] {
  if (target === "all") {
    return ["codex", "claude", "copilot"];
  }
  if (target === "cursor") {
    return [];
  }
  return [target];
}

function applyWorkerModelSetting(
  cwd: string,
  target: AgentTarget,
  workerModel?: string,
  scope: AgentInstallScope = "local",
): { path?: string; targets: WorkerModelTarget[] } {
  const normalized = normalizeValue(workerModel);
  if (!normalized) {
    return { targets: [] };
  }

  const targets = targetsForWorkerModel(target);
  if (!targets.length) {
    return { targets: [] };
  }

  const current = readAgentSettings(cwd, scope);
  const next: AgentSettings = { ...current };
  for (const agent of targets) {
    next[agent] = { worker_model: normalized };
  }

  return {
    path: writeAgentSettings(cwd, next, scope),
    targets,
  };
}

function clearWorkerModelSetting(
  cwd: string,
  target: AgentTarget,
  scope: AgentInstallScope = "local",
): { path: string; removedTargets: WorkerModelTarget[] } | undefined {
  const targets = targetsForWorkerModel(target);
  if (!targets.length) return undefined;

  const current = readAgentSettings(cwd, scope);
  let changed = false;
  const next: AgentSettings = { ...current };

  for (const agent of targets) {
    if (!next[agent]) continue;
    delete next[agent];
    changed = true;
  }

  if (!changed) return undefined;

  if (Object.keys(next).length === 0) {
    removePathIfExists(getAgentSettingsPath(cwd, scope));
    return {
      path: getAgentSettingsPath(cwd, scope),
      removedTargets: targets,
    };
  }

  return {
    path: writeAgentSettings(cwd, next, scope),
    removedTargets: targets,
  };
}

function configuredWorkerModel(cwd: string, target: QueryWorkerTarget): string | null {
  return resolvedWorkerModel(cwd, target).value;
}

function commandAvailable(command: string, versionArg = "--version"): boolean {
  const result = spawnSync(command, [versionArg], {
    encoding: "utf8",
    stdio: "ignore",
  });
  return !result.error;
}

function runCommandAsync(command: string, args: string[], options: {
  cwd?: string;
  env?: NodeJS.ProcessEnv;
  input?: string;
} = {}): Promise<{ status: number | null; stdout: string; stderr: string }> {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, {
      cwd: options.cwd,
      env: options.env,
      stdio: ["pipe", "pipe", "pipe"],
    });

    let stdout = "";
    let stderr = "";

    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      stdout += chunk;
    });

    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk) => {
      stderr += chunk;
    });

    child.on("error", (error) => {
      reject(error);
    });

    child.on("close", (status) => {
      resolvePromise({
        status,
        stdout,
        stderr,
      });
    });

    if (typeof options.input === "string" && options.input.length) {
      child.stdin.write(options.input);
    }
    child.stdin.end();
  });
}

function startCliSpinner(message: string): { stop: () => void } {
  const script = `
const frames = ["-","\\\\","|","/"];
let index = 0;
const message = ${JSON.stringify(message)};
const render = () => {
  process.stderr.write("\\x1b[2K\\r" + frames[index] + " " + message + "\\r");
  index = (index + 1) % frames.length;
};
render();
const timer = setInterval(render, 90);
process.on("SIGTERM", () => {
  clearInterval(timer);
  process.exit(0);
});
`;
  const child = spawn(process.execPath, ["-e", script], {
    stdio: ["ignore", "ignore", "inherit"],
    env: process.env,
  });

  let stopped = false;
  return {
    stop: () => {
      if (stopped) return;
      stopped = true;
      child.kill("SIGTERM");
      process.stderr.write("\x1b[2K\r");
    },
  };
}

function workerTargetAvailable(target: QueryWorkerTarget): boolean {
  if (target === "codex") return commandAvailable("codex");
  if (target === "claude") return commandAvailable("claude");
  return commandAvailable("copilot");
}

function resolveQueryWorkerTarget(cwd: string, explicit?: string): QueryWorkerTarget {
  const normalized = normalizeValue(explicit);
  if (normalized) {
    if (normalized !== "codex" && normalized !== "claude" && normalized !== "copilot") {
      throw new Error('Invalid --worker-target. Use "codex", "claude", or "copilot".');
    }
    return normalized;
  }

  const settings = readResolvedAgentSettings(cwd);
  for (const target of ["codex", "claude", "copilot"] as const) {
    const configured = settings[target]?.worker_model;
    if (typeof configured === "string" && configured.trim().length) {
      return target;
    }
  }

  for (const target of ["codex", "claude", "copilot"] as const) {
    if (workerTargetAvailable(target)) {
      return target;
    }
  }

  throw new Error("No local synthesis worker is available. Install codex, claude, or copilot, or pass --worker-target.");
}

type QuerySynthesisResult = {
  answer: string;
  primary_ids: string[];
  context_ids: string[];
  caveats: string[];
};

function compactList(values: unknown): string[] {
  return Array.isArray(values)
    ? values.map((value) => (typeof value === "string" ? value.trim() : "")).filter(Boolean)
    : [];
}

function coerceQuerySynthesisResult(raw: unknown): QuerySynthesisResult | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const candidate = raw as Record<string, unknown>;
  const answer = typeof candidate.answer === "string" ? candidate.answer.trim() : "";
  if (!answer.length) return null;
  return {
    answer,
    primary_ids: compactList(candidate.primary_ids),
    context_ids: compactList(candidate.context_ids),
    caveats: compactList(candidate.caveats),
  };
}

function parseClaudeSynthesisOutput(raw: string): QuerySynthesisResult | null {
  const direct = coerceQuerySynthesisResult(readJsonFromString<unknown>(raw, null));
  if (direct) return direct;
  const wrapper = readJsonFromString<Record<string, unknown> | null>(raw, null);
  if (!wrapper || typeof wrapper !== "object" || Array.isArray(wrapper)) {
    return null;
  }
  const resultText = typeof wrapper.result === "string"
    ? wrapper.result
    : Array.isArray(wrapper.content)
      ? wrapper.content
        .map((item) => (item && typeof item === "object" && (item as { type?: unknown }).type === "text" && typeof (item as { text?: unknown }).text === "string"
          ? (item as { text: string }).text
          : ""))
        .filter(Boolean)
        .join("\n")
      : "";
  return resultText.length ? coerceQuerySynthesisResult(readJsonFromString<unknown>(resultText, null)) : null;
}

function compactWhitespace(value: string): string {
  return value.replace(/\s+/g, " ").trim();
}

function splitEvidenceContent(content: string): { title: string; detail: string | null } {
  const normalized = compactWhitespace(content);
  if (!normalized) {
    return {
      title: "Untitled memory item",
      detail: null,
    };
  }
  const sentenceMatch = normalized.match(/^.+?[.!?](?:\s|$)/);
  const title = (sentenceMatch?.[0] || normalized).trim().replace(/[.!?]$/, "");
  const remainder = normalized.slice(sentenceMatch?.[0]?.length || title.length).trim();
  return {
    title: title || normalized,
    detail: remainder || null,
  };
}

function formatConfidence(value: unknown): string {
  return typeof value === "number" ? value.toFixed(2) : "--";
}

function formatRelativeTime(value: unknown): string {
  if (typeof value !== "string") return "unknown time";
  const createdAt = new Date(value);
  if (Number.isNaN(createdAt.valueOf())) return "unknown time";
  const diffMs = Date.now() - createdAt.valueOf();
  const diffMinutes = Math.max(0, Math.round(diffMs / 60000));
  if (diffMinutes < 1) return "just now";
  if (diffMinutes < 60) return `${diffMinutes} min ago`;
  const diffHours = Math.round(diffMinutes / 60);
  if (diffHours < 24) return `${diffHours} hour${diffHours === 1 ? "" : "s"} ago`;
  const diffDays = Math.round(diffHours / 24);
  return `${diffDays} day${diffDays === 1 ? "" : "s"} ago`;
}

function formatMemoryRetrieveForCli(response: any): string {
  const primaryItems = Array.isArray(response?.evidence?.primary) ? response.evidence.primary.slice(0, 5) : [];
  const contextItems = Array.isArray(response?.evidence?.context) ? response.evidence.context.slice(0, 3) : [];
  const lines: string[] = [];
  lines.push("STRATEGY");
  lines.push(
    `${response?.strategy?.family || "hybrid_graph_augmented"} · route=${response?.meta?.intent_route || "unknown"} · mode=${response?.meta?.retrieval_mode || "standard"} · packing=${response?.strategy?.evidence_packing?.objective || "balanced"} · graph_depth=${response?.strategy?.graph_expansion?.depth ?? response?.meta?.graph_depth ?? 0}`,
  );
  if (typeof response?.strategy?.normalized_query === "string" && response.strategy.normalized_query.trim().length) {
    lines.push(`Normalized query: ${response.strategy.normalized_query.trim()}`);
  }

  if (contextItems.length) {
    lines.push("");
    lines.push("CONTEXT");
    for (const item of contextItems) {
      const { title, detail } = splitEvidenceContent(typeof item?.content === "string" ? item.content : "");
      const metadata = [
        item?.project?.slug || item?.project?.name || null,
        formatRelativeTime(item?.createdAt),
      ].filter((value): value is string => Boolean(value));
      lines.push(`${formatConfidence(item?.confidence)}  [${item?.id || "unknown"}] ${title}`);
      if (detail) {
        lines.push(`    ${detail}`);
      }
      if (metadata.length) {
        lines.push(`    ${metadata.join(" · ")}`);
      }
      lines.push("");
    }
  }

  lines.push("");
  lines.push("EVIDENCE");
  if (!primaryItems.length) {
    lines.push("No primary evidence returned.");
    return lines.join("\n").trim();
  }

  for (const item of primaryItems) {
    const { title, detail } = splitEvidenceContent(typeof item?.content === "string" ? item.content : "");
    const metadata = [
      item?.project?.slug || item?.project?.name || null,
      formatRelativeTime(item?.createdAt),
    ].filter((value): value is string => Boolean(value));
    lines.push(`${formatConfidence(item?.confidence)}  [${item?.id || "unknown"}] ${title}`);
    if (detail) {
      lines.push(`    ${detail}`);
    }
    if (metadata.length) {
      lines.push(`    ${metadata.join(" · ")}`);
    }
    lines.push("");
  }

  return lines.join("\n").trim();
}

function buildQuerySynthesisPrompt(query: string, retrieval: any): string {
  return [
    "You are Recallstack's local memory-query synthesis worker.",
    "Return valid JSON matching the provided schema exactly.",
    "Use only the retrieval payload below.",
    "Write a concise answer to the user's query.",
    "Treat synthesis as a hint layered on top of evidence, not as proof.",
    "If evidence is partial or conflicting, say so directly in the answer.",
    "Never invent memory ids, implementation details, or conclusions that are not supported by the payload.",
    "Put the strongest ids you relied on in primary_ids/context_ids.",
    "",
    "QUERY",
    query,
    "",
    "SCHEMA",
    JSON.stringify(QUERY_SYNTHESIS_SCHEMA),
    "",
    "RETRIEVAL PAYLOAD",
    JSON.stringify(retrieval, null, 2),
  ].join("\n");
}

async function synthesizeWithCodex(query: string, retrieval: any, workerModel: string | null): Promise<QuerySynthesisResult> {
  const workDir = mkdtempSync(join(tmpdir(), "recallstack-query-synth-codex-"));
  const schemaPath = join(workDir, "schema.json");
  const outputPath = join(workDir, "output.json");
  writeJsonFile(schemaPath, QUERY_SYNTHESIS_SCHEMA);
  const args = [
    "exec",
    "--ephemeral",
    "--disable",
    "codex_hooks",
    "--skip-git-repo-check",
    "-C",
    workDir,
    "--output-schema",
    schemaPath,
    "--output-last-message",
    outputPath,
    "-",
  ];
  if (workerModel) {
    args.splice(1, 0, "--model", workerModel);
  }
  const result = await runCommandAsync("codex", args, {
    cwd: workDir,
    input: buildQuerySynthesisPrompt(query, retrieval),
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  try {
    if (result.status !== 0 || !existsSync(outputPath)) {
      throw new Error(`Codex synthesis failed.\n${String(result.stderr || result.stdout || "").trim()}`);
    }
    const parsed = coerceQuerySynthesisResult(readJsonFile<unknown>(outputPath, null));
    if (!parsed) {
      throw new Error("Codex synthesis returned unreadable output.");
    }
    return parsed;
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
}

async function synthesizeWithClaude(query: string, retrieval: any, workerModel: string | null): Promise<QuerySynthesisResult> {
  const args = [
    "-p",
    "--output-format",
    "json",
    "--json-schema",
    JSON.stringify(QUERY_SYNTHESIS_SCHEMA),
    "--tools",
    "",
    "--no-session-persistence",
    "--append-system-prompt",
    "You are a strict JSON synthesis pass for Recallstack memory query output.",
  ];
  if (workerModel) {
    args.splice(1, 0, "--model", workerModel);
  }
  const result = await runCommandAsync("claude", args, {
    cwd: process.cwd(),
    input: buildQuerySynthesisPrompt(query, retrieval),
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  if (result.status !== 0) {
    throw new Error(`Claude synthesis failed.\n${String(result.stderr || result.stdout || "").trim()}`);
  }
  const parsed = parseClaudeSynthesisOutput(result.stdout || "");
  if (!parsed) {
    throw new Error("Claude synthesis returned unreadable output.");
  }
  return parsed;
}

async function synthesizeWithCopilot(query: string, retrieval: any, workerModel: string | null): Promise<QuerySynthesisResult> {
  const configDir = mkdtempSync(join(tmpdir(), "recallstack-query-synth-copilot-"));
  const args = [
    "--config-dir",
    configDir,
    "--no-custom-instructions",
    "--allow-all-tools",
    "--output-format",
    "text",
    "--silent",
    "-p",
    buildQuerySynthesisPrompt(query, retrieval),
  ];
  if (workerModel) {
    args.unshift(workerModel);
    args.unshift("--model");
  }
  const result = await runCommandAsync("copilot", args, {
    cwd: configDir,
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  try {
    if (result.status !== 0) {
      throw new Error(`Copilot synthesis failed.\n${String(result.stderr || result.stdout || "").trim()}`);
    }
    const parsed = coerceQuerySynthesisResult(readJsonFromString<unknown>(result.stdout || "", null));
    if (!parsed) {
      throw new Error("Copilot synthesis returned unreadable output.");
    }
    return parsed;
  } finally {
    rmSync(configDir, { recursive: true, force: true });
  }
}

async function synthesizeMemoryQueryLocally(options: {
  cwd: string;
  query: string;
  retrieval: any;
  workerTarget?: string;
  workerModel?: string;
}): Promise<QuerySynthesisResult & { worker_target: QueryWorkerTarget; worker_model: string | null }> {
  const workerTarget = resolveQueryWorkerTarget(options.cwd, options.workerTarget);
  if (!workerTargetAvailable(workerTarget)) {
    throw new Error(`The ${workerTarget} worker is not available on PATH.`);
  }
  const workerModel = normalizeValue(options.workerModel) || configuredWorkerModel(options.cwd, workerTarget) || null;
  const base = workerTarget === "codex"
    ? await synthesizeWithCodex(options.query, options.retrieval, workerModel)
    : workerTarget === "claude"
      ? await synthesizeWithClaude(options.query, options.retrieval, workerModel)
      : await synthesizeWithCopilot(options.query, options.retrieval, workerModel);
  return {
    ...base,
    worker_target: workerTarget,
    worker_model: workerModel,
  };
}

function findWorkspaceConfig(startCwd: string): { path?: string; config: WorkspaceRecallstackConfig } {
  let cursor = resolve(startCwd);
  while (true) {
    const candidate = getWorkspaceConfigPath(cursor);
    if (existsSync(candidate)) {
      const parsed = readJsonFile<WorkspaceRecallstackConfig>(candidate, {});
      return { path: candidate, config: parsed || {} };
    }
    const parent = dirname(cursor);
    if (parent === cursor) break;
    cursor = parent;
  }
  return { config: {} };
}

function readStoredTarget(config: WorkspaceRecallstackConfig): {
  workspaceId?: string;
  workspaceSlug?: string;
  workspaceName?: string;
  projectId?: string;
  projectSlug?: string;
  projectName?: string;
  projectMode?: "GLOBAL" | "PROJECT";
} | undefined {
  const workspaceSlug = normalizeValue(config.workspace_slug);
  const projectSlug = normalizeValue(config.project_slug);
  const workspaceId = normalizeValue(config.workspace_id);
  const projectId = normalizeValue(config.project_id);
  const workspaceName = normalizeValue(config.workspace_name);
  const projectName = normalizeValue(config.project_name);
  const projectMode = config.project_mode === "GLOBAL" || config.project_mode === "PROJECT"
    ? config.project_mode
    : undefined;

  if (workspaceSlug && projectSlug) {
    return {
      workspaceId,
      workspaceSlug,
      workspaceName,
      projectId,
      projectSlug,
      projectName,
      projectMode,
    };
  }

  if (workspaceId && projectSlug) {
    return {
      workspaceId,
      workspaceSlug,
      workspaceName,
      projectSlug,
      projectId,
      projectName,
      projectMode,
    };
  }

  return undefined;
}

function fallbackTargetFromStored(stored: ReturnType<typeof readStoredTarget>): ResolvedTarget | undefined {
  if (!stored?.workspaceId || !stored.projectSlug) {
    return undefined;
  }
  const projectMode = stored.projectMode
    || (stored.projectSlug === "global" ? "GLOBAL" : "PROJECT");
  return {
    workspaceId: stored.workspaceId,
    workspaceSlug: stored.workspaceSlug || stored.workspaceId,
    workspaceName: stored.workspaceName || stored.workspaceSlug || "Stored workspace",
    projectId: stored.projectId || stored.projectSlug,
    projectSlug: stored.projectSlug,
    projectName: stored.projectName || stored.projectSlug,
    mode: projectMode,
  };
}

function saveWorkspaceTarget(cwd: string, target: ResolvedTarget): string {
  const path = getWorkspaceConfigPath(cwd);
  const existing = readJsonFile<WorkspaceRecallstackConfig>(path, {});
  writeJsonFile(path, {
    ...existing,
    workspace_id: target.workspaceId,
    workspace_slug: target.workspaceSlug,
    workspace_name: target.workspaceName,
    project_id: target.projectId,
    project_slug: target.projectSlug,
    project_name: target.projectName,
    project_mode: target.mode,
    project: undefined,
    workspace_root: cwd,
    updated_at: new Date().toISOString(),
  });
  return path;
}

function clearWorkspaceTarget(cwd: string): string {
  const path = findWorkspaceConfig(cwd).path || getWorkspaceConfigPath(cwd);
  const existing = readJsonFile<WorkspaceRecallstackConfig>(path, {});
  writeJsonFile(path, {
    ...existing,
    workspace_id: undefined,
    workspace_slug: undefined,
    workspace_name: undefined,
    project_id: undefined,
    project_slug: undefined,
    project_name: undefined,
    project_mode: undefined,
    project: undefined,
    workspace_root: cwd,
    updated_at: new Date().toISOString(),
  });
  return path;
}

async function listWorkspaces(config: ReturnType<typeof loadConfig>): Promise<WorkspaceSummary[]> {
  const response = await authenticatedHttp<WorkspacesResponse>("/v1/workspaces", {}, config);
  const items = Array.isArray(response.items) ? response.items : [];
  const out: WorkspaceSummary[] = [];
  for (const item of items) {
    const id = normalizeValue(item.workspace?.id);
    const slug = normalizeValue(item.workspace?.slug);
    const name = normalizeValue(item.workspace?.name);
    if (!id || !slug || !name) continue;
    out.push({ id, slug, name });
  }
  return out;
}

async function listWorkspaceMemberships(config: ReturnType<typeof loadConfig>): Promise<WorkspaceMembershipSummary[]> {
  const response = await authenticatedHttp<WorkspacesResponse>("/v1/workspaces", {}, config);
  const activeWorkspaceId = normalizeValue(response.activeWorkspaceId);
  const items = Array.isArray(response.items) ? response.items : [];
  const out: WorkspaceMembershipSummary[] = [];

  for (const item of items) {
    const workspaceId = normalizeValue(item.workspace?.id);
    const workspaceSlug = normalizeValue(item.workspace?.slug);
    const workspaceName = normalizeValue(item.workspace?.name);
    if (!workspaceId || !workspaceSlug || !workspaceName) continue;

    out.push({
      id: workspaceId,
      slug: workspaceSlug,
      name: workspaceName,
      kind: normalizeValue(item.workspace?.kind) || "UNKNOWN",
      role: normalizeValue(item.role) || "UNKNOWN",
      status: normalizeValue(item.status) || "UNKNOWN",
      isActive: workspaceId === activeWorkspaceId,
    });
  }

  return out;
}

async function listProjectsForWorkspace(
  config: ReturnType<typeof loadConfig>,
  workspaceId: string,
): Promise<ProjectSummary[]> {
  const listing = await authenticatedHttp<{ items?: ProjectSummary[] }>(
    "/v1/projects",
    { workspaceId },
    config,
  );
  return Array.isArray(listing.items) ? listing.items : [];
}

async function resolveWorkspaceBySlug(
  config: ReturnType<typeof loadConfig>,
  workspaceSlug: string,
): Promise<WorkspaceSummary> {
  const workspaces = await listWorkspaces(config);
  const workspace = workspaces.find((item) => item.slug === workspaceSlug);
  if (!workspace) {
    throw new Error(`Workspace "${workspaceSlug}" not found. Run \`recallstack project use\` to choose from accessible workspaces.`);
  }
  return workspace;
}

async function resolveWorkspaceByToken(
  config: ReturnType<typeof loadConfig>,
  workspaceToken: string,
): Promise<WorkspaceSummary> {
  const workspaces = await listWorkspaces(config);
  const bySlug = workspaces.find((item) => item.slug === workspaceToken);
  if (bySlug) return bySlug;
  const byId = workspaces.find((item) => item.id === workspaceToken);
  if (byId) return byId;
  throw new Error(`Workspace "${workspaceToken}" not found. Run \`recallstack workspace list\`.`);
}

function getConfiguredWorkspace(config: ReturnType<typeof loadConfig>): WorkspaceSummary | undefined {
  const id = normalizeValue(config.activeWorkspaceId);
  const slug = normalizeValue(config.activeWorkspaceSlug);
  const name = normalizeValue(config.activeWorkspaceName);
  if (!id && !slug) return undefined;

  return {
    id: id || "",
    slug: slug || "",
    name: name || "Configured workspace",
  };
}

async function resolveConfiguredWorkspace(config: ReturnType<typeof loadConfig>): Promise<WorkspaceSummary | undefined> {
  const configured = getConfiguredWorkspace(config);
  if (!configured) return undefined;

  const workspaces = await listWorkspaces(config);
  let resolved: WorkspaceSummary | undefined;
  if (configured.id) {
    resolved = workspaces.find((item) => item.id === configured.id);
  }
  if (!resolved && configured.slug) {
    resolved = workspaces.find((item) => item.slug === configured.slug);
  }
  return resolved;
}

async function resolveTargetByCanonicalInput(
  config: ReturnType<typeof loadConfig>,
  input: CanonicalTargetInput,
): Promise<ResolvedTarget> {
  const workspace = await resolveWorkspaceBySlug(config, input.workspaceSlug);
  const projects = await listProjectsForWorkspace(config, workspace.id);
  const project = projects.find((item) => item.slug === input.projectSlug);
  if (!project) {
    throw new Error(`Project "${input.projectSlug}" not found in workspace "${workspace.slug}".`);
  }

  return {
    workspaceId: workspace.id,
    workspaceSlug: workspace.slug,
    workspaceName: workspace.name,
    projectId: project.id,
    projectSlug: project.slug,
    projectName: project.name,
    mode: project.mode,
  };
}

async function resolveProjectTokenInWorkspace(
  config: ReturnType<typeof loadConfig>,
  workspace: WorkspaceSummary,
  projectToken: string,
): Promise<ResolvedTarget | undefined> {
  const projects = await listProjectsForWorkspace(config, workspace.id);
  const project = projects.find((item) => item.slug === projectToken || item.id === projectToken);
  if (!project) return undefined;

  return {
    workspaceId: workspace.id,
    workspaceSlug: workspace.slug,
    workspaceName: workspace.name,
    projectId: project.id,
    projectSlug: project.slug,
    projectName: project.name,
    mode: project.mode,
  };
}

async function tryLoadLocalTarget(
  cwd: string,
  config: ReturnType<typeof loadConfig>,
): Promise<ResolvedTarget | undefined> {
  try {
    return await loadLocalTarget(cwd, config);
  } catch {
    return undefined;
  }
}

async function resolveWorkspaceContextForCommand(
  config: ReturnType<typeof loadConfig>,
  cwd: string,
): Promise<WorkspaceSummary | undefined> {
  const localTarget = await tryLoadLocalTarget(cwd, config);
  if (localTarget) {
    return {
      id: localTarget.workspaceId,
      slug: localTarget.workspaceSlug,
      name: localTarget.workspaceName,
    };
  }

  return resolveConfiguredWorkspace(config);
}

async function resolveTargetByInputValue(
  config: ReturnType<typeof loadConfig>,
  value: string,
  cwd: string,
  options: {
    preferConfiguredWorkspace?: boolean;
  } = {},
): Promise<ResolvedTarget> {
  const normalized = normalizeValue(value);
  if (!normalized) {
    throw new Error("Project value cannot be empty. Use <projectSlug> or <workspaceSlug/projectSlug>.");
  }

  if (normalized.includes("/")) {
    return resolveTargetByCanonicalInput(config, parseCanonicalTarget(normalized));
  }

  const workspaceContext = options.preferConfiguredWorkspace
    ? (await resolveConfiguredWorkspace(config)) ?? await resolveWorkspaceContextForCommand(config, cwd)
    : await resolveWorkspaceContextForCommand(config, cwd);
  if (workspaceContext) {
    const inWorkspace = await resolveProjectTokenInWorkspace(config, workspaceContext, normalized);
    if (inWorkspace) return inWorkspace;
    throw new Error(
      `Project "${normalized}" was not found in workspace "${workspaceContext.slug}". Use \`recallstack project list --workspace ${workspaceContext.slug}\` or pass <workspaceSlug/projectSlug>.`,
    );
  }

  const workspaces = await listWorkspaces(config);
  const matches: ResolvedTarget[] = [];
  for (const workspace of workspaces) {
    const match = await resolveProjectTokenInWorkspace(config, workspace, normalized);
    if (match) matches.push(match);
  }

  if (matches.length === 1) {
    return matches[0] as ResolvedTarget;
  }

  if (matches.length === 0) {
    throw new Error(
      `Project "${normalized}" was not found. Run \`recallstack project use\` or pass <workspaceSlug/projectSlug>.`,
    );
  }

  const matchOptions = matches
    .map((item) => formatCanonicalTarget({ workspaceSlug: item.workspaceSlug, projectSlug: item.projectSlug }))
    .join(", ");
  throw new Error(`Project "${normalized}" is ambiguous across workspaces: ${matchOptions}. Use <workspaceSlug/projectSlug>.`);
}

async function resolveTargetByLegacyProjectValue(
  config: ReturnType<typeof loadConfig>,
  value: string,
): Promise<ResolvedTarget> {
  const parsed = parseLegacyProjectReference(value);

  if (parsed.workspaceToken) {
    const workspace = await resolveWorkspaceByToken(config, parsed.workspaceToken);
    const projects = await listProjectsForWorkspace(config, workspace.id);
    const project = projects.find((item) => item.slug === parsed.projectToken || item.id === parsed.projectToken);
    if (!project) {
      throw new Error(`Legacy target "${value}" could not be resolved. Project was not found in workspace "${workspace.slug}".`);
    }

    return {
      workspaceId: workspace.id,
      workspaceSlug: workspace.slug,
      workspaceName: workspace.name,
      projectId: project.id,
      projectSlug: project.slug,
      projectName: project.name,
      mode: project.mode,
    };
  }

  const workspaces = await listWorkspaces(config);
  const matches: ResolvedTarget[] = [];

  for (const workspace of workspaces) {
    const projects = await listProjectsForWorkspace(config, workspace.id);
    for (const project of projects) {
      if (project.slug === parsed.projectToken || project.id === parsed.projectToken) {
        matches.push({
          workspaceId: workspace.id,
          workspaceSlug: workspace.slug,
          workspaceName: workspace.name,
          projectId: project.id,
          projectSlug: project.slug,
          projectName: project.name,
          mode: project.mode,
        });
      }
    }
  }

  if (matches.length === 1) {
    return matches[0] as ResolvedTarget;
  }

  if (matches.length === 0) {
    throw new Error(`Legacy target "${value}" could not be resolved. Run \`recallstack project use\` to set a new default.`);
  }

  const options = matches
    .map((item) => formatCanonicalTarget({ workspaceSlug: item.workspaceSlug, projectSlug: item.projectSlug }))
    .join(", ");
  throw new Error(`Legacy target "${value}" is ambiguous. Matching targets: ${options}. Run \`recallstack project use <workspaceSlug/projectSlug>\`.`);
}

async function loadLocalTarget(cwd: string, config: ReturnType<typeof loadConfig>): Promise<ResolvedTarget | undefined> {
  const workspaceConfig = findWorkspaceConfig(cwd);
  if (!workspaceConfig.path) {
    return undefined;
  }

  const stored = readStoredTarget(workspaceConfig.config);
  const fallback = fallbackTargetFromStored(stored);
  if (stored?.workspaceSlug && stored.projectSlug) {
    try {
      try {
        const resolved = await resolveTargetByCanonicalInput(config, {
          workspaceSlug: stored.workspaceSlug,
          projectSlug: stored.projectSlug,
        });
        const hasIdDrift = stored.workspaceId !== resolved.workspaceId || stored.projectId !== resolved.projectId;
        if (hasIdDrift) {
          saveWorkspaceTarget(cwd, resolved);
        }
        return resolved;
      } catch (error) {
        if (!stored.workspaceId) {
          throw error;
        }

        const workspaces = await listWorkspaces(config);
        const workspace = workspaces.find((item) => item.id === stored.workspaceId);
        if (!workspace) {
          throw error;
        }

        const projects = await listProjectsForWorkspace(config, workspace.id);
        const project = projects.find((item) => (
          (stored.projectId && item.id === stored.projectId)
          || item.slug === stored.projectSlug
        ));
        if (!project) {
          throw error;
        }

        const resolved: ResolvedTarget = {
          workspaceId: workspace.id,
          workspaceSlug: workspace.slug,
          workspaceName: workspace.name,
          projectId: project.id,
          projectSlug: project.slug,
          projectName: project.name,
          mode: project.mode,
        };
        saveWorkspaceTarget(cwd, resolved);
        console.warn(`Repaired drifted target mapping to canonical format: ${formatCanonicalTarget({ workspaceSlug: resolved.workspaceSlug, projectSlug: resolved.projectSlug })}`);
        return resolved;
      }
    } catch (error) {
      if (fallback) {
        return fallback;
      }
      throw error;
    }
  }

  if (stored?.workspaceId && stored.projectSlug) {
    try {
      const workspaces = await listWorkspaces(config);
      const workspace = workspaces.find((item) => item.id === stored.workspaceId);
      if (!workspace) {
        throw new Error("Stored workspace target is no longer accessible. Run `recallstack project use`.");
      }

      const projects = await listProjectsForWorkspace(config, workspace.id);
      const project = projects.find((item) => item.slug === stored.projectSlug || (stored.projectId && item.id === stored.projectId));
      if (!project) {
        throw new Error("Stored project target is no longer accessible. Run `recallstack project use`.");
      }

      const resolved: ResolvedTarget = {
        workspaceId: workspace.id,
        workspaceSlug: workspace.slug,
        workspaceName: workspace.name,
        projectId: project.id,
        projectSlug: project.slug,
        projectName: project.name,
        mode: project.mode,
      };
      saveWorkspaceTarget(cwd, resolved);
      console.warn(`Migrated workspace target to canonical format: ${formatCanonicalTarget({ workspaceSlug: resolved.workspaceSlug, projectSlug: resolved.projectSlug })}`);
      return resolved;
    } catch (error) {
      if (fallback) {
        return fallback;
      }
      throw error;
    }
  }

  const legacyProject = normalizeValue(workspaceConfig.config.project);
  if (legacyProject) {
    const resolved = await resolveTargetByLegacyProjectValue(config, legacyProject);
    saveWorkspaceTarget(cwd, resolved);
    console.warn(`Migrated legacy target to canonical format: ${formatCanonicalTarget({ workspaceSlug: resolved.workspaceSlug, projectSlug: resolved.projectSlug })}`);
    return resolved;
  }

  return undefined;
}

async function resolveTargetForCommand(input: {
  config: ReturnType<typeof loadConfig>;
  cwd: string;
  explicit?: string;
  requireWritable?: boolean;
}): Promise<ResolvedTarget> {
  let resolved: ResolvedTarget | undefined;

  const explicit = normalizeValue(input.explicit);
  if (explicit) {
    resolved = await resolveTargetByInputValue(input.config, explicit, input.cwd);
  } else {
    resolved = await loadLocalTarget(input.cwd, input.config);
    if (!resolved) {
      throw new Error("No default project configured for this directory. Run `recallstack project use` (or `recallstack workspace use`) or pass `--project <projectSlug|workspaceSlug/projectSlug>`.");
    }
  }

  if (input.requireWritable && resolved.mode === "GLOBAL") {
    throw new Error("GLOBAL_WRITE_FORBIDDEN: raw memory writes cannot target global. Choose a non-global project with `recallstack project use`.");
  }

  return resolved;
}

async function listWritableTargets(config: ReturnType<typeof loadConfig>): Promise<ResolvedTarget[]> {
  const workspaces = await listWorkspaces(config);
  const byWorkspace = await Promise.all(
    workspaces.map(async (workspace) => ({
      workspace,
      projects: await listProjectsForWorkspace(config, workspace.id),
    })),
  );
  return toWritableTargets(byWorkspace);
}

async function listWritableTargetsForWorkspace(
  config: ReturnType<typeof loadConfig>,
  workspace: WorkspaceSummary,
): Promise<ResolvedTarget[]> {
  const byWorkspace = await Promise.all(
    [workspace].map(async (row) => ({
      workspace: row,
      projects: await listProjectsForWorkspace(config, row.id),
    })),
  );
  return toWritableTargets(byWorkspace);
}

function toWritableTargets(byWorkspace: Array<{ workspace: WorkspaceSummary; projects: ProjectSummary[] }>): ResolvedTarget[] {
  const out: ResolvedTarget[] = [];
  for (const entry of byWorkspace) {
    for (const project of entry.projects) {
      if (project.mode !== "PROJECT") continue;
      out.push({
        workspaceId: entry.workspace.id,
        workspaceSlug: entry.workspace.slug,
        workspaceName: entry.workspace.name,
        projectId: project.id,
        projectSlug: project.slug,
        projectName: project.name,
        mode: project.mode,
      });
    }
  }

  out.sort((a, b) => {
    const wsCompare = a.workspaceSlug.localeCompare(b.workspaceSlug);
    if (wsCompare !== 0) return wsCompare;
    return a.projectSlug.localeCompare(b.projectSlug);
  });

  return out;
}

async function promptForTargetChoice(
  targets: ResolvedTarget[],
  defaultTarget?: ResolvedTarget,
): Promise<ResolvedTarget> {
  if (!process.stdin.isTTY || !process.stdout.isTTY) {
    throw new Error("Project selection requires TTY input. Use `recallstack project use <projectSlug|workspaceSlug/projectSlug>` in non-interactive mode.");
  }

  if (targets.length === 0) {
    throw new Error("No writable projects found across accessible workspaces.");
  }

  const defaultCanonical = defaultTarget
    ? formatCanonicalTarget({ workspaceSlug: defaultTarget.workspaceSlug, projectSlug: defaultTarget.projectSlug })
    : undefined;
  const defaultIndex = Math.max(
    0,
    defaultCanonical
      ? targets.findIndex((item) => formatCanonicalTarget({ workspaceSlug: item.workspaceSlug, projectSlug: item.projectSlug }) === defaultCanonical)
      : 0,
  );

  console.log("Select a default project:");
  let currentWorkspaceSlug = "";
  targets.forEach((target, index) => {
    if (target.workspaceSlug !== currentWorkspaceSlug) {
      currentWorkspaceSlug = target.workspaceSlug;
      console.log(`\n${target.workspaceName} (${target.workspaceSlug})`);
    }
    const marker = index === defaultIndex ? "*" : " ";
    const canonical = formatCanonicalTarget({ workspaceSlug: target.workspaceSlug, projectSlug: target.projectSlug });
    console.log(` ${marker} [${index + 1}] ${canonical}  (${target.projectName})`);
  });

  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    const answer = await rl.question(`Choose project [${defaultIndex + 1}]: `);
    const trimmed = answer.trim();
    if (!trimmed.length) {
      return targets[defaultIndex] as ResolvedTarget;
    }

    const asIndex = Number.parseInt(trimmed, 10);
    if (Number.isFinite(asIndex) && asIndex >= 1 && asIndex <= targets.length) {
      return targets[asIndex - 1] as ResolvedTarget;
    }

    const byCanonical = targets.find((target) => {
      const canonical = formatCanonicalTarget({ workspaceSlug: target.workspaceSlug, projectSlug: target.projectSlug });
      return canonical === trimmed;
    });
    if (byCanonical) return byCanonical;

    throw new Error(`Unknown project "${trimmed}". Choose a list index or <workspaceSlug/projectSlug>.`);
  } finally {
    rl.close();
  }
}

async function promptForWorkspaceChoice(
  workspaces: WorkspaceSummary[],
  defaultWorkspace?: WorkspaceSummary,
): Promise<WorkspaceSummary> {
  if (!process.stdin.isTTY || !process.stdout.isTTY) {
    throw new Error("Workspace selection requires TTY input. Use `recallstack workspace use <workspaceSlug|workspaceId>` in non-interactive mode.");
  }

  if (workspaces.length === 0) {
    throw new Error("No accessible workspaces found for this account.");
  }

  const defaultIndex = Math.max(
    0,
    defaultWorkspace
      ? workspaces.findIndex((item) => (
        (defaultWorkspace.id && item.id === defaultWorkspace.id)
        || (defaultWorkspace.slug && item.slug === defaultWorkspace.slug)
      ))
      : 0,
  );

  console.log("Select workspace:");
  workspaces.forEach((workspace, index) => {
    const marker = index === defaultIndex ? "*" : " ";
    console.log(` ${marker} [${index + 1}] ${workspace.name} (${workspace.slug})`);
  });

  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    const answer = await rl.question(`Choose workspace [${defaultIndex + 1}]: `);
    const trimmed = answer.trim();
    if (!trimmed.length) {
      return workspaces[defaultIndex] as WorkspaceSummary;
    }

    const asIndex = Number.parseInt(trimmed, 10);
    if (Number.isFinite(asIndex) && asIndex >= 1 && asIndex <= workspaces.length) {
      return workspaces[asIndex - 1] as WorkspaceSummary;
    }

    const byToken = workspaces.find((item) => item.slug === trimmed || item.id === trimmed);
    if (byToken) return byToken;

    throw new Error(`Unknown workspace "${trimmed}". Choose a list index, slug, or id.`);
  } finally {
    rl.close();
  }
}

async function promptForLoginCode(): Promise<string> {
  if (!process.stdin.isTTY || !process.stdout.isTTY) {
    throw new Error("Missing sign-in code. Run `recallstack login <code>` in non-interactive mode.");
  }

  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    const answer = await rl.question("Enter one-time sign-in code: ");
    const normalized = answer.trim();
    if (!normalized.length) {
      throw new Error("Sign-in code cannot be empty.");
    }
    return normalized;
  } finally {
    rl.close();
  }
}

function installCodexIntegration(cwd: string, options: AgentInstallOptions = {}): JsonRecord {
  const scope = options.scope || "local";
  const codex = resolveCodexIntegrationPaths(cwd, scope);
  const quotedHookPath = shellQuote(codex.hookPath);
  const legacySessionLogHook = "./.codex/hooks/log-hook.sh SessionStart";
  const legacyStopLogHook = "./.codex/hooks/log-hook.sh Stop";
  writeInstructionFile(codex.skillPath, CODEX_SKILL_TEMPLATE);
  writeExecutableScript(codex.hookPath, CODEX_HOOK_TEMPLATE);

  const hooksConfig = readJsonFile<CodexHookConfig>(codex.hooksConfigPath, {});
  removeCodexEventHook(hooksConfig, "SessionStart", legacySessionLogHook);
  removeCodexEventHook(hooksConfig, "Stop", legacyStopLogHook);
  ensureCodexEventHook(hooksConfig, "SessionStart", `${quotedHookPath} session_start`, "startup|resume|clear");
  ensureCodexEventHook(hooksConfig, "Stop", `${quotedHookPath} stop`);
  writeJsonFile(codex.hooksConfigPath, ensureCodexHooksRoot(hooksConfig));

  const nextConfig = upsertTomlKey(readTextFile(codex.configPath), "features", "codex_hooks", "true");
  writeTextFile(codex.configPath, nextConfig);

  return {
    scope,
    skill_dir: codex.skillDir,
    skill_path: codex.skillPath,
    skill_spec: "agentskills.io",
    hook_dir: codex.hookDir,
    hook_path: codex.hookPath,
    hooks_config_path: codex.hooksConfigPath,
    config_path: codex.configPath,
    worker_model: normalizeValue(options.workerModel) || null,
  };
}

function ensureClaudeEventHook(
  hooks: JsonRecord,
  eventName: string,
  command: string,
  options: { async?: boolean; timeout?: number } = {},
): void {
  const existing = Array.isArray(hooks[eventName]) ? hooks[eventName] as unknown[] : [];
  const hasCommand = existing.some((entry) => {
    if (typeof entry !== "object" || entry === null) return false;
    const hookSet = (entry as JsonRecord).hooks;
    if (!Array.isArray(hookSet)) return false;
    return hookSet.some((hook) => {
      if (typeof hook !== "object" || hook === null) return false;
      const typed = hook as JsonRecord;
      return typed.type === "command" && typed.command === command;
    });
  });

  if (!hasCommand) {
    existing.push({
      hooks: [
        {
          type: "command",
          command,
          ...(typeof options.async === "boolean" ? { async: options.async } : {}),
          ...(typeof options.timeout === "number" ? { timeout: options.timeout } : {}),
        },
      ],
    });
  }

  hooks[eventName] = existing;
}

function installClaudeIntegration(cwd: string, options: AgentInstallOptions = {}): JsonRecord {
  const scope = options.scope || "local";
  const claude = resolveClaudeIntegrationPaths(cwd, scope);
  const hookPath = claude.hookPath;
  const legacyHookPath = claude.legacyHookPath;
  const relativeHookPath = ".claude/hooks/recallstack.mjs";
  const relativeLegacyHookPath = ".claude/hooks/recallstack.sh";
  const skillPath = claude.skillPath;
  writeExecutableScript(hookPath, CLAUDE_HOOK_TEMPLATE);
  writeInstructionFile(skillPath, CLAUDE_SKILL_TEMPLATE);
  removePathIfExists(legacyHookPath);

  const settingsPath = claude.settingsPath;
  const settings = readJsonFile<JsonRecord>(settingsPath, {});
  const hooks = typeof settings.hooks === "object" && settings.hooks !== null
    ? settings.hooks as JsonRecord
    : {};

  removeClaudeEventHook(hooks, "UserPromptSubmit", `${shellQuote(legacyHookPath)} user_prompt_submit`);
  removeClaudeEventHook(hooks, "Stop", `${shellQuote(legacyHookPath)} stop`);
  removeClaudeEventHook(hooks, "UserPromptSubmit", `${shellQuote(relativeLegacyHookPath)} user_prompt_submit`);
  removeClaudeEventHook(hooks, "Stop", `${shellQuote(relativeLegacyHookPath)} stop`);
  removeClaudeEventHook(hooks, "UserPromptSubmit", `${shellQuote(hookPath)} user_prompt_submit`);
  removeClaudeEventHook(hooks, "Stop", `${shellQuote(hookPath)} stop`);
  removeClaudeEventHook(hooks, "UserPromptSubmit", `${shellQuote(relativeHookPath)} user_prompt_submit`);
  removeClaudeEventHook(hooks, "Stop", `${shellQuote(relativeHookPath)} stop`);
  ensureClaudeEventHook(hooks, "Stop", `${shellQuote(hookPath)} stop`, { async: true, timeout: 180 });
  settings.hooks = hooks;
  writeJsonFile(settingsPath, settings);

  return {
    scope,
    hook_path: hookPath,
    settings_path: settingsPath,
    skill_path: skillPath,
    worker_model: normalizeValue(options.workerModel) || null,
  };
}

function installCursorIntegration(cwd: string, options: AgentInstallOptions = {}): JsonRecord {
  const scope = options.scope || "local";
  const cursor = resolveCursorIntegrationPaths(cwd, scope);
  const cursorRulePath = cursor.rulePath;
  const cursorHookPath = cursor.hookPath;
  const cursorHooksConfigPath = cursor.hooksConfigPath;
  const cursorHookCommand = `node ${shellQuote(cursorHookPath)}`;
  const legacyRelativeCommand = `node ./.cursor/hooks/${CURSOR_HOOK_FILENAME}`;
  writeInstructionFile(cursorRulePath, CURSOR_RULE_TEMPLATE);
  writeExecutableScript(cursorHookPath, CURSOR_HOOK_TEMPLATE);

  const hooksConfig = ensureCursorHooksRoot(readJsonFile<JsonRecord>(cursorHooksConfigPath, {}));
  const hooks = hooksConfig.hooks as JsonRecord;
  for (const eventName of ["beforeSubmitPrompt", "afterAgentResponse", "stop"]) {
    removeCursorEventHook(hooks, eventName, legacyRelativeCommand);
    removeCursorEventHook(hooks, eventName, cursorHookCommand);
    ensureCursorEventHook(hooks, eventName, cursorHookCommand);
  }
  hooksConfig.hooks = hooks;
  writeJsonFile(cursorHooksConfigPath, hooksConfig);

  return {
    scope,
    rule_path: cursorRulePath,
    hook_path: cursorHookPath,
    hooks_config_path: cursorHooksConfigPath,
    ide_only: true,
  };
}

function installCopilotIntegration(cwd: string, options: AgentInstallOptions = {}): JsonRecord {
  const scope = options.scope || "local";
  if (scope === "global") {
    const copilot = resolveCopilotGlobalIntegrationPaths();
    rmSync(copilot.legacySkillPath, { force: true });
    writeInstructionFile(copilot.skillPath, COPILOT_PLUGIN_SKILL_TEMPLATE);
    writeExecutableScript(copilot.hookPath, COPILOT_HOOK_TEMPLATE);
    writeJsonFile(copilot.pluginManifestPath, {
      name: "recallstack",
      version: CLI_PACKAGE_VERSION,
      description: "Recallstack memory retrieval guidance and hook automation.",
      skills: ["skills/"],
      hooks: "hooks.json",
    });
    writeJsonFile(copilot.hooksConfigPath, {
      version: 1,
      hooks: {
        sessionStart: [
          { type: "command", bash: `node ${shellQuote(copilot.hookPath)} sessionStart` },
        ],
        userPromptSubmitted: [
          { type: "command", bash: `node ${shellQuote(copilot.hookPath)} userPromptSubmitted` },
        ],
        sessionEnd: [
          { type: "command", bash: `node ${shellQuote(copilot.hookPath)} sessionEnd` },
        ],
        SessionStart: [
          { command: `node ${shellQuote(copilot.hookPath)} SessionStart` },
        ],
        UserPromptSubmit: [
          { command: `node ${shellQuote(copilot.hookPath)} UserPromptSubmit` },
        ],
        Stop: [
          { command: `node ${shellQuote(copilot.hookPath)} Stop` },
        ],
      },
    });

    const uninstallExisting = spawnSync("copilot", ["plugin", "uninstall", "recallstack", "--config-dir", copilot.configDir], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    });
    const installResult = spawnSync("copilot", ["plugin", "install", copilot.pluginSourceRoot, "--config-dir", copilot.configDir], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    });
    if (installResult.error || installResult.status !== 0) {
      throw new Error(`Failed to install global Copilot plugin: ${(installResult.stderr || installResult.stdout || installResult.error?.message || "unknown error").trim()}`);
    }

    return {
      scope,
      plugin_source_root: copilot.pluginSourceRoot,
      plugin_manifest_path: copilot.pluginManifestPath,
      skill_path: copilot.skillPath,
      hook_path: copilot.hookPath,
      hooks_config_path: copilot.hooksConfigPath,
      installed_plugin_path: copilot.installedPluginPath,
      config_dir: copilot.configDir,
      reinstall: uninstallExisting.status === 0,
      cli_supported: true,
      vscode_supported: true,
      worker_model: normalizeValue(options.workerModel) || null,
    };
  }

  const copilot = resolveCopilotLocalIntegrationPaths(cwd);
  writeInstructionFile(copilot.instructionsPath, COPILOT_INSTRUCTIONS_TEMPLATE);
  writeExecutableScript(copilot.hookPath, COPILOT_HOOK_TEMPLATE);
  writeJsonFile(copilot.cliHooksConfigPath, {
    version: 1,
    hooks: {
      sessionStart: [
        { type: "command", bash: `node ./.github/hooks/${COPILOT_HOOK_FILENAME} sessionStart` },
      ],
      userPromptSubmitted: [
        { type: "command", bash: `node ./.github/hooks/${COPILOT_HOOK_FILENAME} userPromptSubmitted` },
      ],
      sessionEnd: [
        { type: "command", bash: `node ./.github/hooks/${COPILOT_HOOK_FILENAME} sessionEnd` },
      ],
    },
  });
  writeJsonFile(copilot.vscodeHooksConfigPath, {
    version: 1,
    hooks: {
      SessionStart: [
        { command: `node ./.github/hooks/${COPILOT_HOOK_FILENAME} SessionStart` },
      ],
      UserPromptSubmit: [
        { command: `node ./.github/hooks/${COPILOT_HOOK_FILENAME} UserPromptSubmit` },
      ],
      Stop: [
        { command: `node ./.github/hooks/${COPILOT_HOOK_FILENAME} Stop` },
      ],
    },
  });

  return {
    scope,
    instructions_path: copilot.instructionsPath,
    hook_path: copilot.hookPath,
    cli_hooks_config_path: copilot.cliHooksConfigPath,
    vscode_hooks_config_path: copilot.vscodeHooksConfigPath,
    cli_supported: true,
    vscode_supported: true,
    worker_model: normalizeValue(options.workerModel) || null,
  };
}

function installAgentIntegration(target: AgentTarget, cwd: string, options: AgentInstallOptions = {}): JsonRecord {
  const out: JsonRecord = {};
  if (target === "codex" || target === "all") {
    out.codex = installCodexIntegration(cwd, options);
  }
  if (target === "claude" || target === "all") {
    out.claude = installClaudeIntegration(cwd, options);
  }
  if (target === "cursor" || target === "all") {
    out.cursor = installCursorIntegration(cwd, options);
  }
  if (target === "copilot" || target === "all") {
    out.copilot = installCopilotIntegration(cwd, options);
  }
  return out;
}

function uninstallCodexIntegration(cwd: string, scope: AgentInstallScope = "local"): JsonRecord {
  const codex = resolveCodexIntegrationPaths(cwd, scope);
  const quotedHookPath = shellQuote(codex.hookPath);
  const legacySessionLogHook = "./.codex/hooks/log-hook.sh SessionStart";
  const legacyStopLogHook = "./.codex/hooks/log-hook.sh Stop";

  const skillRemoved = removePathIfExists(codex.skillDir) || removePathIfExists(codex.skillPath);
  const hookRemoved = removePathIfExists(codex.hookPath);

  let hooksConfigUpdated = false;
  let hooksConfigRemoved = false;
  if (existsSync(codex.hooksConfigPath)) {
    const hooksConfig = readJsonFile<CodexHookConfig>(codex.hooksConfigPath, {});
    const sessionChanged = removeCodexEventHook(hooksConfig, "SessionStart", `${quotedHookPath} session_start`);
    const stopChanged = removeCodexEventHook(hooksConfig, "Stop", `${quotedHookPath} stop`);
    const legacySessionChanged = removeCodexEventHook(hooksConfig, "SessionStart", legacySessionLogHook);
    const legacyStopChanged = removeCodexEventHook(hooksConfig, "Stop", legacyStopLogHook);
    hooksConfigUpdated = sessionChanged || stopChanged || legacySessionChanged || legacyStopChanged;
    if (hooksConfigUpdated) {
      if (codexHooksConfigIsEmpty(hooksConfig)) {
        removePathIfExists(codex.hooksConfigPath);
        hooksConfigRemoved = true;
      } else {
        writeJsonFile(codex.hooksConfigPath, ensureCodexHooksRoot(hooksConfig));
      }
    }
  }

  let configUpdated = false;
  let configRemoved = false;
  if (existsSync(codex.configPath)) {
    const nextConfig = removeTomlKey(readTextFile(codex.configPath), "features", "codex_hooks");
    configUpdated = nextConfig !== readTextFile(codex.configPath);
    if (configUpdated) {
      if (nextConfig.trim().length === 0) {
        removePathIfExists(codex.configPath);
        configRemoved = true;
      } else {
        writeTextFile(codex.configPath, nextConfig);
      }
    }
  }

  pruneEmptyDirectories(
    [codex.hookDir, dirname(codex.skillDir), dirname(dirname(codex.skillDir)), dirname(codex.hooksConfigPath)],
    scope === "global" ? homedir() : cwd,
  );
  return {
    scope,
    skill_dir: codex.skillDir,
    skill_path: codex.skillPath,
    skill_removed: skillRemoved,
    hook_path: codex.hookPath,
    hook_removed: hookRemoved,
    hooks_config_path: codex.hooksConfigPath,
    hooks_config_updated: hooksConfigUpdated,
    hooks_config_removed: hooksConfigRemoved,
    config_path: codex.configPath,
    config_updated: configUpdated,
    config_removed: configRemoved,
  };
}

function removeClaudeEventHook(hooks: JsonRecord, eventName: string, command: string): boolean {
  const existing = Array.isArray(hooks[eventName]) ? hooks[eventName] as unknown[] : [];
  let changed = false;
  const nextEntries = existing.flatMap((entry) => {
    if (typeof entry !== "object" || entry === null) return [entry];
    const typedEntry = entry as JsonRecord;
    const hookSet = Array.isArray(typedEntry.hooks) ? typedEntry.hooks as unknown[] : [];
    if (!hookSet.length) return [entry];

    const nextHooks = hookSet.filter((hook) => {
      if (typeof hook !== "object" || hook === null) return true;
      const typedHook = hook as JsonRecord;
      const matches = typedHook.type === "command" && typedHook.command === command;
      if (matches) changed = true;
      return !matches;
    });

    if (!nextHooks.length) {
      changed = true;
      return [];
    }

    return [{
      ...typedEntry,
      hooks: nextHooks,
    }];
  });

  if (nextEntries.length) {
    hooks[eventName] = nextEntries;
  } else if (existing.length) {
    delete hooks[eventName];
    changed = true;
  }

  return changed;
}

function ensureCursorHooksRoot(raw: JsonRecord): JsonRecord {
  const hooks = typeof raw.hooks === "object" && raw.hooks !== null
    ? raw.hooks as JsonRecord
    : {};
  return {
    version: 1,
    ...raw,
    hooks,
  };
}

function ensureCursorEventHook(hooks: JsonRecord, eventName: string, command: string): void {
  const existing = Array.isArray(hooks[eventName]) ? hooks[eventName] as unknown[] : [];
  const hasCommand = existing.some((entry) => typeof entry === "object" && entry !== null && (entry as JsonRecord).command === command);
  if (!hasCommand) {
    existing.push({ command });
  }
  hooks[eventName] = existing;
}

function removeCursorEventHook(hooks: JsonRecord, eventName: string, command: string): boolean {
  const existing = Array.isArray(hooks[eventName]) ? hooks[eventName] as unknown[] : [];
  const nextEntries = existing.filter((entry) => !(typeof entry === "object" && entry !== null && (entry as JsonRecord).command === command));
  const changed = nextEntries.length !== existing.length;
  if (nextEntries.length) {
    hooks[eventName] = nextEntries;
  } else if (existing.length) {
    delete hooks[eventName];
  }
  return changed;
}

function uninstallClaudeIntegration(cwd: string, scope: AgentInstallScope = "local"): JsonRecord {
  const claude = resolveClaudeIntegrationPaths(cwd, scope);
  const hookPath = claude.hookPath;
  const legacyHookPath = claude.legacyHookPath;
  const relativeHookPath = ".claude/hooks/recallstack.mjs";
  const relativeLegacyHookPath = ".claude/hooks/recallstack.sh";
  const skillPath = claude.skillPath;
  const settingsPath = claude.settingsPath;
  const quotedHookPath = shellQuote(hookPath);
  const quotedLegacyHookPath = shellQuote(legacyHookPath);
  const quotedRelativeHookPath = shellQuote(relativeHookPath);
  const quotedRelativeLegacyHookPath = shellQuote(relativeLegacyHookPath);

  const hookRemoved = removePathIfExists(hookPath) || removePathIfExists(legacyHookPath);
  const skillRemoved = removePathIfExists(skillPath);

  let settingsUpdated = false;
  let settingsRemoved = false;
  if (existsSync(settingsPath)) {
    const settings = readJsonFile<JsonRecord>(settingsPath, {});
    const hooks = typeof settings.hooks === "object" && settings.hooks !== null
      ? settings.hooks as JsonRecord
      : undefined;

    if (hooks) {
      const submitChanged = removeClaudeEventHook(hooks, "UserPromptSubmit", `${quotedHookPath} user_prompt_submit`);
      const stopChanged = removeClaudeEventHook(hooks, "Stop", `${quotedHookPath} stop`);
      const legacySubmitChanged = removeClaudeEventHook(hooks, "UserPromptSubmit", `${quotedLegacyHookPath} user_prompt_submit`);
      const legacyStopChanged = removeClaudeEventHook(hooks, "Stop", `${quotedLegacyHookPath} stop`);
      const relativeSubmitChanged = removeClaudeEventHook(hooks, "UserPromptSubmit", `${quotedRelativeHookPath} user_prompt_submit`);
      const relativeStopChanged = removeClaudeEventHook(hooks, "Stop", `${quotedRelativeHookPath} stop`);
      const relativeLegacySubmitChanged = removeClaudeEventHook(
        hooks,
        "UserPromptSubmit",
        `${quotedRelativeLegacyHookPath} user_prompt_submit`,
      );
      const relativeLegacyStopChanged = removeClaudeEventHook(hooks, "Stop", `${quotedRelativeLegacyHookPath} stop`);
      settingsUpdated =
        submitChanged ||
        stopChanged ||
        legacySubmitChanged ||
        legacyStopChanged ||
        relativeSubmitChanged ||
        relativeStopChanged ||
        relativeLegacySubmitChanged ||
        relativeLegacyStopChanged;

      if (Object.keys(hooks).length === 0) {
        delete settings.hooks;
      } else {
        settings.hooks = hooks;
      }
    }

    if (settingsUpdated) {
      if (Object.keys(settings).length === 0) {
        removePathIfExists(settingsPath);
        settingsRemoved = true;
      } else {
        writeJsonFile(settingsPath, settings);
      }
    }
  }

  pruneEmptyDirectories([
    dirname(hookPath),
    dirname(skillPath),
    dirname(dirname(hookPath)),
  ], scope === "global" ? homedir() : cwd);

  return {
    scope,
    hook_path: hookPath,
    hook_removed: hookRemoved,
    settings_path: settingsPath,
    settings_updated: settingsUpdated,
    settings_removed: settingsRemoved,
    skill_path: skillPath,
    skill_removed: skillRemoved,
  };
}

function uninstallCursorIntegration(cwd: string, scope: AgentInstallScope = "local"): JsonRecord {
  const cursor = resolveCursorIntegrationPaths(cwd, scope);
  const cursorRulePath = cursor.rulePath;
  const cursorHookPath = cursor.hookPath;
  const cursorHooksConfigPath = cursor.hooksConfigPath;
  const cursorHookCommand = `node ${shellQuote(cursorHookPath)}`;
  const legacyRelativeCommand = `node ./.cursor/hooks/${CURSOR_HOOK_FILENAME}`;
  const ruleRemoved = removePathIfExists(cursorRulePath);
  const hookRemoved = removePathIfExists(cursorHookPath);

  let hooksConfigUpdated = false;
  let hooksConfigRemoved = false;
  if (existsSync(cursorHooksConfigPath)) {
    const hooksConfig = ensureCursorHooksRoot(readJsonFile<JsonRecord>(cursorHooksConfigPath, {}));
    const hooks = hooksConfig.hooks as JsonRecord;
    const beforeChanged =
      removeCursorEventHook(hooks, "beforeSubmitPrompt", cursorHookCommand)
      || removeCursorEventHook(hooks, "beforeSubmitPrompt", legacyRelativeCommand);
    const afterChanged =
      removeCursorEventHook(hooks, "afterAgentResponse", cursorHookCommand)
      || removeCursorEventHook(hooks, "afterAgentResponse", legacyRelativeCommand);
    const stopChanged =
      removeCursorEventHook(hooks, "stop", cursorHookCommand)
      || removeCursorEventHook(hooks, "stop", legacyRelativeCommand);
    hooksConfigUpdated = beforeChanged || afterChanged || stopChanged;
    if (hooksConfigUpdated) {
      hooksConfig.hooks = hooks;
      if (Object.keys(hooks).length === 0) {
        removePathIfExists(cursorHooksConfigPath);
        hooksConfigRemoved = true;
      } else {
        writeJsonFile(cursorHooksConfigPath, hooksConfig);
      }
    }
  }

  pruneEmptyDirectories([
    dirname(cursorRulePath),
    dirname(cursorHookPath),
    dirname(dirname(cursorRulePath)),
  ], scope === "global" ? homedir() : cwd);

  return {
    scope,
    rule_path: cursorRulePath,
    rule_removed: ruleRemoved,
    hook_path: cursorHookPath,
    hook_removed: hookRemoved,
    hooks_config_path: cursorHooksConfigPath,
    hooks_config_updated: hooksConfigUpdated,
    hooks_config_removed: hooksConfigRemoved,
    ide_only: true,
  };
}

function uninstallCopilotIntegration(cwd: string, scope: AgentInstallScope = "local"): JsonRecord {
  if (scope === "global") {
    const copilot = resolveCopilotGlobalIntegrationPaths();
    const uninstallResult = spawnSync("copilot", ["plugin", "uninstall", "recallstack", "--config-dir", copilot.configDir], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    });
    return {
      scope,
      plugin_source_root: copilot.pluginSourceRoot,
      plugin_source_removed: removePathIfExists(copilot.pluginSourceRoot),
      installed_plugin_path: copilot.installedPluginPath,
      uninstall_status: uninstallResult.status,
      uninstall_stdout: truncate((uninstallResult.stdout || "").trim(), 400),
      uninstall_stderr: truncate((uninstallResult.stderr || "").trim(), 400),
      cli_supported: true,
      vscode_supported: true,
    };
  }

  const copilot = resolveCopilotLocalIntegrationPaths(cwd);
  const instructionsRemoved = removePathIfExists(copilot.instructionsPath);
  const hookRemoved = removePathIfExists(copilot.hookPath);
  const cliHooksRemoved = removePathIfExists(copilot.cliHooksConfigPath);
  const vscodeHooksRemoved = removePathIfExists(copilot.vscodeHooksConfigPath);

  pruneEmptyDirectories([
    copilot.hookDir,
    dirname(copilot.instructionsPath),
  ], cwd);

  return {
    scope,
    instructions_path: copilot.instructionsPath,
    instructions_removed: instructionsRemoved,
    hook_path: copilot.hookPath,
    hook_removed: hookRemoved,
    cli_hooks_config_path: copilot.cliHooksConfigPath,
    cli_hooks_removed: cliHooksRemoved,
    vscode_hooks_config_path: copilot.vscodeHooksConfigPath,
    vscode_hooks_removed: vscodeHooksRemoved,
    cli_supported: true,
    vscode_supported: true,
  };
}

function purgeProjectRecallstackState(cwd: string): JsonRecord {
  const projectStateRoot = join(cwd, ".recallstack");
  const workspaceConfigPath = getWorkspaceConfigPath(cwd);
  const projectConfigPath = join(cwd, ".recallstack", "config.json");
  const turnsPath = join(cwd, ".recallstack", "turns");
  const agentTurnsPath = join(cwd, ".recallstack", "agent-turns");

  const workspaceConfigRemoved = removePathIfExists(workspaceConfigPath);
  const projectConfigRemoved = removePathIfExists(projectConfigPath);
  const turnsRemoved = removePathIfExists(turnsPath);
  const agentTurnsRemoved = removePathIfExists(agentTurnsPath);
  const projectStateRootRemoved = removePathIfExists(projectStateRoot);

  return {
    project_state_root: projectStateRoot,
    project_state_root_removed: projectStateRootRemoved,
    workspace_config_path: workspaceConfigPath,
    workspace_config_removed: workspaceConfigRemoved,
    project_config_path: projectConfigPath,
    project_config_removed: projectConfigRemoved,
    turns_path: turnsPath,
    turns_removed: turnsRemoved,
    agent_turns_path: agentTurnsPath,
    agent_turns_removed: agentTurnsRemoved,
  };
}

function uninstallAgentIntegration(
  target: AgentTarget,
  cwd: string,
  options: { purgeProjectState?: boolean; scope?: AgentInstallScope } = {},
): JsonRecord {
  const scope = options.scope || "local";
  const out: JsonRecord = {};
  if (target === "codex" || target === "all") {
    out.codex = uninstallCodexIntegration(cwd, scope);
  }
  if (target === "claude" || target === "all") {
    out.claude = uninstallClaudeIntegration(cwd, scope);
  }
  if (target === "cursor" || target === "all") {
    out.cursor = uninstallCursorIntegration(cwd, scope);
  }
  if (target === "copilot" || target === "all") {
    out.copilot = uninstallCopilotIntegration(cwd, scope);
  }
  if (options.purgeProjectState && scope === "local") {
    out.project_state = purgeProjectRecallstackState(cwd);
  }
  return out;
}

const program = new Command();

function getRuntimeConfig(): ReturnType<typeof loadConfig> {
  return loadConfig();
}

function getRuntimeBaseUrl(config: ReturnType<typeof loadConfig>): string {
  return getEffectiveBaseUrl(config);
}

function serializeWorkspaceSummary(workspace: WorkspaceSummary | undefined): JsonRecord | null {
  if (!workspace) return null;
  return {
    id: workspace.id || null,
    slug: workspace.slug || null,
    name: workspace.name || null,
  };
}

async function tryResolveConfiguredWorkspace(config: ReturnType<typeof loadConfig>): Promise<{
  workspace?: WorkspaceSummary;
  error?: string;
}> {
  try {
    return {
      workspace: await resolveConfiguredWorkspace(config),
    };
  } catch (error) {
    return {
      error: error instanceof Error ? error.message : String(error),
    };
  }
}

async function showConfigCommand(): Promise<void> {
  const cfg = getRuntimeConfig();
  const resolved = resolveConfig();
  const hasOverride = Boolean(cfg.baseUrl?.trim());
  const configuredWorkspace = getConfiguredWorkspace(cfg);
  const activeWorkspace = await tryResolveConfiguredWorkspace(cfg);
  const currentAuthConfig = getRuntimeConfig();
  const response: JsonRecord = {
    config_path: resolved.path,
    config_scope: resolved.scope,
    scope_root: resolved.scopeRoot,
    base_url_override: hasOverride ? cfg.baseUrl : null,
    default_base_url: DEFAULT_BASE_URL,
    effective_base_url: getRuntimeBaseUrl(cfg),
    auth_profile_key: getRuntimeBaseUrl(cfg),
    authenticated: Boolean(currentAuthConfig.accessToken || currentAuthConfig.apiKey),
    auth_status: summarizeAuthState(currentAuthConfig),
    override_source: hasOverride ? "config.baseUrl" : "default",
    active_workspace: serializeWorkspaceSummary(activeWorkspace.workspace),
    configured_workspace: serializeWorkspaceSummary(configuredWorkspace),
  };

  if (activeWorkspace.error) {
    response.active_workspace_error = activeWorkspace.error;
  }

  console.log(JSON.stringify(response, null, 2));
}

async function showWorkspaceCommand(): Promise<void> {
  const cfg = getRuntimeConfig();
  const configured = getConfiguredWorkspace(cfg);
  const active = await tryResolveConfiguredWorkspace(cfg);
  const response: JsonRecord = {
    workspace: serializeWorkspaceSummary(active.workspace),
    configured_workspace: serializeWorkspaceSummary(configured),
  };

  if (active.error) {
    response.workspace_error = active.error;
  }

  console.log(JSON.stringify(response, null, 2));
}

async function showProjectCommand(options: { cwd: string }): Promise<void> {
  const cfg = getRuntimeConfig();
  const workspaceConfigPath = findWorkspaceConfig(options.cwd).path || getWorkspaceConfigPath(options.cwd);
  const selectedTarget = await loadLocalTarget(options.cwd, cfg);

  console.log(JSON.stringify({
    cwd: options.cwd,
    workspace_config_path: workspaceConfigPath,
    project: selectedTarget
      ? formatCanonicalTarget({ workspaceSlug: selectedTarget.workspaceSlug, projectSlug: selectedTarget.projectSlug })
      : null,
    target: selectedTarget
      ? formatCanonicalTarget({ workspaceSlug: selectedTarget.workspaceSlug, projectSlug: selectedTarget.projectSlug })
      : null,
    workspace_id: selectedTarget?.workspaceId || null,
    workspace_slug: selectedTarget?.workspaceSlug || null,
    project_id: selectedTarget?.projectId || null,
    project_slug: selectedTarget?.projectSlug || null,
  }, null, 2));
}

async function setTargetCommand(targetArg: string | undefined, options: { cwd: string }): Promise<void> {
  const cfg = getRuntimeConfig();
  let previousTarget: ResolvedTarget | undefined;
  try {
    previousTarget = await loadLocalTarget(options.cwd, cfg);
  } catch (error) {
    if (error instanceof Error) {
      console.warn(`${error.message} Continuing without existing default project.`);
    }
  }

  let selected: ResolvedTarget;
  if (targetArg) {
    selected = await resolveTargetByInputValue(cfg, targetArg, options.cwd, {
      preferConfiguredWorkspace: true,
    });
    if (selected.mode === "GLOBAL") {
      throw new Error("GLOBAL_WRITE_FORBIDDEN: raw memory writes cannot target global. Select a non-global project.");
    }
  } else {
    const configuredWorkspace = await resolveConfiguredWorkspace(cfg);
    const workspaceContext = configuredWorkspace ?? await resolveWorkspaceContextForCommand(cfg, options.cwd);
    const writableTargets = workspaceContext
      ? await listWritableTargetsForWorkspace(cfg, workspaceContext)
      : await listWritableTargets(cfg);
    if (workspaceContext && writableTargets.length === 0) {
      throw new Error(`No writable projects found in workspace "${workspaceContext.slug}". Choose another workspace with \`recallstack workspace use\`.`);
    }
    selected = await promptForTargetChoice(writableTargets, previousTarget);
  }

  const workspaceConfigPath = saveWorkspaceTarget(options.cwd, selected);

  console.log(JSON.stringify({
    cwd: options.cwd,
    workspace_config_path: workspaceConfigPath,
    project: formatCanonicalTarget({ workspaceSlug: selected.workspaceSlug, projectSlug: selected.projectSlug }),
    target: formatCanonicalTarget({ workspaceSlug: selected.workspaceSlug, projectSlug: selected.projectSlug }),
    workspace_id: selected.workspaceId,
    workspace_slug: selected.workspaceSlug,
    project_id: selected.projectId,
    project_slug: selected.projectSlug,
    previous_project: previousTarget
      ? formatCanonicalTarget({ workspaceSlug: previousTarget.workspaceSlug, projectSlug: previousTarget.projectSlug })
      : null,
    previous_target: previousTarget
      ? formatCanonicalTarget({ workspaceSlug: previousTarget.workspaceSlug, projectSlug: previousTarget.projectSlug })
      : null,
  }, null, 2));
}

program
  .name("recallstack")
  .description("Recallstack CLI")
  .version(CLI_PACKAGE_VERSION);

program
  .command("login")
  .description("Sign in using a one-time code from Settings")
  .argument("[code]", "One-time sign-in code from Settings")
  .action(async (codeArg) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    return withCliSpan(cfg, {
      name: "cli.login",
      attributes: {
        "cli.command": "login",
      },
    }, async () => {
      const code = typeof codeArg === "string" && codeArg.trim().length > 0
        ? codeArg.trim()
        : await promptForLoginCode();
      try {
        await loginWithCode(code, cfg);
        const updatedCfg = getRuntimeConfig();
        await flushQueuedWritesAfterLogin(updatedCfg);
        await emitCliTelemetry(updatedCfg, {
          eventName: "cli.auth.login",
          operation: "cli.login",
          durationMs: Date.now() - startedAt,
        });
        console.log(`Login successful for ${getRuntimeBaseUrl(updatedCfg)}. Global auth config: ${getConfigPath({ scope: "global" })}`);
      } catch (error) {
        await emitCliTelemetry(cfg, {
          eventName: "cli.auth.login",
          operation: "cli.login",
          outcome: "error",
          durationMs: Date.now() - startedAt,
          attributes: {
            error_code: telemetryErrorCode(error),
          },
        });
        throw error;
      }
    });
  });

program
  .command("logout")
  .description("Revoke the current session (best effort) and clear local auth tokens")
  .action(async () => {
    const cfg = getRuntimeConfig();
    const out = await logout(cfg);
    if (out.warning) {
      console.warn(out.warning);
    }
    if (out.remoteRevoked) {
      console.log("Logged out. Remote session revoked.");
      return;
    }
    console.log("Logged out.");
  });

program
  .command("whoami")
  .description("Show the authenticated account and workspace context")
  .action(async () => {
    const cfg = getRuntimeConfig();
    const out = await authenticatedHttp<any>("/v1/auth/me", {}, cfg);
    console.log(JSON.stringify(out, null, 2));
  });

const configCommand = program
  .command("config")
  .description("Manage Recallstack CLI endpoint settings");

configCommand
  .command("where")
  .description("Print the resolved config path for the current directory scope")
  .action(() => {
    const resolved = resolveConfig();
    console.log(resolved.path);
  });

configCommand
  .command("show")
  .description("Show resolved endpoint, auth state, and workspace configuration")
  .action(async () => {
    await showConfigCommand();
  });

configCommand
  .command("base-url")
  .description("Set or clear an explicit API base URL override for the current directory scope")
  .argument("[url]", "Explicit API base URL override")
  .option("--clear", "Clear the explicit base URL override")
  .action((url, options: { clear?: boolean }) => {
    if (options.clear && url) {
      throw new Error("Provide either a URL or --clear, not both.");
    }
    if (!options.clear && !url) {
      throw new Error("Provide a URL or use --clear.");
    }

    const nextBaseUrl = options.clear ? undefined : url;

    saveBaseUrlOverride(nextBaseUrl);

    const updated = loadConfig();
    console.log(JSON.stringify({
      base_url_override: updated.baseUrl ?? null,
      default_base_url: DEFAULT_BASE_URL,
      effective_base_url: getRuntimeBaseUrl(updated),
      auth_profile_key: getRuntimeBaseUrl(updated),
      override_source: updated.baseUrl ? "config.baseUrl" : "default",
    }, null, 2));
  });

const project = program
  .command("project")
  .description("Manage local default project target and project resources");

const workspace = program
  .command("workspace")
  .description("Inspect accessible workspaces");

workspace
  .command("list")
  .description("List workspaces you can access")
  .action(async () => {
    const cfg = getRuntimeConfig();
    const configured = getConfiguredWorkspace(cfg);
    const items = (await listWorkspaceMemberships(cfg)).map((item) => ({
      ...item,
      isConfigured: Boolean(
        (configured?.id && configured.id === item.id)
        || (configured?.slug && configured.slug === item.slug),
      ),
    }));
    console.log(JSON.stringify({ items }, null, 2));
  });

workspace
  .command("use")
  .description("Set active workspace preference for CLI commands")
  .argument("[workspace]", "Workspace slug or id")
  .action(async (workspaceToken: string | undefined) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    const previous = getConfiguredWorkspace(cfg);
    return withCliSpan(cfg, {
      name: "cli.workspace.use",
      attributes: {
        "cli.command": "workspace use",
        workspace_token: workspaceToken || null,
      },
    }, async () => {
      let selected: WorkspaceSummary;
      try {
        if (workspaceToken) {
          selected = await resolveWorkspaceByToken(cfg, workspaceToken);
        } else {
          const memberships = await listWorkspaceMemberships(cfg);
          const options = memberships.map((item) => ({
            id: item.id,
            slug: item.slug,
            name: item.name,
          }));
          const fallbackDefault = memberships.find((item) => item.isActive);
          selected = await promptForWorkspaceChoice(
            options,
            previous || (fallbackDefault ? { id: fallbackDefault.id, slug: fallbackDefault.slug, name: fallbackDefault.name } : undefined),
          );
        }

        updateProfileForBaseUrl(getRuntimeBaseUrl(cfg), {
          activeWorkspaceId: selected.id,
          activeWorkspaceSlug: selected.slug,
          activeWorkspaceName: selected.name,
        });
        await emitCliTelemetry(cfg, {
          eventName: "cli.workspace.use",
          operation: "cli.workspace.use",
          durationMs: Date.now() - startedAt,
          attributes: {
            workspace_id: selected.id,
            workspace_slug: selected.slug,
          },
        });

        console.log(JSON.stringify({
          workspace: {
            id: selected.id,
            slug: selected.slug,
            name: selected.name,
          },
          previous_workspace: previous ? {
            id: previous.id || null,
            slug: previous.slug || null,
            name: previous.name || null,
          } : null,
        }, null, 2));
      } catch (error) {
        await emitCliTelemetry(cfg, {
          eventName: "cli.workspace.use",
          operation: "cli.workspace.use",
          outcome: "error",
          durationMs: Date.now() - startedAt,
          attributes: {
            error_code: telemetryErrorCode(error),
          },
        });
        throw error;
      }
    });
  });

workspace
  .command("show")
  .description("Show configured and currently active workspace context")
  .action(async () => {
    await showWorkspaceCommand();
  });

workspace
  .command("clear")
  .description("Clear configured workspace preference from local config")
  .action(() => {
    const cfg = getRuntimeConfig();
    const configured = getConfiguredWorkspace(cfg);
    updateProfileForBaseUrl(getRuntimeBaseUrl(cfg), {
      activeWorkspaceId: undefined,
      activeWorkspaceSlug: undefined,
      activeWorkspaceName: undefined,
    });
    console.log(JSON.stringify({
      cleared: true,
      previous_workspace: configured ? {
        id: configured.id || null,
        slug: configured.slug || null,
        name: configured.name || null,
      } : null,
    }, null, 2));
  });

project
  .command("show")
  .description("Show resolved project target for this directory")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .action(async (options: { cwd: string }) => {
    await showProjectCommand(options);
  });

project
  .command("where")
  .description("Print workspace target file path for this directory")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .action((options: { cwd: string }) => {
    const workspaceConfigPath = findWorkspaceConfig(options.cwd).path || getWorkspaceConfigPath(options.cwd);
    console.log(workspaceConfigPath);
  });

project
  .command("clear")
  .description("Clear the saved local project target for this directory")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .action((options: { cwd: string }) => {
    const workspaceConfigPath = clearWorkspaceTarget(options.cwd);
    console.log(JSON.stringify({
      cwd: options.cwd,
      workspace_config_path: workspaceConfigPath,
      cleared: true,
    }, null, 2));
  });

project
  .command("create")
  .description("Create a project in the selected or specified workspace")
  .requiredOption("--name <name>")
  .option("--description <description>")
  .option("--workspace <workspace>", "Workspace slug or id")
  .action(async (options) => {
    const cfg = getRuntimeConfig();

    let workspaceId: string | undefined;
    if (options.workspace) {
      workspaceId = (await resolveWorkspaceByToken(cfg, String(options.workspace))).id;
    } else {
      const workspaceContext = await resolveWorkspaceContextForCommand(cfg, process.cwd());
      workspaceId = workspaceContext?.id;
    }

    if (!workspaceId) {
      throw new Error("Workspace context missing. Run `recallstack workspace use <workspaceSlug|workspaceId>` (or `recallstack project use`) first, or pass `--workspace <workspaceSlug|workspaceId>`.");
    }

    const out = await authenticatedHttp<any>("/v1/projects", {
      method: "POST",
      body: { name: options.name, description: options.description },
      workspaceId,
    }, cfg);
    console.log(JSON.stringify(out, null, 2));
  });

project
  .command("list")
  .description("List accessible projects, optionally scoped to a workspace")
  .option("--workspace <workspace>", "Workspace slug or id")
  .action(async (options: { workspace?: string }) => {
    const cfg = getRuntimeConfig();

    let workspaces: WorkspaceSummary[];
    if (options.workspace) {
      workspaces = [await resolveWorkspaceByToken(cfg, options.workspace)];
    } else {
      const workspaceContext = await resolveWorkspaceContextForCommand(cfg, process.cwd());
      workspaces = workspaceContext ? [workspaceContext] : await listWorkspaces(cfg);
    }

    const rows: Array<{
      workspace_id: string;
      workspace_slug: string;
      workspace_name: string;
      id: string;
      slug: string;
      name: string;
      mode: "GLOBAL" | "PROJECT";
      target: string;
    }> = [];

    for (const workspace of workspaces) {
      const projects = await listProjectsForWorkspace(cfg, workspace.id);
      for (const item of projects) {
        rows.push({
          workspace_id: workspace.id,
          workspace_slug: workspace.slug,
          workspace_name: workspace.name,
          id: item.id,
          slug: item.slug,
          name: item.name,
          mode: item.mode,
          target: formatCanonicalTarget({ workspaceSlug: workspace.slug, projectSlug: item.slug }),
        });
      }
    }

    console.log(JSON.stringify({ items: rows }, null, 2));
  });

project
  .command("use")
  .description("Set default local project target (interactive when omitted)")
  .argument("[project]", "projectSlug or workspaceSlug/projectSlug")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .action(async (projectArg, options: { cwd: string }) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    return withCliSpan(cfg, {
      name: "cli.project.use",
      attributes: {
        "cli.command": "project use",
        project_token: typeof projectArg === "string" ? projectArg : null,
      },
    }, async () => {
      try {
        await setTargetCommand(projectArg, options);
        const selectedTarget = await loadLocalTarget(options.cwd, cfg);
        await emitCliTelemetry(cfg, {
          eventName: "cli.project.use",
          operation: "cli.project.use",
          durationMs: Date.now() - startedAt,
          projectId: selectedTarget?.projectSlug,
          attributes: {
            workspace_slug: selectedTarget?.workspaceSlug || null,
            project_slug: selectedTarget?.projectSlug || null,
          },
        });
      } catch (error) {
        await emitCliTelemetry(cfg, {
          eventName: "cli.project.use",
          operation: "cli.project.use",
          outcome: "error",
          durationMs: Date.now() - startedAt,
          attributes: {
            error_code: telemetryErrorCode(error),
          },
        });
        throw error;
      }
    });
  });

project
  .command("set")
  .description("Alias of `project use`")
  .argument("[project]", "projectSlug or workspaceSlug/projectSlug")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .action(async (projectArg, options: { cwd: string }) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    return withCliSpan(cfg, {
      name: "cli.project.use",
      attributes: {
        "cli.command": "project set",
        project_token: typeof projectArg === "string" ? projectArg : null,
      },
    }, async () => {
      try {
        await setTargetCommand(projectArg, options);
        const selectedTarget = await loadLocalTarget(options.cwd, cfg);
        await emitCliTelemetry(cfg, {
          eventName: "cli.project.use",
          operation: "cli.project.use",
          durationMs: Date.now() - startedAt,
          projectId: selectedTarget?.projectSlug,
          attributes: {
            workspace_slug: selectedTarget?.workspaceSlug || null,
            project_slug: selectedTarget?.projectSlug || null,
          },
        });
      } catch (error) {
        await emitCliTelemetry(cfg, {
          eventName: "cli.project.use",
          operation: "cli.project.use",
          outcome: "error",
          durationMs: Date.now() - startedAt,
          attributes: {
            error_code: telemetryErrorCode(error),
          },
        });
        throw error;
      }
    });
  });

const memory = program.command("memory").description("Ingest and retrieve memories for the current target project");

memory
  .command("ingest")
  .description("Create a memory event from file or stdin in the selected project (turn summaries and change logs)")
  .option("--project <project>", "projectSlug or workspaceSlug/projectSlug")
  .option("--file <path>")
  .option("--stdin", "Read from stdin")
  .option("--idempotency-key <key>", "Custom idempotency key")
  .option("--metadata <json>", "JSON metadata payload")
  .option("--force-event", "Override the durable-context recommendation for document-like payloads")
  .option("--turn-id <id>", "Turn identifier for proof-aware ingestion; auto-attaches diff proof unless disabled")
  .option("--disable-diff-proof", "Disable auto-generated diff proof for --turn-id ingest")
  .option("--diff-proof-mode <mode>", "advisory|strict for auto-generated proof", "advisory")
  .option("--diff-proof <json>", "Optional full diff_proof JSON payload")
  .action(async (options) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    return withCliSpan(cfg, {
      name: "cli.memory.ingest",
      attributes: {
        "cli.command": "memory ingest",
      },
    }, async () => {
    const selectedTarget = await resolveTargetForCommand({
      config: cfg,
      cwd: process.cwd(),
      explicit: options.project,
      requireWritable: true,
    });

    let metadata: JsonRecord | undefined;
    let diffProof: JsonRecord | undefined;

    assertExactlyOneInputSource(options, "memory ingest");

    const hasDisableDiffProofFlag = cliHasFlag("--disable-diff-proof");
    const hasDiffProofModeFlag = cliHasFlag("--diff-proof-mode");
    assertValidDiffProofMode(options.diffProofMode);

    if (hasDisableDiffProofFlag && !options.turnId) {
      throw new Error("--disable-diff-proof requires --turn-id.");
    }
    if (hasDisableDiffProofFlag && hasDiffProofModeFlag) {
      throw new Error("Cannot combine --disable-diff-proof with --diff-proof-mode.");
    }
    if (hasDisableDiffProofFlag && options.diffProof) {
      throw new Error("Cannot combine --disable-diff-proof with --diff-proof.");
    }
    if (hasDiffProofModeFlag && options.diffProof) {
      throw new Error("Cannot combine --diff-proof-mode with --diff-proof; include mode inside the --diff-proof JSON payload.");
    }
    if (hasDiffProofModeFlag && !options.turnId && !options.diffProof) {
      throw new Error("--diff-proof-mode requires --turn-id or --diff-proof.");
    }

    let content = "";
    if (options.stdin) {
      content = await new Promise<string>((resolvePromise) => {
        let out = "";
        process.stdin.setEncoding("utf8");
        process.stdin.on("data", (chunk) => {
          out += chunk;
        });
        process.stdin.on("end", () => resolvePromise(out));
      });
    } else if (options.file) {
      content = readFileSync(options.file, "utf8");
    }

    if (options.metadata) {
      try {
        const parsed = JSON.parse(options.metadata) as unknown;
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("Metadata must be a JSON object.");
        }
        metadata = parsed as JsonRecord;
      } catch (error) {
        throw new Error(`Invalid --metadata JSON: ${error instanceof Error ? error.message : String(error)}`);
      }
    }

    const enableDiffProof = Boolean(options.turnId) && !hasDisableDiffProofFlag;

    if (options.diffProof) {
      try {
        const parsed = JSON.parse(options.diffProof) as unknown;
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("Diff proof must be a JSON object.");
        }
        diffProof = parsed as JsonRecord;
      } catch (error) {
        throw new Error(`Invalid --diff-proof JSON: ${error instanceof Error ? error.message : String(error)}`);
      }
    } else if (options.turnId && enableDiffProof) {
      const closed = closeTurnSnapshot(process.cwd(), options.turnId);
      const generatedProof = closed.proof;
      let validationHint: "proof_too_large" | undefined;

      if (generatedProof && proofBytes(generatedProof) > MAX_PROOF_BYTES) {
        if (options.diffProofMode === "strict") {
          throw new Error(`Diff proof exceeds ${MAX_PROOF_BYTES} bytes in strict mode.`);
        }
        validationHint = "proof_too_large";
      }

      diffProof = {
        enabled: true,
        mode: options.diffProofMode === "strict" ? "strict" : "advisory",
        turn_id: options.turnId,
        claim_text: content,
        validation_hint: validationHint,
        proof: validationHint ? undefined : generatedProof,
      };
    }

    const requestBody: JsonRecord = {
      project_id: selectedTarget.projectSlug,
      content,
      metadata: {
        ...(metadata || {}),
        ...(options.turnId ? { turn_id: options.turnId } : {}),
      },
      diff_proof: diffProof,
      force_event: Boolean(options.forceEvent),
      idempotency_key: options.idempotencyKey || `cli-${Date.now()}`,
    };

    try {
      const out = await authenticatedHttp<any>("/v1/memory/events", {
        method: "POST",
        body: requestBody,
        workspaceId: selectedTarget.workspaceId,
      }, cfg);

      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.ingest",
        operation: "cli.memory.ingest",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          diff_proof_enabled: enableDiffProof,
          diff_proof_mode: options.turnId && enableDiffProof ? options.diffProofMode : null,
          turn_id: options.turnId || null,
          deduped: Boolean(out?.deduped),
        },
      });

      const validation = out?.validation;
      const footer = validation?.applied
        ? `Memory updated: yes (status=${validation.status}, confidence=${validation.confidence_after}, proof=${validation.proof_fingerprint || "none"})`
        : "Memory updated: yes (status=not_applied)";
      console.log(JSON.stringify({ ...out, memory_footer: footer }, null, 2));
    } catch (error) {
      if (isAuthRequiredError(error)) {
        const queued = queueMemoryWrite({
          config: cfg,
          kind: "event",
          target: selectedTarget,
          body: requestBody,
          error,
          cwd: process.cwd(),
          summary: "Memory saved locally and will sync after login.",
        });
        await emitCliTelemetry(cfg, {
          eventName: "cli.memory.ingest",
          operation: "cli.memory.ingest",
          durationMs: Date.now() - startedAt,
          projectId: selectedTarget.projectSlug,
          attributes: {
            workspace_slug: selectedTarget.workspaceSlug,
            project_slug: selectedTarget.projectSlug,
            queued_local: true,
            queue_reason: authQueueReason(error),
            diff_proof_enabled: enableDiffProof,
            turn_id: options.turnId || null,
          },
        });
        console.log(JSON.stringify(queued, null, 2));
        return;
      }
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.ingest",
        operation: "cli.memory.ingest",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          error_code: telemetryErrorCode(error),
          diff_proof_enabled: enableDiffProof,
          turn_id: options.turnId || null,
        },
      });
      throw error;
    }
    });
  });

memory
  .command("query")
  .description("Retrieve top related memories for a query")
  .requiredOption("--query <query>")
  .option("--project <project>", "projectSlug or workspaceSlug/projectSlug")
  .option("--lens <lens>", "raw|mixed|overview", "mixed")
  .option("--mode <mode>", "quick|standard|deep|forensic", "standard")
  .option("--synthesize", "Synthesize locally after retrieval using a local worker")
  .option("--worker-target <target>", "codex|claude|copilot. Select the local worker runtime for synthesis")
  .option("--worker-model <model>", "Override the local worker model for synthesis (implies --synthesize)")
  .option("--json", "Return the raw JSON retrieval object")
  .option("--verbose", "Include retrieval diagnostics in raw JSON output (requires --json)")
  .option("--turn-id <id>", "Turn identifier to start local tracking snapshot")
  .action(async (options) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    return withCliSpan(cfg, {
      name: "cli.memory.query",
      attributes: {
        "cli.command": "memory query",
        "memory.lens": options.lens,
        "memory.mode": options.mode,
      },
    }, async () => {
    const selectedTarget = await resolveTargetForCommand({
      config: cfg,
      cwd: process.cwd(),
      explicit: options.project,
    });

    const startTracking = Boolean(options.turnId);

    if (startTracking) {
      startTurnSnapshot(process.cwd(), options.turnId);
    }

    const jsonMode = Boolean(options.json);
    const synthesizeMode = Boolean(options.synthesize || options.workerModel || options.workerTarget);
    if (cliHasFlag("--verbose") && !jsonMode) {
      throw new Error("--verbose requires --json.");
    }
    const showLiveStatus = !jsonMode && process.stderr.isTTY;
    let spinner: { stop: () => void } | null = null;
    let liveStatusCleared = false;

    const clearLiveStatus = () => {
      if (liveStatusCleared) return;
      liveStatusCleared = true;
      spinner?.stop();
      spinner = null;
    };

    try {
      if (showLiveStatus) {
        spinner = startCliSpinner("searching memories");
      }
      const responseFormat = synthesizeMode || jsonMode ? "json" : "structured";
      const out = await authenticatedHttp<any>("/v1/memory/retrieve", {
        method: "POST",
        body: {
          project_id: selectedTarget.projectSlug,
          query: options.query,
          lens: options.lens,
          mode: options.mode,
          k: 10,
          verbose: jsonMode && Boolean(options.verbose),
          response_format: responseFormat,
        },
        workspaceId: selectedTarget.workspaceId,
      }, cfg);
      const synthesized = synthesizeMode
        ? await (async () => {
          if (showLiveStatus) {
            spinner?.stop();
            spinner = startCliSpinner("synthesizing locally");
          }
          return synthesizeMemoryQueryLocally({
            cwd: process.cwd(),
            query: options.query,
            retrieval: out,
            workerTarget: options.workerTarget,
            workerModel: options.workerModel,
          });
        })()
        : null;
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.query",
        operation: "cli.memory.query",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          lens: options.lens,
          mode: options.mode,
          verbose: jsonMode && Boolean(options.verbose),
          json_mode: jsonMode,
          synthesize: synthesizeMode,
          worker_target: synthesized?.worker_target || normalizeValue(options.workerTarget) || null,
          worker_model: synthesized?.worker_model || normalizeValue(options.workerModel) || null,
          start_tracking: startTracking,
          turn_id: options.turnId || null,
        },
      });
      clearLiveStatus();
      if (jsonMode) {
        const payload = synthesized ? { ...out, synthesis: synthesized } : out;
        console.log(JSON.stringify(payload, null, 2));
      } else if (synthesized) {
        const lines = [
          "SYNTHESIS",
          synthesized.answer,
        ];
        if (synthesized.caveats.length) {
          lines.push("");
          lines.push("CAVEATS");
          for (const caveat of synthesized.caveats) {
            lines.push(`- ${caveat}`);
          }
        }
        lines.push("");
        lines.push(formatMemoryRetrieveForCli(out));
        console.log(formatStructuredQueryForCli(lines.join("\n")));
      } else if (typeof out?.structured === "string") {
        console.log(formatStructuredQueryForCli(out.structured));
      } else {
        console.log(JSON.stringify(out, null, 2));
      }
    } catch (error) {
      clearLiveStatus();
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.query",
        operation: "cli.memory.query",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          error_code: telemetryErrorCode(error),
          lens: options.lens,
          mode: options.mode,
          verbose: Boolean(options.json) && Boolean(options.verbose),
          synthesize: synthesizeMode,
          worker_target: normalizeValue(options.workerTarget) || null,
          worker_model: normalizeValue(options.workerModel) || null,
        },
      });
      throw error;
    } finally {
      clearLiveStatus();
    }
    });
  });

const memorySource = memory.command("source").description("Manage durable sources for direct retrieval");

memorySource
  .command("ingest")
  .description("Ingest a durable source such as a handover note, research doc, or spec")
  .requiredOption("--title <title>")
  .requiredOption("--source-type <type>", "handover|research|spec|notes|code|web_capture|other")
  .option("--project <project>", "projectSlug or workspaceSlug/projectSlug")
  .option("--uri <uri>")
  .option("--file <path>")
  .option("--stdin", "Read from stdin")
  .option("--metadata <json>", "JSON metadata payload")
  .action(async (options) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    const selectedTarget = await resolveTargetForCommand({
      config: cfg,
      cwd: process.cwd(),
      explicit: options.project,
      requireWritable: true,
    });

    let metadata: JsonRecord | undefined;
    let content = "";
    assertExactlyOneInputSource(options, "memory source ingest");
    if (options.stdin) {
      content = await new Promise<string>((resolvePromise) => {
        let out = "";
        process.stdin.setEncoding("utf8");
        process.stdin.on("data", (chunk) => {
          out += chunk;
        });
        process.stdin.on("end", () => resolvePromise(out));
      });
    } else if (options.file) {
      content = readFileSync(options.file, "utf8");
    }

    if (options.metadata) {
      try {
        const parsed = JSON.parse(options.metadata) as unknown;
        if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
          throw new Error("Metadata must be a JSON object.");
        }
        metadata = parsed as JsonRecord;
      } catch (error) {
        throw new Error(`Invalid --metadata JSON: ${error instanceof Error ? error.message : String(error)}`);
      }
    }

    const requestBody: JsonRecord = {
      project_id: selectedTarget.projectSlug,
      title: options.title,
      source_type: options.sourceType,
      uri: options.uri,
      content,
      metadata,
    };

    try {
      const out = await authenticatedHttp<any>("/v1/memory/sources", {
        method: "POST",
        body: requestBody,
        workspaceId: selectedTarget.workspaceId,
      }, cfg);

      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.source.ingest",
        operation: "cli.memory.source.ingest",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          source_type: options.sourceType,
          deduped: Boolean(out?.deduped),
        },
      });
      console.log(JSON.stringify(out, null, 2));
    } catch (error) {
      if (isAuthRequiredError(error)) {
        const queued = queueMemoryWrite({
          config: cfg,
          kind: "source",
          target: selectedTarget,
          body: requestBody,
          error,
          cwd: process.cwd(),
          summary: "Durable source saved locally and will sync after login.",
        });
        await emitCliTelemetry(cfg, {
          eventName: "cli.memory.source.ingest",
          operation: "cli.memory.source.ingest",
          durationMs: Date.now() - startedAt,
          projectId: selectedTarget.projectSlug,
          attributes: {
            workspace_slug: selectedTarget.workspaceSlug,
            project_slug: selectedTarget.projectSlug,
            source_type: options.sourceType,
            queued_local: true,
            queue_reason: authQueueReason(error),
          },
        });
        console.log(JSON.stringify(queued, null, 2));
        return;
      }
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.source.ingest",
        operation: "cli.memory.source.ingest",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          source_type: options.sourceType,
          error_code: telemetryErrorCode(error),
        },
      });
      throw error;
    }
  });

memorySource
  .command("query")
  .description("Query durable sources directly for exact passages")
  .requiredOption("--query <query>")
  .option("--project <project>", "projectSlug or workspaceSlug/projectSlug")
  .option("--k <k>", "Maximum number of source hits", "5")
  .option("--max-chunks-per-source <count>", "Maximum chunk hits to return per source", "2")
  .action(async (options) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    const selectedTarget = await resolveTargetForCommand({
      config: cfg,
      cwd: process.cwd(),
      explicit: options.project,
    });

    try {
      const out = await authenticatedHttp<any>("/v1/memory/sources/query", {
        method: "POST",
        body: {
          project_id: selectedTarget.projectSlug,
          query: options.query,
          k: Number(options.k) || 5,
          max_chunks_per_source: Number(options.maxChunksPerSource) || 2,
        },
        workspaceId: selectedTarget.workspaceId,
      }, cfg);
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.source.query",
        operation: "cli.memory.source.query",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          k: Number(options.k) || 5,
        },
      });
      console.log(JSON.stringify(out, null, 2));
    } catch (error) {
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.source.query",
        operation: "cli.memory.source.query",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          error_code: telemetryErrorCode(error),
        },
      });
      throw error;
    }
  });

memorySource
  .command("get")
  .description("Fetch one durable source by source id")
  .requiredOption("--source-id <sourceId>")
  .option("--project <project>", "projectSlug or workspaceSlug/projectSlug")
  .action(async (options) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    const selectedTarget = await resolveTargetForCommand({
      config: cfg,
      cwd: process.cwd(),
      explicit: options.project,
    });

    try {
      const out = await authenticatedHttp<any>(`/v1/memory/sources/${options.sourceId}`, {
        workspaceId: selectedTarget.workspaceId,
      }, cfg);
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.source.get",
        operation: "cli.memory.source.get",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
        },
      });
      console.log(JSON.stringify(out, null, 2));
    } catch (error) {
      await emitCliTelemetry(cfg, {
        eventName: "cli.memory.source.get",
        operation: "cli.memory.source.get",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget.projectSlug,
        attributes: {
          workspace_slug: selectedTarget.workspaceSlug,
          project_slug: selectedTarget.projectSlug,
          error_code: telemetryErrorCode(error),
        },
      });
      throw error;
    }
  });

const agent = program.command("agent").description("Install and inspect agent memory integrations");

agent
  .command("install")
  .description("Install Recallstack hooks/rules for codex, claude, cursor, copilot, or all")
  .argument("<target>", "supported targets: codex|claude|cursor|copilot|all")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .option("--project <project>", "Workspace default project projectSlug or workspaceSlug/projectSlug")
  .option("--global", "Install into user-level agent config instead of this repo")
  .option("--worker-model <model>", "Model used for background worker runs for this target")
  .option("--no-prompt", "Disable interactive project prompt")
  .action(async (targetName, options) => {
    const startedAt = Date.now();
    const normalized = targetName as AgentTarget;
    const scope: AgentInstallScope = options.global ? "global" : "local";
    if (!["codex", "claude", "cursor", "copilot", "all"].includes(normalized)) {
      throw new Error(`Unsupported target "${targetName}". Use codex, claude, cursor, copilot, or all.`);
    }
    if (scope === "global" && options.project) {
      throw new Error("`--project` cannot be used with `agent install --global`. Configure repo targets separately with `recallstack project use`.");
    }

    const cfg = getRuntimeConfig();
    const currentTarget = await loadLocalTarget(options.cwd, cfg);

    let selectedTarget: ResolvedTarget | undefined;
    if (scope === "local") {
      if (options.project) {
        selectedTarget = await resolveTargetForCommand({
          config: cfg,
          cwd: options.cwd,
          explicit: options.project,
          requireWritable: true,
        });
      } else {
        selectedTarget = await loadLocalTarget(options.cwd, cfg);
        if (!selectedTarget && options.prompt !== false) {
          const writableTargets = await listWritableTargets(cfg);
          selectedTarget = await promptForTargetChoice(writableTargets, currentTarget);
        }
        if (!selectedTarget) {
          throw new Error("No default project configured for this directory. Run `recallstack project use` first.");
        }
        if (selectedTarget.mode === "GLOBAL") {
          throw new Error("GLOBAL_WRITE_FORBIDDEN: raw memory writes cannot target global. Choose a non-global project.");
        }
      }
    }

    try {
      const workspaceConfigPath = scope === "local" && selectedTarget
        ? saveWorkspaceTarget(options.cwd, selectedTarget)
        : null;
      const agentSettings = applyWorkerModelSetting(options.cwd, normalized, options.workerModel, scope);
      const installed = installAgentIntegration(normalized, options.cwd, {
        workerModel: options.workerModel,
        scope,
      });
      await emitCliTelemetry(cfg, {
        eventName: "cli.agent.install",
        operation: "cli.agent.install",
        durationMs: Date.now() - startedAt,
        projectId: selectedTarget?.projectSlug,
        attributes: {
          target: normalized,
          scope,
          workspace_slug: selectedTarget?.workspaceSlug || null,
          project_slug: selectedTarget?.projectSlug || null,
          worker_model: normalizeValue(options.workerModel) || null,
        },
      });

      console.log(JSON.stringify({
        cwd: options.cwd,
        scope,
        workspace_config_path: workspaceConfigPath,
        target: selectedTarget
          ? formatCanonicalTarget({ workspaceSlug: selectedTarget.workspaceSlug, projectSlug: selectedTarget.projectSlug })
          : null,
        workspace_id: selectedTarget?.workspaceId || null,
        workspace_slug: selectedTarget?.workspaceSlug || null,
        project_id: selectedTarget?.projectId || null,
        project_slug: selectedTarget?.projectSlug || null,
        previous_target: currentTarget
          ? formatCanonicalTarget({ workspaceSlug: currentTarget.workspaceSlug, projectSlug: currentTarget.projectSlug })
          : null,
        agent_settings_path: agentSettings.path || null,
        installed,
      }, null, 2));
    } catch (error) {
      await emitCliTelemetry(cfg, {
        eventName: "cli.agent.install",
        operation: "cli.agent.install",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        attributes: {
          target: normalized,
          scope,
          worker_model: normalizeValue(options.workerModel) || null,
          error_code: telemetryErrorCode(error),
        },
      });
      throw error;
    }
  });

agent
  .command("uninstall")
  .description("Remove Recallstack hooks/rules for codex, claude, cursor, copilot, or all")
  .argument("<target>", "codex|claude|cursor|copilot|all")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .option("--global", "Remove the user-level installation instead of this repo installation")
  .option("--purge-project-state", "Also remove .recallstack project state for this workspace")
  .action(async (target, options: { cwd: string; purgeProjectState?: boolean }) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    const normalized = target as AgentTarget;
    const scope: AgentInstallScope = (options as { global?: boolean }).global ? "global" : "local";
    if (!["codex", "claude", "cursor", "copilot", "all"].includes(normalized)) {
      throw new Error('Invalid target. Use "codex", "claude", "cursor", "copilot", or "all".');
    }
    if (scope === "global" && options.purgeProjectState) {
      throw new Error("`--purge-project-state` is only valid for local uninstall.");
    }

    try {
      const removed = uninstallAgentIntegration(normalized, options.cwd, {
        purgeProjectState: options.purgeProjectState,
        scope,
      });
      const workerSettings = clearWorkerModelSetting(options.cwd, normalized, scope);
      await emitCliTelemetry(cfg, {
        eventName: "cli.agent.uninstall",
        operation: "cli.agent.uninstall",
        durationMs: Date.now() - startedAt,
        attributes: {
          target: normalized,
          scope,
          purge_project_state: Boolean(options.purgeProjectState),
        },
      });

      console.log(JSON.stringify({
        cwd: options.cwd,
        scope,
        target: normalized,
        purge_project_state: Boolean(options.purgeProjectState),
        agent_settings_path: workerSettings?.path || null,
        removed,
      }, null, 2));
    } catch (error) {
      await emitCliTelemetry(cfg, {
        eventName: "cli.agent.uninstall",
        operation: "cli.agent.uninstall",
        outcome: "error",
        durationMs: Date.now() - startedAt,
        attributes: {
          target: normalized,
          scope,
          purge_project_state: Boolean(options.purgeProjectState),
          error_code: telemetryErrorCode(error),
        },
      });
      throw error;
    }
  });

agent
  .command("status")
  .description("Show integration install status and resolved project target")
  .option("--cwd <cwd>", "Target workspace", process.cwd())
  .option("--global", "Show the user-level installation status instead of this repo status")
  .action(async (options) => {
    const startedAt = Date.now();
    const cfg = getRuntimeConfig();
    const scope: AgentInstallScope = options.global ? "global" : "local";
    const workspaceConfig = scope === "local" ? findWorkspaceConfig(options.cwd) : { path: null };
    const selectedTarget = scope === "local" ? await loadLocalTarget(options.cwd, cfg) : undefined;

    const codex = resolveCodexIntegrationPaths(options.cwd, scope);
    const claude = resolveClaudeIntegrationPaths(options.cwd, scope);
    const cursor = resolveCursorIntegrationPaths(options.cwd, scope);
    const copilotLocal = resolveCopilotLocalIntegrationPaths(options.cwd);
    const copilotGlobal = resolveCopilotGlobalIntegrationPaths();
    const agentSettingsPath = getAgentSettingsPath(options.cwd, scope);
    const localAgentSettings = readAgentSettings(options.cwd, "local");
    const globalAgentSettings = readAgentSettings(options.cwd, "global");
    const codexConfigText = readTextFile(codex.configPath);

    await emitCliTelemetry(cfg, {
      eventName: "cli.agent.status",
      operation: "cli.agent.status",
      durationMs: Date.now() - startedAt,
      projectId: selectedTarget?.projectSlug,
      attributes: {
        scope,
        workspace_slug: selectedTarget?.workspaceSlug || null,
        project_slug: selectedTarget?.projectSlug || null,
      },
    });
    const currentAuthConfig = getRuntimeConfig();

    console.log(JSON.stringify({
      config_path: getConfigPath(),
      api_base: getRuntimeBaseUrl(cfg),
      workspace_config_path: workspaceConfig.path || null,
      scope,
      agent_settings_path: agentSettingsPath,
      local_agent_settings_path: getAgentSettingsPath(options.cwd, "local"),
      global_agent_settings_path: getAgentSettingsPath(options.cwd, "global"),
      target: selectedTarget
        ? formatCanonicalTarget({ workspaceSlug: selectedTarget.workspaceSlug, projectSlug: selectedTarget.projectSlug })
        : null,
      workspace_id: selectedTarget?.workspaceId || null,
      workspace_slug: selectedTarget?.workspaceSlug || null,
      project_id: selectedTarget?.projectId || null,
      project_slug: selectedTarget?.projectSlug || null,
      authenticated: Boolean(currentAuthConfig.accessToken || currentAuthConfig.apiKey),
      auth_status: summarizeAuthState(currentAuthConfig),
      codex: {
        skill_dir: codex.skillDir,
        skill_path: codex.skillPath,
        skill_installed: existsSync(codex.skillPath),
        skill_spec: "agentskills.io",
        hook_dir: codex.hookDir,
        hook_path: codex.hookPath,
        hook_installed: existsSync(codex.hookPath),
        hooks_config_path: codex.hooksConfigPath,
        hooks_config_present: existsSync(codex.hooksConfigPath),
        config_path: codex.configPath,
        config_present: existsSync(codex.configPath),
        repo_hooks_feature_enabled: tomlKeyEnabled(codexConfigText, "features", "codex_hooks", "true"),
        worker_model: workerModelForStatus(options.cwd, scope, "codex").value,
        worker_model_source: workerModelForStatus(options.cwd, scope, "codex").source,
      },
      claude: {
        hook_path: claude.hookPath,
        settings_path: claude.settingsPath,
        hook_installed: existsSync(claude.hookPath),
        settings_present: existsSync(claude.settingsPath),
        skill_path: claude.skillPath,
        skill_installed: existsSync(claude.skillPath),
        worker_model: workerModelForStatus(options.cwd, scope, "claude").value,
        worker_model_source: workerModelForStatus(options.cwd, scope, "claude").source,
      },
      cursor: {
        rule_path: cursor.rulePath,
        rule_installed: existsSync(cursor.rulePath),
        hook_path: cursor.hookPath,
        hook_installed: existsSync(cursor.hookPath),
        hooks_config_path: cursor.hooksConfigPath,
        hooks_config_present: existsSync(cursor.hooksConfigPath),
        ide_only: true,
        cli_supported: false,
      },
      copilot: {
        ...(scope === "global"
          ? {
            plugin_source_root: copilotGlobal.pluginSourceRoot,
            plugin_source_present: existsSync(copilotGlobal.pluginSourceRoot),
            plugin_manifest_path: copilotGlobal.pluginManifestPath,
            plugin_manifest_present: existsSync(copilotGlobal.pluginManifestPath),
            skill_path: copilotGlobal.skillPath,
            skill_present: existsSync(copilotGlobal.skillPath),
            hook_path: copilotGlobal.hookPath,
            hook_present: existsSync(copilotGlobal.hookPath),
            hooks_config_path: copilotGlobal.hooksConfigPath,
            hooks_config_present: existsSync(copilotGlobal.hooksConfigPath),
            installed_plugin_path: copilotGlobal.installedPluginPath,
            installed_plugin_present: existsSync(copilotGlobal.installedPluginPath),
            config_dir: copilotGlobal.configDir,
          }
          : {
            instructions_path: copilotLocal.instructionsPath,
            instructions_installed: existsSync(copilotLocal.instructionsPath),
            hook_path: copilotLocal.hookPath,
            hook_installed: existsSync(copilotLocal.hookPath),
            cli_hooks_config_path: copilotLocal.cliHooksConfigPath,
            cli_hooks_config_present: existsSync(copilotLocal.cliHooksConfigPath),
            vscode_hooks_config_path: copilotLocal.vscodeHooksConfigPath,
            vscode_hooks_config_present: existsSync(copilotLocal.vscodeHooksConfigPath),
          }),
        cli_supported: true,
        vscode_supported: true,
        worker_model: workerModelForStatus(options.cwd, scope, "copilot").value,
        worker_model_source: workerModelForStatus(options.cwd, scope, "copilot").source,
      },
      worker_models: {
        local: localAgentSettings,
        global: globalAgentSettings,
      },
    }, null, 2));
  });

function rewriteLegacyAliases(argv: string[]): string[] {
  const next = [...argv];
  if (next[2] === "target") {
    next[2] = "project";
  }
  if (next[2] === "config" || next[2] === "workspace" || next[2] === "project") {
    const firstArg = next[3];
    if (!firstArg || (firstArg.startsWith("-") && firstArg !== "--help" && firstArg !== "-h")) {
      next.splice(3, 0, "show");
    }
  }
  return next;
}

program.parseAsync(rewriteLegacyAliases(process.argv)).catch((error) => {
  console.error(error.message);
  process.exit(1);
});
