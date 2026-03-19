import { existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { authenticatedHttp } from "./auth.js";
import type { RecallstackConfig } from "./config.js";
import { HttpError } from "./http.js";

export type PendingWriteKind = "event" | "source";

export type PendingWriteRecord = {
  id: string;
  fingerprint: string;
  kind: PendingWriteKind;
  baseUrl: string;
  workspaceId: string;
  projectToken: string;
  endpoint: "/v1/memory/events" | "/v1/memory/sources";
  body: Record<string, unknown>;
  origin: "cli" | "hook";
  cwd?: string;
  queuedAt: string;
  replayCount: number;
  queueReason: string;
  lastError?: {
    message: string;
    code?: string;
  };
  lastAttemptAt?: string;
};

export type QueuePendingWriteInput = {
  kind: PendingWriteKind;
  baseUrl: string;
  workspaceId: string;
  projectToken: string;
  body: Record<string, unknown>;
  origin: "cli" | "hook";
  cwd?: string;
  queueReason: string;
  lastError?: {
    message: string;
    code?: string;
  };
};

export type FlushPendingWritesResult = {
  attempted: number;
  replayed: number;
  failed: number;
  blockedByAuth: boolean;
  remaining: number;
};

export type PendingWriteSender = (
  record: PendingWriteRecord,
  config: RecallstackConfig,
) => Promise<unknown>;

type PendingWriteOptions = {
  dir?: string;
};

const DEFAULT_PENDING_WRITES_DIR = join(homedir(), ".recallstack", "pending-writes");

function pendingWritesDir(options: PendingWriteOptions = {}): string {
  return options.dir || DEFAULT_PENDING_WRITES_DIR;
}

function readJsonFile<T>(filePath: string, fallback: T): T {
  if (!existsSync(filePath)) return fallback;
  try {
    return JSON.parse(readFileSync(filePath, "utf8")) as T;
  } catch {
    return fallback;
  }
}

function writeJsonFile(filePath: string, value: unknown): void {
  mkdirSync(dirname(filePath), { recursive: true });
  writeFileSync(filePath, `${JSON.stringify(value, null, 2)}\n`, "utf8");
}

function stableStringify(value: unknown): string {
  if (value === null || typeof value !== "object") {
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map((item) => stableStringify(item)).join(",")}]`;
  }
  const entries = Object.entries(value as Record<string, unknown>)
    .filter(([, entryValue]) => entryValue !== undefined)
    .sort(([left], [right]) => left.localeCompare(right));
  return `{${entries.map(([key, entryValue]) => `${JSON.stringify(key)}:${stableStringify(entryValue)}`).join(",")}}`;
}

function fingerprintForInput(input: QueuePendingWriteInput): string {
  return createHash("sha1").update(stableStringify({
    kind: input.kind,
    baseUrl: input.baseUrl,
    workspaceId: input.workspaceId,
    projectToken: input.projectToken,
    body: input.body,
  })).digest("hex");
}

function pendingWritePath(id: string, options: PendingWriteOptions = {}): string {
  return join(pendingWritesDir(options), `${id}.json`);
}

function readPendingWriteRecords(options: PendingWriteOptions = {}): PendingWriteRecord[] {
  const dir = pendingWritesDir(options);
  if (!existsSync(dir)) return [];
  const items: PendingWriteRecord[] = [];
  for (const entry of readdirSync(dir)) {
    if (!entry.endsWith(".json")) continue;
    const record = readJsonFile<PendingWriteRecord | null>(join(dir, entry), null);
    if (!record || typeof record !== "object") continue;
    if (typeof record.id !== "string" || !record.id.length) continue;
    if (typeof record.fingerprint !== "string" || !record.fingerprint.length) continue;
    if (record.kind !== "event" && record.kind !== "source") continue;
    if (typeof record.baseUrl !== "string" || !record.baseUrl.length) continue;
    if (typeof record.workspaceId !== "string" || !record.workspaceId.length) continue;
    if (typeof record.projectToken !== "string" || !record.projectToken.length) continue;
    if (record.endpoint !== "/v1/memory/events" && record.endpoint !== "/v1/memory/sources") continue;
    if (typeof record.body !== "object" || record.body === null || Array.isArray(record.body)) continue;
    items.push(record);
  }
  return items.sort((left, right) => left.queuedAt.localeCompare(right.queuedAt));
}

function defaultSender(record: PendingWriteRecord, config: RecallstackConfig): Promise<unknown> {
  return authenticatedHttp(record.endpoint, {
    method: "POST",
    body: record.body,
    workspaceId: record.workspaceId,
  }, config);
}

export function isAuthRequiredError(error: unknown): boolean {
  if (error instanceof HttpError && error.status === 401) {
    return true;
  }
  const message = error instanceof Error ? error.message : String(error ?? "");
  return /not authenticated|authentication expired|http 401|invalid refresh token|unauthorized/i.test(message);
}

export function authQueueReason(error: unknown): string {
  if (error instanceof HttpError && error.status === 401) {
    return "HTTP_401";
  }
  const message = error instanceof Error ? error.message : String(error ?? "");
  if (/authentication expired/i.test(message)) {
    return "AUTHENTICATION_EXPIRED";
  }
  if (/not authenticated/i.test(message)) {
    return "NOT_AUTHENTICATED";
  }
  return "AUTH_REQUIRED";
}

export function queuePendingWrite(
  input: QueuePendingWriteInput,
  options: PendingWriteOptions = {},
): { record: PendingWriteRecord; duplicate: boolean } {
  const fingerprint = fingerprintForInput(input);
  const existing = readPendingWriteRecords(options).find((record) => record.fingerprint === fingerprint);
  if (existing) {
    const updated: PendingWriteRecord = {
      ...existing,
      queueReason: input.queueReason,
      lastError: input.lastError || existing.lastError,
      cwd: input.cwd || existing.cwd,
    };
    writeJsonFile(pendingWritePath(existing.id, options), updated);
    return {
      record: updated,
      duplicate: true,
    };
  }

  const id = createHash("sha1").update(`${fingerprint}:${Date.now()}`).digest("hex");
  const endpoint = input.kind === "event" ? "/v1/memory/events" : "/v1/memory/sources";
  const record: PendingWriteRecord = {
    id,
    fingerprint,
    kind: input.kind,
    baseUrl: input.baseUrl,
    workspaceId: input.workspaceId,
    projectToken: input.projectToken,
    endpoint,
    body: input.body,
    origin: input.origin,
    cwd: input.cwd,
    queuedAt: new Date().toISOString(),
    replayCount: 0,
    queueReason: input.queueReason,
    lastError: input.lastError,
  };
  writeJsonFile(pendingWritePath(id, options), record);
  return {
    record,
    duplicate: false,
  };
}

export function listPendingWrites(
  filter: { baseUrl?: string } = {},
  options: PendingWriteOptions = {},
): PendingWriteRecord[] {
  return readPendingWriteRecords(options).filter((record) => (
    !filter.baseUrl || record.baseUrl === filter.baseUrl
  ));
}

export function removePendingWrite(id: string, options: PendingWriteOptions = {}): void {
  rmSync(pendingWritePath(id, options), { force: true });
}

export async function flushPendingWrites(
  input: {
    config: RecallstackConfig;
    sender?: PendingWriteSender;
  },
  options: PendingWriteOptions = {},
): Promise<FlushPendingWritesResult> {
  const sender = input.sender || defaultSender;
  const queue = listPendingWrites({ baseUrl: input.config.effectiveBaseUrl }, options);
  let attempted = 0;
  let replayed = 0;
  let failed = 0;
  let blockedByAuth = false;

  for (const record of queue) {
    attempted += 1;
    try {
      await sender(record, input.config);
      replayed += 1;
      removePendingWrite(record.id, options);
    } catch (error) {
      const updated: PendingWriteRecord = {
        ...record,
        replayCount: record.replayCount + 1,
        lastAttemptAt: new Date().toISOString(),
        lastError: {
          message: error instanceof Error ? error.message : String(error),
          ...(typeof error === "object" && error !== null && typeof (error as { code?: unknown }).code === "string"
            ? { code: (error as { code: string }).code }
            : {}),
        },
      };
      writeJsonFile(pendingWritePath(record.id, options), updated);
      failed += 1;
      if (isAuthRequiredError(error)) {
        blockedByAuth = true;
        break;
      }
    }
  }

  return {
    attempted,
    replayed,
    failed,
    blockedByAuth,
    remaining: listPendingWrites({ baseUrl: input.config.effectiveBaseUrl }, options).length,
  };
}
