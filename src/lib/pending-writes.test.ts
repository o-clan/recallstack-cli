import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  authQueueReason,
  flushPendingWrites,
  isAuthRequiredError,
  listPendingWrites,
  queuePendingWrite,
} from "./pending-writes.js";
import { HttpError } from "./http.js";

function tempQueueDir(): string {
  return mkdtempSync(join(tmpdir(), "recallstack-pending-writes-"));
}

test("queuePendingWrite deduplicates repeated pending writes", () => {
  const dir = tempQueueDir();
  const first = queuePendingWrite({
    kind: "event",
    baseUrl: "https://api.recallstack.com",
    workspaceId: "ws_123",
    projectToken: "project-alpha",
    body: {
      project_id: "project-alpha",
      content: "Remember this change",
      idempotency_key: "event-1",
    },
    origin: "cli",
    queueReason: "NOT_AUTHENTICATED",
  }, { dir });
  const second = queuePendingWrite({
    kind: "event",
    baseUrl: "https://api.recallstack.com",
    workspaceId: "ws_123",
    projectToken: "project-alpha",
    body: {
      project_id: "project-alpha",
      content: "Remember this change",
      idempotency_key: "event-1",
    },
    origin: "cli",
    queueReason: "HTTP_401",
  }, { dir });

  assert.equal(first.duplicate, false);
  assert.equal(second.duplicate, true);
  assert.equal(first.record.id, second.record.id);
  assert.equal(listPendingWrites({}, { dir }).length, 1);
  assert.equal(listPendingWrites({}, { dir })[0]?.queueReason, "HTTP_401");
});

test("flushPendingWrites replays and clears queued writes after login", async () => {
  const dir = tempQueueDir();
  queuePendingWrite({
    kind: "source",
    baseUrl: "https://api.recallstack.com",
    workspaceId: "ws_456",
    projectToken: "project-beta",
    body: {
      project_id: "project-beta",
      title: "Plan",
      source_type: "spec",
      content: "Stored while logged out",
    },
    origin: "hook",
    queueReason: "AUTHENTICATION_EXPIRED",
  }, { dir });

  const sentIds: string[] = [];
  const result = await flushPendingWrites({
    config: {
      effectiveBaseUrl: "https://api.recallstack.com",
    },
    sender: async (record) => {
      sentIds.push(record.id);
      return { ok: true };
    },
  }, { dir });

  assert.equal(result.attempted, 1);
  assert.equal(result.replayed, 1);
  assert.equal(result.failed, 0);
  assert.equal(result.remaining, 0);
  assert.deepEqual(sentIds.length, 1);
  assert.equal(listPendingWrites({}, { dir }).length, 0);
});

test("flushPendingWrites stops when replay is still blocked by auth", async () => {
  const dir = tempQueueDir();
  const queued = queuePendingWrite({
    kind: "event",
    baseUrl: "https://api.recallstack.com",
    workspaceId: "ws_789",
    projectToken: "project-gamma",
    body: {
      project_id: "project-gamma",
      content: "Pending auth replay",
      idempotency_key: "event-2",
    },
    origin: "hook",
    queueReason: "NOT_AUTHENTICATED",
  }, { dir });

  const result = await flushPendingWrites({
    config: {
      effectiveBaseUrl: "https://api.recallstack.com",
    },
    sender: async () => {
      throw new HttpError(401, { error: { code: "UNAUTHORIZED" } }, "UNAUTHORIZED");
    },
  }, { dir });

  assert.equal(result.attempted, 1);
  assert.equal(result.replayed, 0);
  assert.equal(result.failed, 1);
  assert.equal(result.blockedByAuth, true);
  assert.equal(result.remaining, 1);
  assert.equal(listPendingWrites({}, { dir })[0]?.id, queued.record.id);
});

test("isAuthRequiredError and authQueueReason classify auth failures", () => {
  assert.equal(isAuthRequiredError(new Error("Not authenticated. Run `recallstack login`.")), true);
  assert.equal(isAuthRequiredError(new Error("Authentication expired. Run `recallstack login`.")), true);
  assert.equal(isAuthRequiredError(new HttpError(401, { error: { code: "UNAUTHORIZED" } }, "UNAUTHORIZED")), true);
  assert.equal(isAuthRequiredError(new Error("socket hang up")), false);
  assert.equal(authQueueReason(new Error("Authentication expired. Run `recallstack login`.")), "AUTHENTICATION_EXPIRED");
  assert.equal(authQueueReason(new Error("Not authenticated. Run `recallstack login`.")), "NOT_AUTHENTICATED");
  assert.equal(authQueueReason(new HttpError(401, { error: { code: "UNAUTHORIZED" } }, "UNAUTHORIZED")), "HTTP_401");
});
