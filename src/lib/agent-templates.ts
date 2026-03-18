export const CLAUDE_HOOK_TEMPLATE = `#!/usr/bin/env node
import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";

const RECALLSTACK_BIN = process.env.RECALLSTACK_BIN || "recallstack";
const CLAUDE_BIN = process.env.CLAUDE_BIN || "claude";
const EXTRACT_SCHEMA = {
  "$schema": "http://json-schema.org/draft-07/schema#",
  type: "object",
  additionalProperties: false,
  properties: {
    turn_kind: { type: "string", enum: ["substantial", "thin", "pleasantry"] },
    user_intent: { type: "string" },
    agent_intent: { type: "string" },
    key_actions: { type: "array", items: { type: "string" } },
    tradeoffs: { type: "array", items: { type: "string" } },
    outcome: { type: "string" },
    open_questions: { type: "array", items: { type: "string" } }
  },
  required: [
    "turn_kind",
    "user_intent",
    "agent_intent",
    "key_actions",
    "tradeoffs",
    "outcome",
    "open_questions"
  ]
};

function ensureDir(dirPath) {
  mkdirSync(dirPath, { recursive: true });
}

function readJsonFile(filePath, fallback) {
  if (!existsSync(filePath)) return fallback;
  try {
    return JSON.parse(readFileSync(filePath, "utf8"));
  } catch {
    return fallback;
  }
}

function readJsonFromString(raw, fallback) {
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

function writeJsonFile(filePath, value) {
  ensureDir(dirname(filePath));
  writeFileSync(filePath, JSON.stringify(value, null, 2) + "\\n", "utf8");
}

function appendLog(stateDir, fileName, entry) {
  ensureDir(stateDir);
  appendFileSync(join(stateDir, fileName), JSON.stringify({
    timestamp: new Date().toISOString(),
    ...entry,
  }) + "\\n", "utf8");
}

function appendError(stateDir, message, extra) {
  appendLog(stateDir, "errors.log", { message, extra: extra || null });
}

function appendEvent(stateDir, event, extra) {
  appendLog(stateDir, "events.log", { event, extra: extra || null });
}

function readStdin() {
  return new Promise((resolve) => {
    let out = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => {
      out += chunk;
    });
    process.stdin.on("end", () => resolve(out));
  });
}

function run(command, args, options) {
  return spawnSync(command, args, {
    encoding: "utf8",
    stdio: ["pipe", "pipe", "pipe"],
    ...options,
  });
}

function truncate(value, limit) {
  if (typeof value !== "string") return "";
  const normalized = value.trim();
  if (normalized.length <= limit) return normalized;
  return normalized.slice(0, limit - 3) + "...";
}

function parsePositiveInt(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

const RETRY_WINDOW_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_WINDOW_MS, 24 * 60 * 60 * 1000);
const RETRY_BASE_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_BASE_MS, 30 * 1000);
const RETRY_MAX_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_MAX_MS, 15 * 60 * 1000);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function stateDirForCwd(cwd) {
  return join(cwd, ".recallstack", "claude-hooks");
}

function retryDirForState(stateDir) {
  return join(stateDir, "retry");
}

function retryDelayMs(attemptCount) {
  return Math.min(RETRY_MAX_DELAY_MS, RETRY_BASE_DELAY_MS * Math.pow(2, Math.max(0, attemptCount - 1)));
}

function isRetryableIngestFailure(result) {
  const combined = String(result.stdout || "") + "\\n" + String(result.stderr || "");
  return /(429|too many requests|rate limit|rate-limit|service unavailable|temporarily unavailable|bad gateway|gateway timeout|upstream|fetch failed|network error|socket hang up|connection refused|econnrefused|enotfound|ehostunreach|etimedout|timed out|timeout|5\\d\\d)/i.test(combined);
}

function resolveWorkerModel(cwd) {
  const envModel = typeof process.env.RECALLSTACK_CLAUDE_WORKER_MODEL === "string"
    ? process.env.RECALLSTACK_CLAUDE_WORKER_MODEL.trim()
    : typeof process.env.RECALLSTACK_WORKER_MODEL === "string"
      ? process.env.RECALLSTACK_WORKER_MODEL.trim()
      : "";
  if (envModel.length) {
    return envModel;
  }

  const settings = readJsonFile(join(cwd, ".recallstack", "agent-settings.json"), {});
  const configured = settings && typeof settings === "object" && settings.claude && typeof settings.claude === "object"
    ? settings.claude.worker_model
    : "";
  if (typeof configured === "string" && configured.trim().length) {
    return configured.trim();
  }

  const globalSettings = readJsonFile(join(homedir(), ".recallstack", "agent-settings.json"), {});
  const globalConfigured = globalSettings && typeof globalSettings === "object" && globalSettings.claude && typeof globalSettings.claude === "object"
    ? globalSettings.claude.worker_model
    : "";
  return typeof globalConfigured === "string" ? globalConfigured.trim() : "";
}

function sessionStatePath(stateDir, sessionId) {
  return join(stateDir, "sessions", sessionId + ".json");
}

function updateSessionState(stateDir, sessionId, updates) {
  const filePath = sessionStatePath(stateDir, sessionId);
  const current = readJsonFile(filePath, {});
  writeJsonFile(filePath, {
    ...current,
    ...updates,
    updated_at: new Date().toISOString(),
  });
}

function extractTextContent(content) {
  if (typeof content === "string") {
    return content.trim();
  }
  if (!Array.isArray(content)) {
    return "";
  }
  return content
    .map((item) => {
      if (!item || typeof item !== "object") return "";
      if (item.type === "text" && typeof item.text === "string") {
        return item.text;
      }
      return "";
    })
    .filter(Boolean)
    .join("\\n")
    .trim();
}

function extractToolUses(content) {
  if (!Array.isArray(content)) {
    return [];
  }
  return content
    .map((item) => {
      if (!item || typeof item !== "object" || item.type !== "tool_use") return "";
      return typeof item.name === "string" ? item.name.trim() : "";
    })
    .filter(Boolean);
}

function parseTranscript(transcriptPath) {
  if (!existsSync(transcriptPath)) return [];
  return readFileSync(transcriptPath, "utf8")
    .split(/\\r?\\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .flatMap((line) => {
      try {
        return [JSON.parse(line)];
      } catch {
        return [];
      }
    });
}

function findLatestCompletedTurn(transcriptPath, payload) {
  const entries = parseTranscript(transcriptPath);
  let lastAssistantIndex = -1;
  for (let index = entries.length - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    if (entry?.type !== "assistant" || entry?.message?.role !== "assistant") {
      continue;
    }
    const text = extractTextContent(entry.message?.content);
    if (text.length) {
      lastAssistantIndex = index;
      break;
    }
  }

  if (lastAssistantIndex === -1) {
    const fallbackText = typeof payload.last_assistant_message === "string" ? payload.last_assistant_message.trim() : "";
    if (!fallbackText.length) {
      return null;
    }
    return {
      dedupe_key: typeof payload.stop_hook_active === "string" && payload.stop_hook_active.length
        ? payload.stop_hook_active
        : "assistant:" + createHash("sha1").update(fallbackText).digest("hex"),
      last_assistant_message: fallbackText,
      user_messages: [],
      assistant_messages: [fallbackText],
      items: [
        {
          kind: "assistant_message",
          text: fallbackText,
        },
      ],
    };
  }

  let previousAssistantIndex = -1;
  for (let index = lastAssistantIndex - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    if (entry?.type !== "assistant" || entry?.message?.role !== "assistant") {
      continue;
    }
    const text = extractTextContent(entry.message?.content);
    if (text.length) {
      previousAssistantIndex = index;
      break;
    }
  }

  const assistantEntry = entries[lastAssistantIndex];
  const slice = entries.slice(previousAssistantIndex + 1);
  const userMessages = [];
  const assistantMessages = [];
  const items = [];

  for (const entry of slice) {
    if (entry?.type === "user" && entry?.message?.role === "user") {
      const text = extractTextContent(entry.message?.content);
      if (text.length) {
        userMessages.push(text);
        items.push({
          kind: "user_message",
          text,
        });
      }
      continue;
    }

    if (entry?.type === "assistant" && entry?.message?.role === "assistant") {
      const text = extractTextContent(entry.message?.content);
      const toolUses = extractToolUses(entry.message?.content);
      if (text.length) {
        assistantMessages.push(text);
        items.push({
          kind: "assistant_message",
          text,
        });
      }
      for (const toolName of toolUses) {
        items.push({
          kind: "tool_use",
          name: toolName,
        });
      }
    }
  }

  return {
    dedupe_key: typeof assistantEntry?.uuid === "string" && assistantEntry.uuid.length
      ? assistantEntry.uuid
      : typeof assistantEntry?.timestamp === "string" && assistantEntry.timestamp.length
        ? assistantEntry.timestamp
        : "assistant-index:" + String(lastAssistantIndex),
    last_assistant_message: assistantMessages[assistantMessages.length - 1]
      || (typeof payload.last_assistant_message === "string" ? payload.last_assistant_message : null),
    user_messages: userMessages,
    assistant_messages: assistantMessages,
    items,
  };
}

function resolveProjectTarget(cwd, stateDir) {
  const result = run(RECALLSTACK_BIN, ["project", "--cwd", cwd], { cwd });
  if (result.status !== 0) {
    appendError(stateDir, "Failed to resolve Recallstack target.", {
      stdout: truncate(result.stdout || "", 800),
      stderr: truncate(result.stderr || "", 800),
    });
    return null;
  }

  const parsed = readJsonFromString(result.stdout, null);
  if (!parsed || typeof parsed.target !== "string" || !parsed.target.length || parsed.project_slug === "global") {
    return null;
  }
  return parsed;
}

function buildExtractorPrompt(turnPayload) {
  return [
    "You are extracting Recallstack turn memory from a completed Claude Code turn.",
    "Return JSON matching the schema exactly.",
    "Use only the provided turn payload.",
    "The source of truth for user_intent is the latest real user message in turnPayload.user_messages.",
    "If multiple real user messages are present, treat the latest one as the active steering instruction and use earlier user messages only as supporting context.",
    "If the latest user message is referential or underspecified (for example: 'Let's go with Option 1', 'do that', 'go ahead'), you may use turnPayload.previous_turn_snapshot to clarify the intent.",
    "Use at most that one-turn lookback. Do not reconstruct deeper history into user_intent.",
    "Rewrite that message for clarity only. Preserve its meaning, even if the user message is vague or has typos.",
    "Do not describe the extraction task, hook task, JSON task, or Recallstack ingestion task as user_intent.",
    "Capture the project-relevant facts that should be remembered later:",
    "- what the user wanted",
    "- what the agent set out to do",
    "- meaningful actions taken",
    "- tradeoffs or alternatives discussed",
    "- the current outcome or best answer",
    "- open questions or next steps when relevant",
    "Avoid raw terminal noise, ids, timestamps, and low-value housekeeping.",
    "Classify the turn with turn_kind:",
    "- substantial: concrete implementation, decisions, verification, meaningful research, or high-signal project progress",
    "- thin: short but still meaningful steering or confirmation, such as approval to proceed or a brief clarification that changes direction",
    "- pleasantry: pure courtesy/closure like thank you, noted, or you're welcome with no project signal",
    "",
    "TURN PAYLOAD",
    JSON.stringify(turnPayload, null, 2),
  ].join("\\n");
}

function parseClaudeStructuredOutput(raw) {
  const direct = readJsonFromString(raw, null);
  if (direct && typeof direct === "object" && !Array.isArray(direct) && "turn_kind" in direct) {
    return direct;
  }
  if (direct && typeof direct === "object" && !Array.isArray(direct)) {
    const resultText = typeof direct.result === "string"
      ? direct.result
      : Array.isArray(direct.content)
        ? direct.content
          .map((item) => (item && typeof item === "object" && item.type === "text" && typeof item.text === "string" ? item.text : ""))
          .filter(Boolean)
          .join("\\n")
        : "";
    if (resultText.length) {
      return readJsonFromString(resultText, null);
    }
  }
  return null;
}

function runExtractor(turnPayload, cwd, stateDir) {
  const workerModel = resolveWorkerModel(cwd);
  const args = [
    "-p",
    "--output-format",
    "json",
    "--json-schema",
    JSON.stringify(EXTRACT_SCHEMA),
    "--tools",
    "",
    "--no-session-persistence",
    "--append-system-prompt",
    "You are a strict JSON extraction pass for Recallstack hook ingestion.",
  ];
  if (workerModel.length) {
    args.splice(1, 0, "--model", workerModel);
  }

  const result = run(
    CLAUDE_BIN,
    args,
    {
      cwd,
      input: buildExtractorPrompt(turnPayload),
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );

  if (result.status !== 0) {
    appendError(stateDir, "Claude extractor failed.", {
      status: result.status,
      stdout: truncate(result.stdout || "", 1200),
      stderr: truncate(result.stderr || "", 1200),
      cwd,
    });
    return fallbackExtractedTurn(turnPayload);
  }

  const parsed = normalizeExtractedTurn(parseClaudeStructuredOutput(result.stdout || ""), turnPayload);
  if (!parsed) {
    appendError(stateDir, "Claude extractor returned unreadable output.", {
      stdout: truncate(result.stdout || "", 1200),
      stderr: truncate(result.stderr || "", 1200),
    });
    return fallbackExtractedTurn(turnPayload);
  }
  return parsed;
}

function compactList(values) {
  return Array.isArray(values)
    ? values.map((value) => (typeof value === "string" ? value.trim() : "")).filter(Boolean)
    : [];
}

function compactText(value) {
  if (typeof value !== "string") return "";
  return value.replace(/\\s+/g, " ").trim();
}

function directUserIntent(turnPayload) {
  const values = compactList(turnPayload && turnPayload.user_messages);
  if (!values.length) {
    return "";
  }
  return compactText(values[values.length - 1]);
}

function directAssistantOutcome(turnPayload) {
  const values = compactList(turnPayload && turnPayload.assistant_messages);
  if (values.length) {
    return compactText(values[values.length - 1]);
  }
  return compactText(turnPayload && turnPayload.last_assistant_message);
}

function fallbackExtractedTurn(turnPayload) {
  const userIntent = directUserIntent(turnPayload);
  const outcome = directAssistantOutcome(turnPayload);
  return {
    turn_kind: isPleasantryOnlyTurn(turnPayload) ? "pleasantry" : "thin",
    user_intent: userIntent,
    agent_intent: outcome.length ? "Respond to the user's latest request and move the work forward." : "",
    key_actions: [],
    tradeoffs: [],
    outcome,
    open_questions: [],
  };
}

function isPleasantryOnlyTurn(turnPayload) {
  const latestUserMessage = directUserIntent(turnPayload);
  if (!latestUserMessage.length) {
    return false;
  }
  return /^(thanks|thank you|thx|got it(?:,? thanks)?|okay,? thanks|ok,? thanks|cool,? thanks|nice,? thanks|appreciate it|sounds good,? thanks|perfect,? thanks)[!. ]*$/i.test(latestUserMessage);
}

function buildTurnSnapshot(extracted, summary) {
  return {
    turn_kind: typeof extracted.turn_kind === "string" ? extracted.turn_kind : "thin",
    user_intent: compactText(extracted.user_intent),
    agent_intent: compactText(extracted.agent_intent),
    outcome: compactText(extracted.outcome),
    summary: compactText(summary),
  };
}

function normalizeExtractedTurn(extracted, turnPayload) {
  if (!extracted || typeof extracted !== "object") {
    return fallbackExtractedTurn(turnPayload);
  }
  const sourceUserMessage = directUserIntent(turnPayload);
  const extractedKind = typeof extracted.turn_kind === "string" ? extracted.turn_kind.trim() : "";
  const normalizedKind = extractedKind === "substantial" || extractedKind === "thin" || extractedKind === "pleasantry"
    ? extractedKind
    : fallbackExtractedTurn(turnPayload).turn_kind;
  const turnKind = isPleasantryOnlyTurn(turnPayload) ? "pleasantry" : normalizedKind === "pleasantry" ? "thin" : normalizedKind;
  return {
    ...extracted,
    turn_kind: turnKind,
    user_intent:
      (() => {
        const extractedIntent = compactText(extracted.user_intent);
        if (!sourceUserMessage.length) {
          return extractedIntent;
        }
        if (extractedIntent.length && !/(capture a recallstack memory|provided codex turn payload|provided claude code turn payload|provided github copilot turn payload|extraction task|hook task|json task|recallstack ingestion task)/i.test(extractedIntent)) {
          return extractedIntent;
        }
        return sourceUserMessage;
      })(),
    agent_intent: compactText(extracted.agent_intent),
    outcome: compactText(extracted.outcome) || directAssistantOutcome(turnPayload),
  };
}

function formatSummary(extracted, turnPayload) {
  const lines = [];
  const turnKind = typeof extracted.turn_kind === "string" ? extracted.turn_kind : "thin";
  if (typeof extracted.user_intent === "string" && extracted.user_intent.trim().length) {
    lines.push("User intent: " + extracted.user_intent.trim());
  }
  if (turnKind === "substantial" && typeof extracted.agent_intent === "string" && extracted.agent_intent.trim().length) {
    lines.push("Agent intent: " + extracted.agent_intent.trim());
  }
  const keyActions = compactList(extracted.key_actions);
  if (turnKind === "substantial" && keyActions.length) {
    lines.push("Actions: " + keyActions.join("; "));
  }
  const tradeoffs = compactList(extracted.tradeoffs);
  if (turnKind === "substantial" && tradeoffs.length) {
    lines.push("Tradeoffs: " + tradeoffs.join("; "));
  }
  if (typeof extracted.outcome === "string" && extracted.outcome.trim().length) {
    lines.push("Outcome: " + extracted.outcome.trim());
  }
  const openQuestions = compactList(extracted.open_questions);
  if (turnKind === "substantial" && openQuestions.length) {
    lines.push("Open questions: " + openQuestions.join("; "));
  }
  if (lines.length) {
    return lines.join("\\n").trim();
  }
  const fallback = fallbackExtractedTurn(turnPayload);
  if (fallback.user_intent.length) {
    lines.push("User intent: " + fallback.user_intent);
  }
  if (fallback.outcome.length) {
    lines.push("Outcome: " + fallback.outcome);
  }
  return lines.join("\\n").trim();
}

function buildRetryState(retryState, ingest) {
  const now = Date.now();
  const createdAt = typeof retryState.created_at === "string" && retryState.created_at.length
    ? retryState.created_at
    : new Date(now).toISOString();
  const createdAtMs = Date.parse(createdAt);
  const expiresAtMs = Number.isFinite(createdAtMs) ? createdAtMs + RETRY_WINDOW_MS : now + RETRY_WINDOW_MS;
  const attemptCount = typeof retryState.attempt_count === "number" && retryState.attempt_count >= 0
    ? retryState.attempt_count + 1
    : 1;
  return {
    ...retryState,
    created_at: createdAt,
    expires_at: new Date(expiresAtMs).toISOString(),
    attempt_count: attemptCount,
    last_failed_at: new Date(now).toISOString(),
    next_attempt_at: new Date(Math.min(expiresAtMs, now + retryDelayMs(attemptCount))).toISOString(),
    last_error: {
      status: ingest.status,
      stdout: truncate(ingest.stdout || "", 1200),
      stderr: truncate(ingest.stderr || "", 1200),
    },
  };
}

function ingestMemory(cwd, target, summary, idempotencyKey, metadata) {
  return run(
    RECALLSTACK_BIN,
    [
      "memory",
      "ingest",
      "--project",
      target,
      "--stdin",
      "--idempotency-key",
      idempotencyKey,
      "--metadata",
      JSON.stringify(metadata),
    ],
    {
      cwd,
      input: summary,
    },
  );
}

function spawnDetachedWorker(jobPath, cwd, stateDir) {
  const child = spawn(
    process.execPath,
    [process.argv[1], "worker-stop", jobPath],
    {
      cwd,
      detached: true,
      stdio: "ignore",
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );
  child.unref();
  appendEvent(stateDir, "job_spawned", {
    job_path: jobPath,
    pid: child.pid,
  });
}

function spawnDetachedRetryWorker(retryPath, cwd, stateDir) {
  const child = spawn(
    process.execPath,
    [process.argv[1], "worker-retry", retryPath],
    {
      cwd,
      detached: true,
      stdio: "ignore",
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );
  child.unref();
  appendEvent(stateDir, "retry_worker_spawned", {
    retry_path: retryPath,
    pid: child.pid,
  });
}

function markClaudeIngested(stateDir, sessionId, dedupeKey) {
  if (typeof sessionId !== "string" || !sessionId.length || typeof dedupeKey !== "string" || !dedupeKey.length) {
    return;
  }
  updateSessionState(stateDir, sessionId, {
    last_ingested_turn_id: dedupeKey,
    last_ingested_at: new Date().toISOString(),
  });
}

function queueRetry(stateDir, retryState, ingest) {
  const retryId = typeof retryState.retry_id === "string" && retryState.retry_id.length
    ? retryState.retry_id
    : createHash("sha1").update(String(retryState.idempotency_key || randomUUID())).digest("hex");
  const retryPath = join(retryDirForState(stateDir), retryId + ".json");
  const nextState = buildRetryState({
    ...retryState,
    retry_id: retryId,
  }, ingest);
  writeJsonFile(retryPath, nextState);
  appendEvent(stateDir, "retry_queued", {
    retry_path: retryPath,
    session_id: nextState.session_id || null,
    dedupe_key: nextState.dedupe_key || null,
    next_attempt_at: nextState.next_attempt_at,
  });
  spawnDetachedRetryWorker(retryPath, nextState.cwd || process.cwd(), stateDir);
}

async function processRetryJob(retryPath) {
  let retryState = readJsonFile(retryPath, null);
  if (!retryState || typeof retryState !== "object") {
    return;
  }

  const cwd = typeof retryState.cwd === "string" && retryState.cwd.length ? retryState.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  while (true) {
    retryState = readJsonFile(retryPath, retryState);
    if (!retryState || typeof retryState !== "object") {
      return;
    }

    const expiresAtMs = Date.parse(typeof retryState.expires_at === "string" ? retryState.expires_at : "");
    const now = Date.now();
    if (Number.isFinite(expiresAtMs) && now >= expiresAtMs) {
      appendEvent(stateDir, "retry_expired", {
        retry_path: retryPath,
        session_id: retryState.session_id || null,
        dedupe_key: retryState.dedupe_key || null,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextAttemptAtMs = Date.parse(typeof retryState.next_attempt_at === "string" ? retryState.next_attempt_at : "");
    if (Number.isFinite(nextAttemptAtMs) && nextAttemptAtMs > now) {
      await sleep(nextAttemptAtMs - now);
      continue;
    }

    if (typeof retryState.target !== "string" || !retryState.target.length || typeof retryState.summary !== "string" || !retryState.summary.length || typeof retryState.idempotency_key !== "string" || !retryState.idempotency_key.length) {
      appendError(stateDir, "Claude retry job is missing ingest payload.", {
        retry_path: retryPath,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const ingest = ingestMemory(cwd, retryState.target, retryState.summary, retryState.idempotency_key, retryState.metadata || {});
    if (ingest.status === 0) {
      markClaudeIngested(stateDir, retryState.session_id, retryState.dedupe_key);
      appendEvent(stateDir, "retry_ingested", {
        retry_path: retryPath,
        session_id: retryState.session_id || null,
        dedupe_key: retryState.dedupe_key || null,
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    if (!isRetryableIngestFailure(ingest)) {
      appendError(stateDir, "Claude retry abandoned after non-retryable ingest failure.", {
        retry_path: retryPath,
        status: ingest.status,
        stdout: truncate(ingest.stdout || "", 1200),
        stderr: truncate(ingest.stderr || "", 1200),
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextState = buildRetryState(retryState, ingest);
    writeJsonFile(retryPath, nextState);
    appendEvent(stateDir, "retry_rescheduled", {
      retry_path: retryPath,
      session_id: nextState.session_id || null,
      dedupe_key: nextState.dedupe_key || null,
      attempt_count: nextState.attempt_count,
      next_attempt_at: nextState.next_attempt_at,
    });
    await sleep(retryDelayMs(nextState.attempt_count));
  }
}

function enqueueStopJob(cwd, stateDir, payload) {
  if (typeof payload.session_id !== "string" || !payload.session_id.length) {
    return;
  }
  if (typeof payload.transcript_path !== "string" || !payload.transcript_path.length) {
    return;
  }

  const jobId = Date.now().toString() + "-" + process.pid.toString() + "-" + randomUUID();
  const jobPath = join(stateDir, "jobs", jobId + ".json");
  writeJsonFile(jobPath, {
    job_id: jobId,
    queued_at: new Date().toISOString(),
    cwd,
    payload,
  });
  updateSessionState(stateDir, payload.session_id, {
    session_id: payload.session_id,
    transcript_path: payload.transcript_path,
    cwd,
    last_stop_queued_at: new Date().toISOString(),
    last_stop_job_id: jobId,
  });
  spawnDetachedWorker(jobPath, cwd, stateDir);
}

async function processStopJob(jobPath) {
  const job = readJsonFile(jobPath, null);
  if (!job || typeof job !== "object") {
    return;
  }

  const cwd = typeof job.cwd === "string" && job.cwd.length ? job.cwd : process.cwd();
  const payload = job.payload && typeof job.payload === "object" ? job.payload : {};
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  try {
    if (typeof payload.session_id !== "string" || !payload.session_id.length) {
      return;
    }
    if (typeof payload.transcript_path !== "string" || !payload.transcript_path.length) {
      return;
    }

    const project = resolveProjectTarget(cwd, stateDir);
    if (!project) {
      return;
    }

    const turn = findLatestCompletedTurn(payload.transcript_path, payload);
    if (!turn || !turn.dedupe_key) {
      appendError(stateDir, "Claude Stop worker could not identify the latest completed turn.", {
        job_path: jobPath,
        transcript_path: payload.transcript_path,
        session_id: payload.session_id,
      });
      return;
    }

    const sessionState = readJsonFile(sessionStatePath(stateDir, payload.session_id), {});
    if (sessionState.last_processed_turn_id === turn.dedupe_key) {
      appendEvent(stateDir, "job_skipped_duplicate", {
        job_path: jobPath,
        session_id: payload.session_id,
        dedupe_key: turn.dedupe_key,
      });
      return;
    }

    const turnPayload = {
      session_id: payload.session_id,
      cwd,
      transcript_path: payload.transcript_path,
      previous_turn_snapshot: sessionState.previous_turn_snapshot || null,
      last_assistant_message: turn.last_assistant_message,
      user_messages: turn.user_messages,
      assistant_messages: turn.assistant_messages,
      items: turn.items,
    };

    const extracted = runExtractor(turnPayload, cwd, stateDir);
    if (!extracted) {
      return;
    }

    updateSessionState(stateDir, payload.session_id, {
      session_id: payload.session_id,
      transcript_path: payload.transcript_path,
      cwd,
      last_processed_turn_id: turn.dedupe_key,
      last_processed_at: new Date().toISOString(),
    });

    if (extracted.turn_kind === "pleasantry") {
      appendEvent(stateDir, "job_noop", {
        job_path: jobPath,
        session_id: payload.session_id,
        dedupe_key: turn.dedupe_key,
      });
      return;
    }

    const summary = formatSummary(extracted, turnPayload);
    if (!summary.length) {
      appendEvent(stateDir, "job_empty_summary", {
        job_path: jobPath,
        session_id: payload.session_id,
        dedupe_key: turn.dedupe_key,
      });
      return;
    }

    updateSessionState(stateDir, payload.session_id, {
      previous_turn_snapshot: buildTurnSnapshot(extracted, summary),
    });

    const metadata = {
      agent: "claude_code",
      source: "claude_stop_hook",
      extractor: "claude_print",
      session_id: payload.session_id,
      transcript_path: payload.transcript_path,
      previous_turn_snapshot_used: Boolean(turnPayload.previous_turn_snapshot),
      turn_kind: extracted.turn_kind || null,
      user_intent: extracted.user_intent || null,
      agent_intent: extracted.agent_intent || null,
      key_actions: compactList(extracted.key_actions),
      tradeoffs: compactList(extracted.tradeoffs),
      open_questions: compactList(extracted.open_questions),
    };

    const idempotencyKey = "claude-hook-" + createHash("sha1").update(payload.session_id + ":" + turn.dedupe_key).digest("hex");
    const ingest = ingestMemory(cwd, project.target, summary, idempotencyKey, metadata);

    if (ingest.status !== 0) {
      if (isRetryableIngestFailure(ingest)) {
        queueRetry(stateDir, {
          cwd,
          target: project.target,
          summary,
          metadata,
          idempotency_key: idempotencyKey,
          session_id: payload.session_id,
          dedupe_key: turn.dedupe_key,
        }, ingest);
        return;
      }
      appendError(stateDir, "Recallstack ingest failed from Claude Stop worker.", {
        status: ingest.status,
        stdout: truncate(ingest.stdout || "", 1200),
        stderr: truncate(ingest.stderr || "", 1200),
        target: project.target,
        dedupe_key: turn.dedupe_key,
        job_path: jobPath,
      });
      return;
    }

    markClaudeIngested(stateDir, payload.session_id, turn.dedupe_key);
    appendEvent(stateDir, "job_ingested", {
      job_path: jobPath,
      session_id: payload.session_id,
      target: project.target,
      dedupe_key: turn.dedupe_key,
    });
  } finally {
    rmSync(jobPath, { force: true });
  }
}

async function main() {
  if (process.argv[2] === "worker-stop" && typeof process.argv[3] === "string") {
    await processStopJob(process.argv[3]);
    return;
  }
  if (process.argv[2] === "worker-retry" && typeof process.argv[3] === "string") {
    await processRetryJob(process.argv[3]);
    return;
  }

  if (process.env.RECALLSTACK_HOOK_ACTIVE === "1") {
    return;
  }

  const eventName = process.argv[2] || "unknown";
  const raw = await readStdin();
  const payload = readJsonFromString(raw, {});
  const cwd = typeof payload.cwd === "string" && payload.cwd.length ? payload.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  if (eventName === "stop") {
    enqueueStopJob(cwd, stateDir, payload);
  }
}

main().catch((error) => {
  const cwd = process.cwd();
  appendError(stateDirForCwd(cwd), "Unhandled Claude hook failure.", {
    error: error instanceof Error ? error.stack || error.message : String(error),
  });
  process.exit(0);
});
`;

export const CODEX_HOOK_TEMPLATE = `#!/usr/bin/env node
import { appendFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";

const RECALLSTACK_BIN = process.env.RECALLSTACK_BIN || "recallstack";
const CODEX_BIN = process.env.CODEX_BIN || "codex";
const EXTRACT_SCHEMA = {
  "$schema": "http://json-schema.org/draft-07/schema#",
  type: "object",
  additionalProperties: false,
  properties: {
    turn_kind: { type: "string", enum: ["substantial", "thin", "pleasantry"] },
    user_intent: { type: "string" },
    agent_intent: { type: "string" },
    key_actions: { type: "array", items: { type: "string" } },
    tradeoffs: { type: "array", items: { type: "string" } },
    outcome: { type: "string" },
    open_questions: { type: "array", items: { type: "string" } }
  },
  required: [
    "turn_kind",
    "user_intent",
    "agent_intent",
    "key_actions",
    "tradeoffs",
    "outcome",
    "open_questions"
  ]
};

function ensureDir(dirPath) {
  mkdirSync(dirPath, { recursive: true });
}

function readJsonFile(filePath, fallback) {
  if (!existsSync(filePath)) return fallback;
  try {
    return JSON.parse(readFileSync(filePath, "utf8"));
  } catch {
    return fallback;
  }
}

function readJsonFromString(raw, fallback) {
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

function writeJsonFile(filePath, value) {
  ensureDir(dirname(filePath));
  writeFileSync(filePath, JSON.stringify(value, null, 2) + "\\n", "utf8");
}

function appendError(stateDir, message, extra) {
  ensureDir(stateDir);
  const entry = {
    timestamp: new Date().toISOString(),
    message,
    extra: extra || null,
  };
  appendFileSync(join(stateDir, "errors.log"), JSON.stringify(entry) + "\\n", "utf8");
}

function appendJobEvent(stateDir, event, extra) {
  ensureDir(stateDir);
  const entry = {
    timestamp: new Date().toISOString(),
    event,
    extra: extra || null,
  };
  appendFileSync(join(stateDir, "jobs.log"), JSON.stringify(entry) + "\\n", "utf8");
}

function readStdin() {
  return new Promise((resolve) => {
    let out = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => {
      out += chunk;
    });
    process.stdin.on("end", () => resolve(out));
  });
}

function run(command, args, options) {
  return spawnSync(command, args, {
    encoding: "utf8",
    stdio: ["pipe", "pipe", "pipe"],
    ...options,
  });
}

function truncate(value, limit) {
  if (typeof value !== "string") return "";
  const normalized = value.trim();
  if (normalized.length <= limit) return normalized;
  return normalized.slice(0, limit - 3) + "...";
}

function parsePositiveInt(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

const RETRY_WINDOW_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_WINDOW_MS, 24 * 60 * 60 * 1000);
const RETRY_BASE_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_BASE_MS, 30 * 1000);
const RETRY_MAX_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_MAX_MS, 15 * 60 * 1000);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function stateDirForCwd(cwd) {
  return join(cwd, ".recallstack", "codex-hooks");
}

function retryDirForState(stateDir) {
  return join(stateDir, "retry");
}

function retryDelayMs(attemptCount) {
  return Math.min(RETRY_MAX_DELAY_MS, RETRY_BASE_DELAY_MS * Math.pow(2, Math.max(0, attemptCount - 1)));
}

function isRetryableIngestFailure(result) {
  const combined = String(result.stdout || "") + "\\n" + String(result.stderr || "");
  return /(429|too many requests|rate limit|rate-limit|service unavailable|temporarily unavailable|bad gateway|gateway timeout|upstream|fetch failed|network error|socket hang up|connection refused|econnrefused|enotfound|ehostunreach|etimedout|timed out|timeout|5\\d\\d)/i.test(combined);
}

function resolveWorkerModel(cwd) {
  const envModel = typeof process.env.RECALLSTACK_CODEX_WORKER_MODEL === "string"
    ? process.env.RECALLSTACK_CODEX_WORKER_MODEL.trim()
    : typeof process.env.RECALLSTACK_WORKER_MODEL === "string"
      ? process.env.RECALLSTACK_WORKER_MODEL.trim()
      : "";
  if (envModel.length) {
    return envModel;
  }

  const settings = readJsonFile(join(cwd, ".recallstack", "agent-settings.json"), {});
  const configured = settings && typeof settings === "object" && settings.codex && typeof settings.codex === "object"
    ? settings.codex.worker_model
    : "";
  if (typeof configured === "string" && configured.trim().length) {
    return configured.trim();
  }

  const globalSettings = readJsonFile(join(homedir(), ".recallstack", "agent-settings.json"), {});
  const globalConfigured = globalSettings && typeof globalSettings === "object" && globalSettings.codex && typeof globalSettings.codex === "object"
    ? globalSettings.codex.worker_model
    : "";
  return typeof globalConfigured === "string" ? globalConfigured.trim() : "";
}

function sessionStatePath(stateDir, sessionId) {
  return join(stateDir, "sessions", sessionId + ".json");
}

function updateSessionState(stateDir, sessionId, updates) {
  const filePath = sessionStatePath(stateDir, sessionId);
  const current = readJsonFile(filePath, {});
  writeJsonFile(filePath, {
    ...current,
    ...updates,
    updated_at: new Date().toISOString(),
  });
}

function recordSessionStart(stateDir, payload) {
  if (typeof payload.session_id !== "string" || !payload.session_id.length) {
    return;
  }
  updateSessionState(stateDir, payload.session_id, {
    session_id: payload.session_id,
    transcript_path: typeof payload.transcript_path === "string" ? payload.transcript_path : null,
    cwd: typeof payload.cwd === "string" ? payload.cwd : process.cwd(),
    source: typeof payload.source === "string" ? payload.source : null,
    started_at: new Date().toISOString(),
  });
}

function extractMessageText(payload) {
  if (!payload || payload.type !== "message" || !Array.isArray(payload.content)) {
    return "";
  }

  return payload.content
    .map((item) => {
      if (!item || typeof item !== "object") return "";
      if (item.type === "input_text" || item.type === "output_text") {
        return typeof item.text === "string" ? item.text : "";
      }
      return "";
    })
    .filter(Boolean)
    .join("\\n")
    .trim();
}

function summarizePayload(payload) {
  if (!payload || typeof payload !== "object") return null;
  const itemType = typeof payload.type === "string" ? payload.type : "unknown";
  if (itemType === "reasoning") return null;
  if (itemType === "message") {
    const text = extractMessageText(payload);
    const role = typeof payload.role === "string" ? payload.role : "unknown";
    if (!text.length) return null;
    return {
      kind: role === "assistant" ? "assistant_message" : role === "user" ? "user_message" : "message",
      role,
      phase: typeof payload.phase === "string" ? payload.phase : null,
      text,
    };
  }

  return {
    kind: "response_item",
    item_type: itemType,
    summary: truncate(JSON.stringify(payload), 1200),
  };
}

function parseTranscript(transcriptPath) {
  if (!existsSync(transcriptPath)) return [];
  return readFileSync(transcriptPath, "utf8")
    .split(/\\r?\\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .flatMap((line) => {
      try {
        return [JSON.parse(line)];
      } catch {
        return [];
      }
    });
}

function findLatestCompletedTurn(transcriptPath) {
  const entries = parseTranscript(transcriptPath);
  let lastAssistantIndex = -1;
  for (let index = entries.length - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    if (
      entry?.type === "response_item"
      && entry?.payload?.type === "message"
      && entry?.payload?.role === "assistant"
      && extractMessageText(entry.payload).length > 0
    ) {
      lastAssistantIndex = index;
      break;
    }
  }

  if (lastAssistantIndex === -1) {
    return null;
  }

  let previousAssistantIndex = -1;
  for (let index = lastAssistantIndex - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    if (
      entry?.type === "response_item"
      && entry?.payload?.type === "message"
      && entry?.payload?.role === "assistant"
      && extractMessageText(entry.payload).length > 0
    ) {
      previousAssistantIndex = index;
      break;
    }
  }

  const assistantEntry = entries[lastAssistantIndex];
  const slice = entries.slice(previousAssistantIndex + 1);
  const items = [];
  const userMessages = [];
  const assistantMessages = [];
  let turnId = null;
  let dedupeKey = typeof assistantEntry?.timestamp === "string"
    ? "assistant:" + assistantEntry.timestamp
    : "assistant-index:" + String(lastAssistantIndex);

  for (const entry of slice) {
    if (entry?.type === "response_item") {
      const summary = summarizePayload(entry.payload);
      if (!summary) {
        continue;
      }
      items.push(summary);
      if (summary.kind === "user_message") {
        userMessages.push(summary.text);
      }
      if (summary.kind === "assistant_message") {
        assistantMessages.push(summary.text);
      }
      continue;
    }

    if (entry?.type === "event_msg" && entry?.payload?.type === "task_complete") {
      if (typeof entry.payload.turn_id === "string" && entry.payload.turn_id.length) {
        turnId = entry.payload.turn_id;
        dedupeKey = entry.payload.turn_id;
      }
      items.push({
        kind: "task_complete",
        turn_id: typeof entry.payload.turn_id === "string" ? entry.payload.turn_id : null,
        last_agent_message: typeof entry.payload.last_agent_message === "string" ? entry.payload.last_agent_message : null,
      });
      continue;
    }

    if (entry?.type === "event_msg" && entry?.payload?.type === "task_started") {
      if (!turnId && typeof entry.payload.turn_id === "string" && entry.payload.turn_id.length) {
        turnId = entry.payload.turn_id;
      }
      items.push({
        kind: "task_started",
        turn_id: typeof entry.payload.turn_id === "string" ? entry.payload.turn_id : null,
      });
    }
  }

  return {
    turn_id: turnId,
    dedupe_key: dedupeKey,
    last_agent_message: assistantMessages[assistantMessages.length - 1] || null,
    items,
    user_messages: userMessages,
    assistant_messages: assistantMessages,
  };
}

async function waitForLatestCompletedTurn(transcriptPath) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    const turn = findLatestCompletedTurn(transcriptPath);
    if (turn && turn.dedupe_key) {
      return turn;
    }
    await sleep(250);
  }
  return null;
}

function resolveProjectTarget(cwd, stateDir) {
  const result = run(RECALLSTACK_BIN, ["project", "--cwd", cwd], { cwd });
  if (result.status !== 0) {
    appendError(stateDir, "Failed to resolve Recallstack target.", {
      stdout: truncate(result.stdout || "", 800),
      stderr: truncate(result.stderr || "", 800),
    });
    return null;
  }

  const parsed = readJsonFromString(result.stdout, null);
  if (!parsed || typeof parsed.target !== "string" || !parsed.target.length || parsed.project_slug === "global") {
    return null;
  }
  return parsed;
}

function buildExtractorPrompt(turnPayload) {
  return [
    "You are extracting Recallstack turn memory from a completed Codex turn.",
    "Return JSON matching the schema.",
    "Use only the provided turn payload.",
    "The source of truth for user_intent is the latest real user message in turnPayload.user_messages.",
    "If multiple real user messages are present, treat the latest one as the active steering instruction and use earlier user messages only as supporting context.",
    "If the latest user message is referential or underspecified (for example: 'Let's go with Option 1', 'do that', 'go ahead'), you may use turnPayload.previous_turn_snapshot to clarify the intent.",
    "Use at most that one-turn lookback. Do not reconstruct deeper history into user_intent.",
    "Rewrite that message for clarity only. Preserve its meaning, even if the user message is vague or has typos.",
    "Do not describe the extraction task, hook task, JSON task, or Recallstack ingestion task as user_intent.",
    "Capture the project-relevant facts that should be remembered later:",
    "- what the user wanted",
    "- what the agent set out to do",
    "- meaningful actions taken",
    "- tradeoffs or alternatives discussed",
    "- the current outcome or best answer",
    "- open questions or next steps when relevant",
    "Avoid raw terminal noise, ids, timestamps, and low-value housekeeping.",
    "Classify the turn with turn_kind:",
    "- substantial: concrete implementation, decisions, verification, meaningful research, or high-signal project progress",
    "- thin: short but still meaningful steering or confirmation, such as approval to proceed or a brief clarification that changes direction",
    "- pleasantry: pure courtesy/closure like thank you, noted, or you're welcome with no project signal",
    "",
    "TURN PAYLOAD",
    JSON.stringify(turnPayload, null, 2),
  ].join("\\n");
}

function runExtractor(turnPayload, cwd, stateDir) {
  const workDir = mkdtempSync(join(tmpdir(), "recallstack-codex-hook-"));
  const schemaPath = join(workDir, "schema.json");
  const outputPath = join(workDir, "output.json");
  writeJsonFile(schemaPath, EXTRACT_SCHEMA);
  const workerModel = resolveWorkerModel(cwd);
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
  if (workerModel.length) {
    args.splice(1, 0, "--model", workerModel);
  }

  const result = run(
    CODEX_BIN,
    args,
    {
      cwd: workDir,
      input: buildExtractorPrompt(turnPayload),
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );

  try {
    if (result.status !== 0 || !existsSync(outputPath)) {
      appendError(stateDir, "Codex extractor failed.", {
        status: result.status,
        stdout: truncate(result.stdout || "", 1200),
        stderr: truncate(result.stderr || "", 1200),
        cwd,
      });
      return fallbackExtractedTurn(turnPayload);
    }
    return normalizeExtractedTurn(readJsonFile(outputPath, null), turnPayload);
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
}

function compactList(values) {
  return Array.isArray(values)
    ? values.map((value) => (typeof value === "string" ? value.trim() : "")).filter(Boolean)
    : [];
}

function compactText(value) {
  if (typeof value !== "string") return "";
  return value.replace(/\\s+/g, " ").trim();
}

function directUserIntent(turnPayload) {
  const values = compactList(turnPayload && turnPayload.user_messages);
  if (!values.length) {
    return "";
  }
  return compactText(values[values.length - 1]);
}

function directAssistantOutcome(turnPayload) {
  const values = compactList(turnPayload && turnPayload.assistant_messages);
  if (values.length) {
    return compactText(values[values.length - 1]);
  }
  return compactText(turnPayload && turnPayload.last_assistant_message);
}

function fallbackExtractedTurn(turnPayload) {
  const userIntent = directUserIntent(turnPayload);
  const outcome = directAssistantOutcome(turnPayload);
  return {
    turn_kind: isPleasantryOnlyTurn(turnPayload) ? "pleasantry" : "thin",
    user_intent: userIntent,
    agent_intent: outcome.length ? "Respond to the user's latest request and move the work forward." : "",
    key_actions: [],
    tradeoffs: [],
    outcome,
    open_questions: [],
  };
}

function isPleasantryOnlyTurn(turnPayload) {
  const latestUserMessage = directUserIntent(turnPayload);
  if (!latestUserMessage.length) {
    return false;
  }
  return /^(thanks|thank you|thx|got it(?:,? thanks)?|okay,? thanks|ok,? thanks|cool,? thanks|nice,? thanks|appreciate it|sounds good,? thanks|perfect,? thanks)[!. ]*$/i.test(latestUserMessage);
}

function buildTurnSnapshot(extracted, summary) {
  return {
    turn_kind: typeof extracted.turn_kind === "string" ? extracted.turn_kind : "thin",
    user_intent: compactText(extracted.user_intent),
    agent_intent: compactText(extracted.agent_intent),
    outcome: compactText(extracted.outcome),
    summary: compactText(summary),
  };
}

function normalizeExtractedTurn(extracted, turnPayload) {
  if (!extracted || typeof extracted !== "object") {
    return fallbackExtractedTurn(turnPayload);
  }
  const sourceUserMessage = directUserIntent(turnPayload);
  const extractedKind = typeof extracted.turn_kind === "string" ? extracted.turn_kind.trim() : "";
  const normalizedKind = extractedKind === "substantial" || extractedKind === "thin" || extractedKind === "pleasantry"
    ? extractedKind
    : fallbackExtractedTurn(turnPayload).turn_kind;
  const turnKind = isPleasantryOnlyTurn(turnPayload) ? "pleasantry" : normalizedKind === "pleasantry" ? "thin" : normalizedKind;
  return {
    ...extracted,
    turn_kind: turnKind,
    user_intent:
      (() => {
        const extractedIntent = compactText(extracted.user_intent);
        if (!sourceUserMessage.length) {
          return extractedIntent;
        }
        if (extractedIntent.length && !/(capture a recallstack memory|provided codex turn payload|provided claude code turn payload|provided github copilot turn payload|extraction task|hook task|json task|recallstack ingestion task)/i.test(extractedIntent)) {
          return extractedIntent;
        }
        return sourceUserMessage;
      })(),
    agent_intent: compactText(extracted.agent_intent),
    outcome: compactText(extracted.outcome) || directAssistantOutcome(turnPayload),
  };
}

function formatSummary(extracted, turnPayload) {
  const lines = [];
  const turnKind = typeof extracted.turn_kind === "string" ? extracted.turn_kind : "thin";
  if (typeof extracted.user_intent === "string" && extracted.user_intent.trim().length) {
    lines.push("User intent: " + extracted.user_intent.trim());
  }
  if (turnKind === "substantial" && typeof extracted.agent_intent === "string" && extracted.agent_intent.trim().length) {
    lines.push("Agent intent: " + extracted.agent_intent.trim());
  }
  const keyActions = compactList(extracted.key_actions);
  if (turnKind === "substantial" && keyActions.length) {
    lines.push("Actions: " + keyActions.join("; "));
  }
  const tradeoffs = compactList(extracted.tradeoffs);
  if (turnKind === "substantial" && tradeoffs.length) {
    lines.push("Tradeoffs: " + tradeoffs.join("; "));
  }
  if (typeof extracted.outcome === "string" && extracted.outcome.trim().length) {
    lines.push("Outcome: " + extracted.outcome.trim());
  }
  const openQuestions = compactList(extracted.open_questions);
  if (turnKind === "substantial" && openQuestions.length) {
    lines.push("Open questions: " + openQuestions.join("; "));
  }
  if (lines.length) {
    return lines.join("\\n").trim();
  }
  const fallback = fallbackExtractedTurn(turnPayload);
  if (fallback.user_intent.length) {
    lines.push("User intent: " + fallback.user_intent);
  }
  if (fallback.outcome.length) {
    lines.push("Outcome: " + fallback.outcome);
  }
  return lines.join("\\n").trim();
}

function buildRetryState(retryState, ingest) {
  const now = Date.now();
  const createdAt = typeof retryState.created_at === "string" && retryState.created_at.length
    ? retryState.created_at
    : new Date(now).toISOString();
  const createdAtMs = Date.parse(createdAt);
  const expiresAtMs = Number.isFinite(createdAtMs) ? createdAtMs + RETRY_WINDOW_MS : now + RETRY_WINDOW_MS;
  const attemptCount = typeof retryState.attempt_count === "number" && retryState.attempt_count >= 0
    ? retryState.attempt_count + 1
    : 1;
  return {
    ...retryState,
    created_at: createdAt,
    expires_at: new Date(expiresAtMs).toISOString(),
    attempt_count: attemptCount,
    last_failed_at: new Date(now).toISOString(),
    next_attempt_at: new Date(Math.min(expiresAtMs, now + retryDelayMs(attemptCount))).toISOString(),
    last_error: {
      status: ingest.status,
      stdout: truncate(ingest.stdout || "", 1200),
      stderr: truncate(ingest.stderr || "", 1200),
    },
  };
}

function ingestMemory(cwd, target, summary, idempotencyKey, metadata) {
  return run(
    RECALLSTACK_BIN,
    [
      "memory",
      "ingest",
      "--project",
      target,
      "--stdin",
      "--idempotency-key",
      idempotencyKey,
      "--metadata",
      JSON.stringify(metadata),
    ],
    {
      cwd,
      input: summary,
    },
  );
}

function spawnDetachedWorker(jobPath, cwd, stateDir) {
  const child = spawn(
    process.execPath,
    [process.argv[1], "worker-stop", jobPath],
    {
      cwd,
      detached: true,
      stdio: "ignore",
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );
  child.unref();
  appendJobEvent(stateDir, "job_spawned", {
    job_path: jobPath,
    pid: child.pid,
  });
}

function spawnDetachedRetryWorker(retryPath, cwd, stateDir) {
  const child = spawn(
    process.execPath,
    [process.argv[1], "worker-retry", retryPath],
    {
      cwd,
      detached: true,
      stdio: "ignore",
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );
  child.unref();
  appendJobEvent(stateDir, "retry_worker_spawned", {
    retry_path: retryPath,
    pid: child.pid,
  });
}

function markCodexIngested(stateDir, sessionId, turnKey) {
  if (typeof sessionId !== "string" || !sessionId.length || typeof turnKey !== "string" || !turnKey.length) {
    return;
  }
  updateSessionState(stateDir, sessionId, {
    last_ingested_turn_id: turnKey,
    last_ingested_at: new Date().toISOString(),
  });
}

function queueRetry(stateDir, retryState, ingest) {
  const retryId = typeof retryState.retry_id === "string" && retryState.retry_id.length
    ? retryState.retry_id
    : createHash("sha1").update(String(retryState.idempotency_key || randomUUID())).digest("hex");
  const retryPath = join(retryDirForState(stateDir), retryId + ".json");
  const nextState = buildRetryState({
    ...retryState,
    retry_id: retryId,
  }, ingest);
  writeJsonFile(retryPath, nextState);
  appendJobEvent(stateDir, "retry_queued", {
    retry_path: retryPath,
    session_id: nextState.session_id || null,
    dedupe_key: nextState.dedupe_key || null,
    next_attempt_at: nextState.next_attempt_at,
  });
  spawnDetachedRetryWorker(retryPath, nextState.cwd || process.cwd(), stateDir);
}

async function processRetryJob(retryPath) {
  let retryState = readJsonFile(retryPath, null);
  if (!retryState || typeof retryState !== "object") {
    return;
  }

  const cwd = typeof retryState.cwd === "string" && retryState.cwd.length ? retryState.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  while (true) {
    retryState = readJsonFile(retryPath, retryState);
    if (!retryState || typeof retryState !== "object") {
      return;
    }

    const expiresAtMs = Date.parse(typeof retryState.expires_at === "string" ? retryState.expires_at : "");
    const now = Date.now();
    if (Number.isFinite(expiresAtMs) && now >= expiresAtMs) {
      appendJobEvent(stateDir, "retry_expired", {
        retry_path: retryPath,
        session_id: retryState.session_id || null,
        dedupe_key: retryState.dedupe_key || null,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextAttemptAtMs = Date.parse(typeof retryState.next_attempt_at === "string" ? retryState.next_attempt_at : "");
    if (Number.isFinite(nextAttemptAtMs) && nextAttemptAtMs > now) {
      await sleep(nextAttemptAtMs - now);
      continue;
    }

    if (typeof retryState.target !== "string" || !retryState.target.length || typeof retryState.summary !== "string" || !retryState.summary.length || typeof retryState.idempotency_key !== "string" || !retryState.idempotency_key.length) {
      appendError(stateDir, "Codex retry job is missing ingest payload.", {
        retry_path: retryPath,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const ingest = ingestMemory(cwd, retryState.target, retryState.summary, retryState.idempotency_key, retryState.metadata || {});
    if (ingest.status === 0) {
      markCodexIngested(stateDir, retryState.session_id, retryState.turn_key || retryState.dedupe_key);
      appendJobEvent(stateDir, "retry_ingested", {
        retry_path: retryPath,
        session_id: retryState.session_id || null,
        dedupe_key: retryState.dedupe_key || null,
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    if (!isRetryableIngestFailure(ingest)) {
      appendError(stateDir, "Codex retry abandoned after non-retryable ingest failure.", {
        retry_path: retryPath,
        status: ingest.status,
        stdout: truncate(ingest.stdout || "", 1200),
        stderr: truncate(ingest.stderr || "", 1200),
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextState = buildRetryState(retryState, ingest);
    writeJsonFile(retryPath, nextState);
    appendJobEvent(stateDir, "retry_rescheduled", {
      retry_path: retryPath,
      session_id: nextState.session_id || null,
      dedupe_key: nextState.dedupe_key || null,
      attempt_count: nextState.attempt_count,
      next_attempt_at: nextState.next_attempt_at,
    });
    await sleep(retryDelayMs(nextState.attempt_count));
  }
}

function enqueueStopJob(cwd, stateDir, payload) {
  if (typeof payload.session_id !== "string" || !payload.session_id.length) {
    return;
  }
  if (typeof payload.transcript_path !== "string" || !payload.transcript_path.length) {
    return;
  }

  const jobId = Date.now().toString() + "-" + process.pid.toString() + "-" + randomUUID();
  const jobPath = join(stateDir, "jobs", jobId + ".json");
  writeJsonFile(jobPath, {
    job_id: jobId,
    queued_at: new Date().toISOString(),
    cwd,
    payload,
  });
  updateSessionState(stateDir, payload.session_id, {
    session_id: payload.session_id,
    transcript_path: payload.transcript_path,
    cwd,
    last_stop_queued_at: new Date().toISOString(),
    last_stop_job_id: jobId,
  });
  spawnDetachedWorker(jobPath, cwd, stateDir);
}

async function processStopJob(jobPath) {
  const job = readJsonFile(jobPath, null);
  if (!job || typeof job !== "object") {
    return;
  }

  const cwd = typeof job.cwd === "string" && job.cwd.length ? job.cwd : process.cwd();
  const payload = job.payload && typeof job.payload === "object" ? job.payload : {};
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  try {
    if (typeof payload.session_id !== "string" || !payload.session_id.length) {
      return;
    }
    if (typeof payload.transcript_path !== "string" || !payload.transcript_path.length) {
      return;
    }

    const project = resolveProjectTarget(cwd, stateDir);
    if (!project) {
      return;
    }

    const turn = await waitForLatestCompletedTurn(payload.transcript_path);
    if (!turn || !turn.dedupe_key) {
      appendError(stateDir, "Stop worker could not find a completed turn in transcript.", {
        job_path: jobPath,
        transcript_path: payload.transcript_path,
        session_id: payload.session_id,
      });
      return;
    }

    const sessionState = readJsonFile(sessionStatePath(stateDir, payload.session_id), {});
    if (sessionState.last_processed_turn_id === turn.dedupe_key) {
      appendJobEvent(stateDir, "job_skipped_duplicate", {
        job_path: jobPath,
        session_id: payload.session_id,
        dedupe_key: turn.dedupe_key,
      });
      return;
    }

    const turnPayload = {
      session_id: payload.session_id,
      turn_id: turn.turn_id,
      dedupe_key: turn.dedupe_key,
      cwd,
      transcript_path: payload.transcript_path,
      previous_turn_snapshot: sessionState.previous_turn_snapshot || null,
      last_assistant_message: turn.last_agent_message || payload.last_assistant_message || null,
      items: turn.items,
      user_messages: turn.user_messages,
      assistant_messages: turn.assistant_messages,
    };

    const extracted = runExtractor(turnPayload, cwd, stateDir);
    if (!extracted) {
      return;
    }

    updateSessionState(stateDir, payload.session_id, {
      last_processed_turn_id: turn.dedupe_key,
      last_processed_at: new Date().toISOString(),
    });

    if (extracted.turn_kind === "pleasantry") {
      appendJobEvent(stateDir, "job_noop", {
        job_path: jobPath,
        session_id: payload.session_id,
        dedupe_key: turn.dedupe_key,
      });
      return;
    }

    const summary = formatSummary(extracted, turnPayload);
    if (!summary.length) {
      appendJobEvent(stateDir, "job_empty_summary", {
        job_path: jobPath,
        session_id: payload.session_id,
        dedupe_key: turn.dedupe_key,
      });
      return;
    }

    updateSessionState(stateDir, payload.session_id, {
      previous_turn_snapshot: buildTurnSnapshot(extracted, summary),
    });

    const metadata = {
      agent: "codex",
      source: "codex_hook",
      extractor: "codex_exec",
      session_id: payload.session_id,
      turn_id: turn.turn_id,
      transcript_path: payload.transcript_path,
      previous_turn_snapshot_used: Boolean(turnPayload.previous_turn_snapshot),
      turn_kind: extracted.turn_kind || null,
      user_intent: extracted.user_intent || null,
      agent_intent: extracted.agent_intent || null,
      key_actions: compactList(extracted.key_actions),
      tradeoffs: compactList(extracted.tradeoffs),
      open_questions: compactList(extracted.open_questions),
    };

    const dedupeBase = payload.session_id + ":" + (turn.turn_id || turn.dedupe_key);
    const idempotencyKey = "codex-hook-" + createHash("sha1").update(dedupeBase).digest("hex");
    const ingest = ingestMemory(cwd, project.target, summary, idempotencyKey, metadata);

    if (ingest.status !== 0) {
      if (isRetryableIngestFailure(ingest)) {
        queueRetry(stateDir, {
          cwd,
          target: project.target,
          summary,
          metadata,
          idempotency_key: idempotencyKey,
          session_id: payload.session_id,
          dedupe_key: turn.dedupe_key,
          turn_key: turn.turn_id || turn.dedupe_key,
        }, ingest);
        return;
      }
      appendError(stateDir, "Recallstack ingest failed from Codex Stop worker.", {
        status: ingest.status,
        stdout: truncate(ingest.stdout || "", 1200),
        stderr: truncate(ingest.stderr || "", 1200),
        target: project.target,
        turn_id: turn.turn_id || turn.dedupe_key,
        job_path: jobPath,
      });
      return;
    }

    markCodexIngested(stateDir, payload.session_id, turn.turn_id || turn.dedupe_key);
    appendJobEvent(stateDir, "job_ingested", {
      job_path: jobPath,
      session_id: payload.session_id,
      target: project.target,
      dedupe_key: turn.dedupe_key,
    });
  } finally {
    rmSync(jobPath, { force: true });
  }
}

async function main() {
  if (process.argv[2] === "worker-stop" && typeof process.argv[3] === "string") {
    await processStopJob(process.argv[3]);
    return;
  }
  if (process.argv[2] === "worker-retry" && typeof process.argv[3] === "string") {
    await processRetryJob(process.argv[3]);
    return;
  }

  const raw = await readStdin();
  const payload = readJsonFromString(raw, {});
  const cwd = typeof payload.cwd === "string" && payload.cwd.length ? payload.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  if (payload.hook_event_name === "SessionStart") {
    recordSessionStart(stateDir, payload);
    return;
  }

  if (payload.hook_event_name === "Stop") {
    enqueueStopJob(cwd, stateDir, payload);
  }
}

main().catch((error) => {
  const cwd = process.cwd();
  appendError(stateDirForCwd(cwd), "Unhandled Codex hook failure.", {
    error: error instanceof Error ? error.stack || error.message : String(error),
  });
  process.exit(0);
});
`;

export const CODEX_SKILL_TEMPLATE = `---
name: recallstack-memory
description: "MANDATORY: Always validate the Recallstack target. Query memory only when history, rationale, prior work, or non-local context makes Recallstack the right source. Codex hooks ingest turns asynchronously."
---

# Recallstack Memory (Codex Local)

Mandatory rule:

- Always validate the Recallstack target before work.
- Do not query memory by reflex just because a new turn started.
- Use memory only when it is the right source of truth for the user's intent.
- Codex hooks handle async end-of-turn ingest automatically. Do not narrate or manually run turn ingest unless the user explicitly asks for a manual Recallstack action.
- Keep Recallstack operations in the background. Do not narrate turn ids, query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer.

Required setup (once per repository):

1. Authenticate globally:
   \`recallstack login <CODE>\`
2. Set workspace context for this repo:
   \`recallstack workspace use\`
3. Set non-global default project for this repo:
   \`recallstack project use\`
4. Verify current target:
   \`recallstack project\`
5. Optional inline override per command:
   add \`--project <projectSlug|workspaceSlug/projectSlug>\`

Setup validation gate (run before any task work):

1. Confirm this repository target is configured:
   \`recallstack project\`
   Expected shape (values vary):
   \`{"workspace_config_path":"<repo>/.recallstack/workspace.json","project":"<workspaceSlug>/<projectSlug>","target":"<workspaceSlug>/<projectSlug>"}\`
2. If \`recallstack project\` fails or required fields are null, diagnose runtime config:
   \`recallstack config\`
   Expected shape (values vary):
   \`{"effective_base_url":"...","active_workspace":{"id":"...","slug":"..."},"configured_workspace":{"id":"...","slug":"..."}}\`
3. If \`recallstack project\` fails or required fields are null, confirm signed-in identity:
   \`recallstack whoami\`
   Expected shape (values vary):
   \`{"id":"...","email":"...","auth_type":"jwt","workspace":{"id":"...","kind":"PERSONAL|TEAM","role":"ADMIN|MEMBER"}}\`
4. If the target is still invalid:
   - run \`recallstack login <CODE>\`
   - run \`recallstack workspace use\`
   - run \`recallstack project use\`
   - rerun \`recallstack project\`
   - only rerun \`recallstack config\` and \`recallstack whoami\` if \`project\` still fails
5. Only continue to task execution after \`recallstack project\` passes with a non-global target.

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - Use the current thread first when the answer is already established in the conversation.
   - Use code, logs, config, tests, and local files for directly verifiable current-state facts.
   - Use Recallstack memory for rationale, prior decisions, historical context, related prior work, recurring issue history, user preferences, or context that is not cheap to verify locally.
2. If memory is the right source, query it directly:
   \`recallstack memory query --query "<state/proof-oriented task>" --mode standard\`
   Add \`--synthesize\` to draft a local answer from the retrieved evidence, or pass \`--worker-model <model>\` to both enable synthesis and choose the local worker model.
3. Use \`mode=quick\` for fast checks, \`standard\` for default recall, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
4. Do not use \`--turn-id\` for Codex retrieval. Codex hooks own turn ingestion and there is no manual query/ingest turn pairing to preserve.
5. Raw writes to \`global\` are forbidden. Ensure the configured target is non-global.

Codex hook behavior (installed by \`recallstack agent install codex\`):

1. \`SessionStart\` records session metadata for the current repo.
2. \`Stop\` immediately queues background processing and returns without delaying the thread.
3. The background worker reads the latest completed turn from the Codex transcript.
4. The worker runs an ephemeral \`codex exec --ephemeral --disable codex_hooks\` extraction pass to turn the transcript slice into a Recallstack-ready summary.
5. Set the background extraction model by reinstalling with \`recallstack agent install codex --worker-model <model>\`.
6. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours.
7. The worker ingests that processed summary into the selected non-global project.
8. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance (mandatory):

1. Codex hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s processing pass is responsible for condensing actions and consequences into retrievable memory.
4. Use \`memory source ingest\` (MCP: \`memory_ingest_source\`) for durable artifacts the user is likely to want again: plans, handovers, specs, checklists, research notes, copied docs, meeting notes, or verified issue-remedy runbooks.
5. Durable ingest is the default when the user says \`remember this\`, \`store this\`, \`save this\`, or asks you to write/update a plan, handover, checklist, spec, or long-lived project note.
6. Use \`memory_query\` only when the question is about history, rationale, prior related work, recurring issue history, user preference history, or context that is not cheaply verifiable in the repo. Keep \`memory_query_direct\` for exact durable passages.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests. Example: \`What auth options have we today?\` should usually be answered from the repo, not memory.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer queries like \`what already exists for X\`, \`find prior related work on X\`, \`why did we choose X\`, or \`is X already implemented\`. Avoid queries like \`how should we improve X\`, \`add X\`, \`implement Y\`, or \`fix Z\`.
9. Use \`memory_query_direct\` when exact durable-source passages matter, when the user asks for an existing plan/handover/spec/checklist, or before drafting a new durable artifact if prior documentation may already exist.
10. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search. Use \`memory_query\` first, then \`memory_query_direct\` if you need exact durable passages.
11. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
12. Use \`memory_get_source\` when excerpt-level results are insufficient, or when the user asks for the full document/body.
13. Example split:
   - \`Why did we move away from OOP to functional programming in TypeScript?\` -> query memory.
   - \`What auth options have we today?\` -> inspect code first.
14. Do not pollute the thread with memory activity. Recallstack use should usually be silent. Only mention it when the user explicitly asks, the operation fails, or the retrieved memory materially affects the answer. If you must mention it, keep it to one short sentence.
`;

export const CLAUDE_SKILL_TEMPLATE = `# Recallstack Memory (Claude Code Local)

Required setup (once per repository):

1. Authenticate globally:
   \`recallstack login <CODE>\`
2. Set workspace context for this repo:
   \`recallstack workspace use\`
3. Set non-global default project for this repo:
   \`recallstack project use\`
4. Verify current target:
   \`recallstack project\`
5. Optional inline override per command:
   add \`--project <projectSlug|workspaceSlug/projectSlug>\`

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - use the current thread for already-established answers
   - inspect code, logs, config, and tests for directly verifiable current-state facts
   - query Recallstack for rationale, prior decisions, historical context, related prior work, recurring issue history, user preference history, or non-local context
   - keep Recallstack operations in the background and avoid narrating turn ids, query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer
2. If memory is the right source, query it directly:
   \`recallstack memory query --query "<state/proof-oriented task>" --mode standard\`
   Add \`--synthesize\` to draft a local answer from the retrieved evidence, or pass \`--worker-model <model>\` to both enable synthesis and choose the local worker model.
3. Use \`mode=quick\` for fast checks, \`standard\` for default recall, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
4. Do not use \`--turn-id\` for Claude retrieval. Claude hooks own turn ingestion and there is no manual query/ingest pairing to preserve.
5. Raw writes to \`global\` are forbidden. Ensure the configured target is non-global.

Claude hook behavior (installed by \`recallstack agent install claude\`):

1. The Claude \`Stop\` hook immediately queues a background worker and returns without delaying the main interaction loop.
2. The background worker reads the latest completed turn from the Claude transcript and current Stop payload.
3. The worker runs a headless \`claude -p --output-format json --json-schema ... --no-session-persistence\` extraction pass to produce a clean Recallstack summary without polluting the user's normal thread history.
4. Set the background extraction model by reinstalling with \`recallstack agent install claude --worker-model <model>\`.
5. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours.
6. The hook ingests that processed summary into the selected non-global project.
7. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance:

1. Claude hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s postprocessing pass is responsible for condensing actions and consequences into retrievable memory.
4. Durable context must use \`recallstack memory source ingest\` (MCP: \`memory_ingest_source\`) for plans, handovers, specs, checklists, research notes, copied docs, meeting notes, and verified issue-remedy runbooks.
5. Durable ingest is the default when the user says \`remember this\`, \`store this\`, \`save this\`, or asks you to write/update a long-lived plan, handover, checklist, spec, or reference note.
6. Use \`memory_query\` for blended recall and \`memory_query_direct\` for exact source passages or existing durable artifacts only when memory is the right source. Prefer \`mode=standard\` by default, \`quick\` for low-latency checks, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer \`what already exists for X\`, \`find prior related work on X\`, \`why did we choose X\`, or \`is X already implemented\`.
9. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search.
10. Check durable memory before drafting a new plan/spec/handover if prior documentation may already exist.
11. After a remedy is verified and likely reusable, ingest it durably as a concise issue-remedy note with a searchable title and metadata.
12. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
13. Use \`memory_get_source\` for full durable-source body drilldown.
14. Do not pollute the thread with memory activity. Recallstack use should usually be silent. If memory activity must be mentioned, keep it to one short sentence.
`;

export const CURSOR_HOOK_TEMPLATE = `#!/usr/bin/env node
import { appendFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";

const RECALLSTACK_BIN = process.env.RECALLSTACK_BIN || "recallstack";

function ensureDir(dirPath) {
  mkdirSync(dirPath, { recursive: true });
}

function readJsonFile(filePath, fallback) {
  if (!existsSync(filePath)) return fallback;
  try {
    return JSON.parse(readFileSync(filePath, "utf8"));
  } catch {
    return fallback;
  }
}

function readJsonFromString(raw, fallback) {
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

function writeJsonFile(filePath, value) {
  ensureDir(dirname(filePath));
  writeFileSync(filePath, JSON.stringify(value, null, 2) + "\\n", "utf8");
}

function appendLog(stateDir, fileName, entry) {
  ensureDir(stateDir);
  appendFileSync(join(stateDir, fileName), JSON.stringify({
    timestamp: new Date().toISOString(),
    ...entry,
  }) + "\\n", "utf8");
}

function appendEvent(stateDir, event, extra) {
  appendLog(stateDir, "events.log", { event, extra: extra || null });
}

function appendError(stateDir, message, extra) {
  appendLog(stateDir, "errors.log", { message, extra: extra || null });
}

function readStdin() {
  return new Promise((resolve) => {
    let out = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => {
      out += chunk;
    });
    process.stdin.on("end", () => resolve(out));
  });
}

function run(command, args, options) {
  return spawnSync(command, args, {
    encoding: "utf8",
    stdio: ["pipe", "pipe", "pipe"],
    ...options,
  });
}

function truncate(value, limit) {
  if (typeof value !== "string") return "";
  const normalized = value.trim();
  if (normalized.length <= limit) return normalized;
  return normalized.slice(0, limit - 3) + "...";
}

function parsePositiveInt(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

const RETRY_WINDOW_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_WINDOW_MS, 24 * 60 * 60 * 1000);
const RETRY_BASE_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_BASE_MS, 30 * 1000);
const RETRY_MAX_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_MAX_MS, 15 * 60 * 1000);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function stateDirForCwd(cwd) {
  return join(cwd, ".recallstack", "cursor-hooks");
}

function retryDirForState(stateDir) {
  return join(stateDir, "retry");
}

function retryDelayMs(attemptCount) {
  return Math.min(RETRY_MAX_DELAY_MS, RETRY_BASE_DELAY_MS * Math.pow(2, Math.max(0, attemptCount - 1)));
}

function isRetryableIngestFailure(result) {
  const combined = String(result.stdout || "") + "\\n" + String(result.stderr || "");
  return /(429|too many requests|rate limit|rate-limit|service unavailable|temporarily unavailable|bad gateway|gateway timeout|upstream|fetch failed|network error|socket hang up|connection refused|econnrefused|enotfound|ehostunreach|etimedout|timed out|timeout|5\\d\\d)/i.test(combined);
}

function conversationStatePath(stateDir, conversationId) {
  return join(stateDir, "conversations", conversationId + ".json");
}

function updateConversationState(stateDir, conversationId, updates) {
  const filePath = conversationStatePath(stateDir, conversationId);
  const current = readJsonFile(filePath, {});
  writeJsonFile(filePath, {
    ...current,
    ...updates,
    updated_at: new Date().toISOString(),
  });
}

function readConversationState(stateDir, conversationId) {
  return readJsonFile(conversationStatePath(stateDir, conversationId), {});
}

function extractWorkspaceCwd(payload) {
  if (Array.isArray(payload.workspace_roots) && payload.workspace_roots.length > 0) {
    const first = payload.workspace_roots.find((entry) => typeof entry === "string" && entry.length);
    if (typeof first === "string" && first.length) {
      return first;
    }
  }
  return process.cwd();
}

function extractPrompt(payload) {
  if (typeof payload.prompt === "string" && payload.prompt.trim().length) {
    return payload.prompt.trim();
  }
  if (typeof payload.text === "string" && payload.text.trim().length) {
    return payload.text.trim();
  }
  return "";
}

function extractResponse(payload) {
  const candidates = [
    payload.text,
    payload.response,
    payload.agent_message,
    payload.message,
  ];
  for (const candidate of candidates) {
    if (typeof candidate === "string" && candidate.trim().length) {
      return candidate.trim();
    }
  }
  return "";
}

function resolveProjectTarget(cwd, stateDir) {
  const result = run(RECALLSTACK_BIN, ["project", "--cwd", cwd], { cwd });
  if (result.status !== 0) {
    appendError(stateDir, "Failed to resolve Recallstack target for Cursor hook.", {
      stdout: truncate(result.stdout || "", 800),
      stderr: truncate(result.stderr || "", 800),
    });
    return null;
  }

  const parsed = readJsonFromString(result.stdout, null);
  if (!parsed || typeof parsed.target !== "string" || !parsed.target.length || parsed.project_slug === "global") {
    return null;
  }
  return parsed;
}

function compactText(value, limit) {
  if (typeof value !== "string") return "";
  const normalized = value.replace(/\\s+/g, " ").trim();
  if (normalized.length <= limit) return normalized;
  return normalized.slice(0, limit - 3) + "...";
}

function isPleasantryOnlyPrompt(prompt) {
  return /^(thanks|thank you|thx|got it(?:,? thanks)?|okay,? thanks|ok,? thanks|cool,? thanks|nice,? thanks|appreciate it|sounds good,? thanks|perfect,? thanks)[!. ]*$/i.test(compactText(prompt, 400));
}

function isReferentialPrompt(prompt) {
  return /^(let'?s go with|go with|option\\s+\\d+|do that|use that|go ahead|that works|sounds good|let'?s do that|the first one|the second one|the third one|that one|this one|proceed with that)\\b/i.test(compactText(prompt, 400));
}

function resolvePromptIntent(prompt, previousTurnSnapshot) {
  const latest = compactText(prompt, 1600);
  if (!latest.length || !previousTurnSnapshot || !isReferentialPrompt(latest)) {
    return latest;
  }
  const priorIntent = compactText(previousTurnSnapshot.user_intent, 800);
  const priorOutcome = compactText(previousTurnSnapshot.outcome, 800);
  const context = priorIntent || priorOutcome;
  if (!context.length) {
    return latest;
  }
  return compactText(latest + " (referring to previous turn: " + context + ")", 1600);
}

function buildSummary(job) {
  const lines = [];
  const userIntent = resolvePromptIntent(job.prompt, job.previous_turn_snapshot || null);
  if (userIntent) {
    lines.push("User intent: " + userIntent);
  }
  if (job.response) {
    lines.push("Outcome: " + compactText(job.response, 3200));
  }
  return lines.join("\\n").trim();
}

function buildTurnSnapshot(job, summary) {
  return {
    turn_kind: "thin",
    user_intent: resolvePromptIntent(job.prompt, job.previous_turn_snapshot || null),
    agent_intent: "",
    outcome: compactText(job.response, 600),
    summary: compactText(summary, 900),
  };
}

function buildRetryState(retryState, ingest) {
  const now = Date.now();
  const createdAt = typeof retryState.created_at === "string" && retryState.created_at.length
    ? retryState.created_at
    : new Date(now).toISOString();
  const createdAtMs = Date.parse(createdAt);
  const expiresAtMs = Number.isFinite(createdAtMs) ? createdAtMs + RETRY_WINDOW_MS : now + RETRY_WINDOW_MS;
  const attemptCount = typeof retryState.attempt_count === "number" && retryState.attempt_count >= 0
    ? retryState.attempt_count + 1
    : 1;
  return {
    ...retryState,
    created_at: createdAt,
    expires_at: new Date(expiresAtMs).toISOString(),
    attempt_count: attemptCount,
    last_failed_at: new Date(now).toISOString(),
    next_attempt_at: new Date(Math.min(expiresAtMs, now + retryDelayMs(attemptCount))).toISOString(),
    last_error: {
      status: ingest.status,
      stdout: truncate(ingest.stdout || "", 1200),
      stderr: truncate(ingest.stderr || "", 1200),
    },
  };
}

function ingestMemory(cwd, target, summary, idempotencyKey, metadata) {
  return run(
    RECALLSTACK_BIN,
    [
      "memory",
      "ingest",
      "--project",
      target,
      "--stdin",
      "--idempotency-key",
      idempotencyKey,
      "--metadata",
      JSON.stringify(metadata),
    ],
    {
      cwd,
      input: summary,
    },
  );
}

function spawnDetachedRetryWorker(retryPath, cwd, stateDir) {
  const child = spawn(process.execPath, [process.argv[1], "--worker-retry", retryPath], {
    cwd,
    detached: true,
    stdio: "ignore",
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  child.unref();
  appendEvent(stateDir, "retry_worker_spawned", {
    retry_path: retryPath,
    pid: child.pid,
  });
}

function markCursorIngested(stateDir, conversationId, generationId, cwd) {
  if (typeof conversationId !== "string" || !conversationId.length || typeof generationId !== "string" || !generationId.length) {
    return;
  }
  updateConversationState(stateDir, conversationId, {
    cwd,
    last_ingested_generation_id: generationId,
    last_ingested_at: new Date().toISOString(),
  });
}

function queueRetry(stateDir, retryState, ingest) {
  const retryId = typeof retryState.retry_id === "string" && retryState.retry_id.length
    ? retryState.retry_id
    : createHash("sha1").update(String(retryState.idempotency_key || Date.now())).digest("hex");
  const retryPath = join(retryDirForState(stateDir), retryId + ".json");
  const nextState = buildRetryState({
    ...retryState,
    retry_id: retryId,
  }, ingest);
  writeJsonFile(retryPath, nextState);
  appendEvent(stateDir, "retry_queued", {
    retry_path: retryPath,
    conversation_id: nextState.conversation_id || null,
    generation_id: nextState.generation_id || null,
    next_attempt_at: nextState.next_attempt_at,
  });
  spawnDetachedRetryWorker(retryPath, nextState.cwd || process.cwd(), stateDir);
}

async function processRetryJob(retryPath) {
  let retryState = readJsonFile(retryPath, null);
  if (!retryState || typeof retryState !== "object") {
    return;
  }

  const cwd = typeof retryState.cwd === "string" && retryState.cwd.length ? retryState.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  while (true) {
    retryState = readJsonFile(retryPath, retryState);
    if (!retryState || typeof retryState !== "object") {
      return;
    }

    const expiresAtMs = Date.parse(typeof retryState.expires_at === "string" ? retryState.expires_at : "");
    const now = Date.now();
    if (Number.isFinite(expiresAtMs) && now >= expiresAtMs) {
      appendEvent(stateDir, "retry_expired", {
        retry_path: retryPath,
        conversation_id: retryState.conversation_id || null,
        generation_id: retryState.generation_id || null,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextAttemptAtMs = Date.parse(typeof retryState.next_attempt_at === "string" ? retryState.next_attempt_at : "");
    if (Number.isFinite(nextAttemptAtMs) && nextAttemptAtMs > now) {
      await sleep(nextAttemptAtMs - now);
      continue;
    }

    if (typeof retryState.target !== "string" || !retryState.target.length || typeof retryState.summary !== "string" || !retryState.summary.length || typeof retryState.idempotency_key !== "string" || !retryState.idempotency_key.length) {
      appendError(stateDir, "Cursor retry job is missing ingest payload.", {
        retry_path: retryPath,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const ingest = ingestMemory(cwd, retryState.target, retryState.summary, retryState.idempotency_key, retryState.metadata || {});
    if (ingest.status === 0) {
      markCursorIngested(stateDir, retryState.conversation_id, retryState.generation_id, cwd);
      appendEvent(stateDir, "retry_ingested", {
        retry_path: retryPath,
        conversation_id: retryState.conversation_id || null,
        generation_id: retryState.generation_id || null,
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    if (!isRetryableIngestFailure(ingest)) {
      appendError(stateDir, "Cursor retry abandoned after non-retryable ingest failure.", {
        retry_path: retryPath,
        status: ingest.status,
        stdout: truncate(ingest.stdout || "", 1200),
        stderr: truncate(ingest.stderr || "", 1200),
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextState = buildRetryState(retryState, ingest);
    writeJsonFile(retryPath, nextState);
    appendEvent(stateDir, "retry_rescheduled", {
      retry_path: retryPath,
      conversation_id: nextState.conversation_id || null,
      generation_id: nextState.generation_id || null,
      attempt_count: nextState.attempt_count,
      next_attempt_at: nextState.next_attempt_at,
    });
    await sleep(retryDelayMs(nextState.attempt_count));
  }
}

function enqueueStopJob(cwd, stateDir, payload) {
  const conversationId = typeof payload.conversation_id === "string" ? payload.conversation_id : "";
  const generationId = typeof payload.generation_id === "string" ? payload.generation_id : "";
  if (!conversationId.length) {
    return;
  }

  const sessionState = readConversationState(stateDir, conversationId);
  const dedupeKey = generationId || sessionState.last_response_generation_id || sessionState.last_prompt_generation_id || conversationId;
  if (sessionState.last_ingested_generation_id === dedupeKey || sessionState.last_queued_generation_id === dedupeKey) {
    appendEvent(stateDir, "stop_duplicate_skipped", {
      conversation_id: conversationId,
      generation_id: dedupeKey,
    });
    return;
  }

  const prompt = typeof sessionState.last_prompt === "string" ? sessionState.last_prompt : "";
  const response = typeof sessionState.last_response === "string" ? sessionState.last_response : "";
  if (!prompt.length && !response.length) {
    appendEvent(stateDir, "stop_empty_state", {
      conversation_id: conversationId,
      generation_id: dedupeKey,
    });
    return;
  }

  const jobsDir = join(stateDir, "jobs");
  ensureDir(jobsDir);
  const jobId = createHash("sha1")
    .update(String(conversationId) + ":" + String(dedupeKey) + ":" + String(Date.now()))
    .digest("hex");
  const jobPath = join(jobsDir, jobId + ".json");
  writeJsonFile(jobPath, {
    cwd,
    conversation_id: conversationId,
    generation_id: dedupeKey,
    prompt,
    response,
    previous_turn_snapshot: sessionState.previous_turn_snapshot || null,
    raw_payload: payload,
  });

  const child = spawn(process.execPath, [process.argv[1], "--worker", jobPath], {
    cwd,
    detached: true,
    stdio: "ignore",
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  child.unref();

  updateConversationState(stateDir, conversationId, {
    cwd,
    last_queued_generation_id: dedupeKey,
    last_queued_at: new Date().toISOString(),
  });
  appendEvent(stateDir, "job_spawned", {
    conversation_id: conversationId,
    generation_id: dedupeKey,
    job_path: jobPath,
  });
}

function runWorker(jobPath) {
  const job = readJsonFile(jobPath, null);
  if (!job || typeof job !== "object") {
    return;
  }

  const cwd = typeof job.cwd === "string" && job.cwd.length ? job.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  const project = resolveProjectTarget(cwd, stateDir);
  if (!project) {
    return;
  }

  if (isPleasantryOnlyPrompt(job.prompt || "")) {
    appendEvent(stateDir, "job_noop", {
      job_path: jobPath,
      conversation_id: job.conversation_id || null,
      generation_id: job.generation_id || null,
    });
    rmSync(jobPath, { force: true });
    return;
  }

  const summary = buildSummary(job);
  if (!summary.length) {
    appendEvent(stateDir, "job_empty_summary", {
      job_path: jobPath,
      conversation_id: job.conversation_id || null,
      generation_id: job.generation_id || null,
    });
    rmSync(jobPath, { force: true });
    return;
  }

  updateConversationState(stateDir, typeof job.conversation_id === "string" ? job.conversation_id : "", {
    previous_turn_snapshot: buildTurnSnapshot(job, summary),
  });

  const metadata = {
    agent: "cursor_ide",
    source: "cursor_stop_hook",
    extractor: "cursor_hook_pairing",
    conversation_id: typeof job.conversation_id === "string" ? job.conversation_id : null,
    generation_id: typeof job.generation_id === "string" ? job.generation_id : null,
    previous_turn_snapshot_used: Boolean(job.previous_turn_snapshot),
    prompt: typeof job.prompt === "string" ? compactText(job.prompt, 4000) : null,
  };

  const dedupeKey = typeof job.generation_id === "string" && job.generation_id.length
    ? job.generation_id
    : typeof job.conversation_id === "string"
      ? job.conversation_id
      : createHash("sha1").update(summary).digest("hex");

  const idempotencyKey = "cursor-hook-" + createHash("sha1").update(String(job.conversation_id) + ":" + dedupeKey).digest("hex");
  const ingest = ingestMemory(cwd, project.target, summary, idempotencyKey, metadata);

  if (ingest.status !== 0) {
    if (isRetryableIngestFailure(ingest)) {
      queueRetry(stateDir, {
        cwd,
        target: project.target,
        summary,
        metadata,
        idempotency_key: idempotencyKey,
        conversation_id: typeof job.conversation_id === "string" ? job.conversation_id : null,
        generation_id: dedupeKey,
      }, ingest);
      return;
    }
    appendError(stateDir, "Recallstack ingest failed from Cursor Stop hook.", {
      status: ingest.status,
      stdout: truncate(ingest.stdout || "", 1200),
      stderr: truncate(ingest.stderr || "", 1200),
      target: project.target,
      conversation_id: job.conversation_id || null,
      generation_id: dedupeKey,
      job_path: jobPath,
    });
    return;
  }

  markCursorIngested(stateDir, typeof job.conversation_id === "string" ? job.conversation_id : "", dedupeKey, cwd);
  appendEvent(stateDir, "job_ingested", {
    conversation_id: job.conversation_id || null,
    generation_id: dedupeKey,
    target: project.target,
    job_path: jobPath,
  });
  rmSync(jobPath, { force: true });
}

async function main() {
  if (process.argv[2] === "--worker") {
    const jobPath = process.argv[3];
    if (typeof jobPath === "string" && jobPath.length) {
      runWorker(jobPath);
    }
    return;
  }
  if (process.argv[2] === "--worker-retry") {
    const retryPath = process.argv[3];
    if (typeof retryPath === "string" && retryPath.length) {
      await processRetryJob(retryPath);
    }
    return;
  }
  if (process.env.RECALLSTACK_HOOK_ACTIVE === "1") {
    return;
  }

  const raw = await readStdin();
  const payload = readJsonFromString(raw, {});
  const eventName = typeof payload.hook_event_name === "string" ? payload.hook_event_name : "";
  const cwd = extractWorkspaceCwd(payload);
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  if (eventName === "beforeSubmitPrompt") {
    const conversationId = typeof payload.conversation_id === "string" ? payload.conversation_id : "";
    const prompt = extractPrompt(payload);
    if (conversationId.length && prompt.length) {
      updateConversationState(stateDir, conversationId, {
        cwd,
        last_prompt: prompt,
        last_prompt_generation_id: typeof payload.generation_id === "string" ? payload.generation_id : null,
        last_prompt_at: new Date().toISOString(),
      });
      appendEvent(stateDir, "prompt_captured", {
        conversation_id: conversationId,
        generation_id: typeof payload.generation_id === "string" ? payload.generation_id : null,
      });
    }
    return;
  }

  if (eventName === "afterAgentResponse") {
    const conversationId = typeof payload.conversation_id === "string" ? payload.conversation_id : "";
    const response = extractResponse(payload);
    if (conversationId.length && response.length) {
      updateConversationState(stateDir, conversationId, {
        cwd,
        last_response: response,
        last_response_generation_id: typeof payload.generation_id === "string" ? payload.generation_id : null,
        last_response_at: new Date().toISOString(),
      });
      appendEvent(stateDir, "response_captured", {
        conversation_id: conversationId,
        generation_id: typeof payload.generation_id === "string" ? payload.generation_id : null,
      });
    }
    return;
  }

  if (eventName === "stop") {
    enqueueStopJob(cwd, stateDir, payload);
  }
}

main().catch((error) => {
  const stateDir = stateDirForCwd(process.cwd());
  appendError(stateDir, "Unhandled Cursor hook failure.", {
    error: error instanceof Error ? error.stack || error.message : String(error),
  });
  process.exit(0);
});
`;

export const CURSOR_RULE_TEMPLATE = `# Recallstack Memory (Cursor IDE Local)

Required setup (once per repository):

1. \`recallstack login <CODE>\`
2. \`recallstack workspace use\`
3. \`recallstack project use\`
4. \`recallstack project\` (verify target)
5. Optional per-command override:
   add \`--project <projectSlug|workspaceSlug/projectSlug>\`

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - use the current thread for already-established answers
   - inspect code, logs, config, and tests for directly verifiable current-state facts
   - query Recallstack for rationale, prior decisions, historical context, related prior work, recurring issue history, user preference history, or non-local context
   - keep Recallstack operations in the background and avoid narrating query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer
2. If memory is the right source, call:
   \`recallstack memory query --query "<state/proof-oriented task>" --mode standard\`
   Add \`--synthesize\` to draft a local answer from the retrieved evidence, or pass \`--worker-model <model>\` to both enable synthesis and choose the local worker model.
3. Use \`mode=quick\` for fast checks, \`standard\` for default recall, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
4. Do not use \`--turn-id\` for Cursor IDE retrieval. Cursor IDE hooks own turn ingestion and there is no manual query/ingest pairing to preserve.
5. Raw writes to \`global\` are forbidden. Ensure the configured target is non-global.

Cursor IDE hook behavior (installed by \`recallstack agent install cursor\`):

1. Cursor IDE installs repo-local hooks through \`.cursor/hooks.json\`. This integration targets the IDE only, not Cursor CLI.
2. \`beforeSubmitPrompt\` captures the latest user prompt.
3. \`afterAgentResponse\` captures the latest agent response text.
4. \`stop\` queues background processing and returns immediately so editor interaction is not delayed.
5. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours.
6. The background worker pairs the captured prompt and response, condenses them into a clean turn memory, and ingests that summary into the selected non-global project.
7. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance:

1. Cursor IDE hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s postprocessing pass is responsible for condensing actions and consequences into retrievable memory.
4. Use \`recallstack memory source ingest\` for durable plans, handovers, specs, checklists, research notes, copied docs, meeting notes, and verified issue-remedy runbooks.
5. Durable ingest is the default when the user says \`remember this\`, \`store this\`, \`save this\`, or asks for a long-lived plan, handover, checklist, spec, or reference note.
6. Use \`memory_query\` for blended recall only when the question is about history, rationale, prior related work, recurring issue history, user preference history, or non-local context. Prefer \`mode=standard\` by default, \`quick\` for low-latency checks, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer \`what already exists for X\`, \`find prior related work on X\`, \`why did we choose X\`, or \`is X already implemented\`.
9. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search.
10. Use \`memory_query_direct\` for exact durable-source passages or existing durable artifacts.
11. Check durable memory before drafting a new durable artifact if prior documentation may already exist.
12. After a remedy is verified and likely reusable, ingest it durably as a concise issue-remedy note with a searchable title and metadata.
13. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
14. Use \`memory_get_source\` for full-body drilldown.
15. Do not pollute the thread with memory activity. Recallstack use should usually be silent. If memory activity must be mentioned, keep it to one short sentence.
16. Do not rely on Cursor CLI for this integration. The supported path is Cursor IDE hooks only.
`;

export const COPILOT_HOOK_TEMPLATE = `#!/usr/bin/env node
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";

const RECALLSTACK_BIN = process.env.RECALLSTACK_BIN || "recallstack";
const COPILOT_BIN = process.env.COPILOT_BIN || "copilot";
const EXTRACT_SCHEMA = {
  type: "object",
  additionalProperties: false,
  properties: {
    turn_kind: { type: "string", enum: ["substantial", "thin", "pleasantry"] },
    user_intent: { type: "string" },
    agent_intent: { type: "string" },
    key_actions: { type: "array", items: { type: "string" } },
    tradeoffs: { type: "array", items: { type: "string" } },
    outcome: { type: "string" },
    open_questions: { type: "array", items: { type: "string" } }
  },
  required: [
    "turn_kind",
    "user_intent",
    "agent_intent",
    "key_actions",
    "tradeoffs",
    "outcome",
    "open_questions"
  ]
};

function ensureDir(dirPath) {
  mkdirSync(dirPath, { recursive: true });
}

function readJsonFile(filePath, fallback) {
  if (!existsSync(filePath)) return fallback;
  try {
    return JSON.parse(readFileSync(filePath, "utf8"));
  } catch {
    return fallback;
  }
}

function readJsonFromString(raw, fallback) {
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

function writeJsonFile(filePath, value) {
  ensureDir(dirname(filePath));
  writeFileSync(filePath, JSON.stringify(value, null, 2) + "\\n", "utf8");
}

function appendLog(stateDir, fileName, entry) {
  ensureDir(stateDir);
  appendFileSync(join(stateDir, fileName), JSON.stringify({
    timestamp: new Date().toISOString(),
    ...entry,
  }) + "\\n", "utf8");
}

function appendEvent(stateDir, event, extra) {
  appendLog(stateDir, "events.log", { event, extra: extra || null });
}

function appendError(stateDir, message, extra) {
  appendLog(stateDir, "errors.log", { message, extra: extra || null });
}

function readStdin() {
  return new Promise((resolve) => {
    let out = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => {
      out += chunk;
    });
    process.stdin.on("end", () => resolve(out));
  });
}

function run(command, args, options) {
  return spawnSync(command, args, {
    encoding: "utf8",
    stdio: ["pipe", "pipe", "pipe"],
    ...options,
  });
}

function truncate(value, limit) {
  if (typeof value !== "string") return "";
  const normalized = value.trim();
  if (normalized.length <= limit) return normalized;
  return normalized.slice(0, limit - 3) + "...";
}

function parsePositiveInt(value, fallback) {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

const RETRY_WINDOW_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_WINDOW_MS, 24 * 60 * 60 * 1000);
const RETRY_BASE_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_BASE_MS, 30 * 1000);
const RETRY_MAX_DELAY_MS = parsePositiveInt(process.env.RECALLSTACK_INGEST_RETRY_MAX_MS, 15 * 60 * 1000);

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function stateDirForCwd(cwd) {
  return join(cwd, ".recallstack", "copilot-hooks");
}

function retryDirForState(stateDir) {
  return join(stateDir, "retry");
}

function retryDelayMs(attemptCount) {
  return Math.min(RETRY_MAX_DELAY_MS, RETRY_BASE_DELAY_MS * Math.pow(2, Math.max(0, attemptCount - 1)));
}

function isRetryableIngestFailure(result) {
  const combined = String(result.stdout || "") + "\\n" + String(result.stderr || "");
  return /(429|too many requests|rate limit|rate-limit|service unavailable|temporarily unavailable|bad gateway|gateway timeout|upstream|fetch failed|network error|socket hang up|connection refused|econnrefused|enotfound|ehostunreach|etimedout|timed out|timeout|5\\d\\d)/i.test(combined);
}

function resolveWorkerModel(cwd) {
  const envModel = typeof process.env.RECALLSTACK_COPILOT_WORKER_MODEL === "string"
    ? process.env.RECALLSTACK_COPILOT_WORKER_MODEL.trim()
    : typeof process.env.RECALLSTACK_WORKER_MODEL === "string"
      ? process.env.RECALLSTACK_WORKER_MODEL.trim()
      : "";
  if (envModel.length) {
    return envModel;
  }

  const settings = readJsonFile(join(cwd, ".recallstack", "agent-settings.json"), {});
  const configured = settings && typeof settings === "object" && settings.copilot && typeof settings.copilot === "object"
    ? settings.copilot.worker_model
    : "";
  if (typeof configured === "string" && configured.trim().length) {
    return configured.trim();
  }

  const globalSettings = readJsonFile(join(homedir(), ".recallstack", "agent-settings.json"), {});
  const globalConfigured = globalSettings && typeof globalSettings === "object" && globalSettings.copilot && typeof globalSettings.copilot === "object"
    ? globalSettings.copilot.worker_model
    : "";
  return typeof globalConfigured === "string" ? globalConfigured.trim() : "";
}

function repoRootForCwd(cwd) {
  const result = run("git", ["rev-parse", "--show-toplevel"], { cwd });
  if (result.status === 0) {
    const root = (result.stdout || "").trim();
    if (root.length) {
      return root;
    }
  }
  return cwd;
}

function sessionStatePath(stateDir, sessionKey) {
  return join(stateDir, "sessions", sessionKey + ".json");
}

function updateSessionState(stateDir, sessionKey, updates) {
  const filePath = sessionStatePath(stateDir, sessionKey);
  const current = readJsonFile(filePath, {});
  writeJsonFile(filePath, {
    ...current,
    ...updates,
    updated_at: new Date().toISOString(),
  });
}

function readSessionState(stateDir, sessionKey) {
  return readJsonFile(sessionStatePath(stateDir, sessionKey), {});
}

function normalizeEventName(raw) {
  if (typeof raw !== "string" || !raw.length) {
    return "";
  }
  const map = {
    SessionStart: "sessionStart",
    UserPromptSubmit: "userPromptSubmitted",
    Stop: "agentStop",
  };
  return map[raw] || raw;
}

function extractPrompt(payload) {
  const candidates = [
    payload.prompt,
    payload.message,
    payload.text,
  ];
  for (const candidate of candidates) {
    if (typeof candidate === "string" && candidate.trim().length) {
      return candidate.trim();
    }
  }
  return "";
}

function isHeadlessPrompt(prompt) {
  return typeof prompt === "string"
    && prompt.startsWith("You are extracting Recallstack turn memory from a completed GitHub Copilot turn.");
}

function extractSessionKey(payload, cwd) {
  const candidates = [
    payload.sessionId,
    payload.session_id,
    payload.conversationId,
    payload.conversation_id,
  ];
  for (const candidate of candidates) {
    if (typeof candidate === "string" && candidate.trim().length) {
      return candidate.trim();
    }
  }
  return createHash("sha1").update(resolve(cwd)).digest("hex");
}

function extractTranscriptPath(payload) {
  const candidates = [
    payload.transcript_path,
    payload.transcriptPath,
  ];
  for (const candidate of candidates) {
    if (typeof candidate === "string" && candidate.trim().length) {
      return candidate.trim();
    }
  }
  return "";
}

function parseJsonLines(filePath) {
  if (!filePath.length || !existsSync(filePath)) {
    return [];
  }
  return readFileSync(filePath, "utf8")
    .split(/\\r?\\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .flatMap((line) => {
      try {
        return [JSON.parse(line)];
      } catch {
        return [];
      }
    });
}

function extractTurnFromEvents(entries, expectedPrompt) {
  const assistantEntries = entries.filter((entry) =>
    entry
    && entry.type === "assistant.message"
    && typeof entry.data?.content === "string"
    && entry.data.content.trim().length > 0
  );
  const lastAssistant = assistantEntries[assistantEntries.length - 1];
  if (!lastAssistant) {
    return null;
  }

  const interactionId = typeof lastAssistant.data?.interactionId === "string" ? lastAssistant.data.interactionId : "";
  const precedingUsers = entries.filter((entry) =>
    entry
    && entry.type === "user.message"
    && typeof entry.data?.content === "string"
    && entry.data.content.trim().length > 0
    && (!interactionId || entry.data?.interactionId === interactionId)
  );
  const lastUser = precedingUsers[precedingUsers.length - 1] || null;

  const userPrompt = lastUser?.data?.content?.trim() || "";
  if (expectedPrompt && userPrompt && expectedPrompt !== userPrompt && !userPrompt.includes(expectedPrompt)) {
    return null;
  }

  const toolNames = assistantEntries
    .flatMap((entry) => Array.isArray(entry.data?.toolRequests) ? entry.data.toolRequests : [])
    .map((tool) => (tool && typeof tool === "object" && typeof tool.name === "string" ? tool.name.trim() : ""))
    .filter(Boolean);

  const assistantText = String(lastAssistant.data?.content || "").trim();
  if (!assistantText.length) {
    return null;
  }

  return {
    prompt: userPrompt,
    response: assistantText,
    tool_names: Array.from(new Set(toolNames)),
    dedupe_key:
      (typeof lastAssistant.data?.messageId === "string" && lastAssistant.data.messageId)
      || (typeof lastAssistant.id === "string" && lastAssistant.id)
      || createHash("sha1").update(assistantText).digest("hex"),
  };
}

function listRecentSessionFiles() {
  const root = join(homedir(), ".copilot", "session-state");
  if (!existsSync(root)) {
    return [];
  }

  const files = [];
  const entries = readdirSync(root, { withFileTypes: true });
  for (const entry of entries) {
    const candidate = join(root, entry.name);
    if (entry.isFile() && entry.name.endsWith(".jsonl")) {
      files.push(candidate);
      continue;
    }
    if (!entry.isDirectory()) {
      continue;
    }
    for (const child of readdirSync(candidate, { withFileTypes: true })) {
      if (child.isFile() && child.name.endsWith(".jsonl")) {
        files.push(join(candidate, child.name));
      }
    }
  }

  return files.sort((a, b) => statSync(b).mtimeMs - statSync(a).mtimeMs);
}

function sessionFileMatchesRepo(entries, repoRoot) {
  const sessionStart = entries.find((entry) => entry && entry.type === "session.start");
  const context = sessionStart?.data?.context;
  const candidateRoot = typeof context?.gitRoot === "string" && context.gitRoot.length
    ? context.gitRoot
    : typeof context?.cwd === "string" && context.cwd.length
      ? context.cwd
      : "";
  if (!candidateRoot.length) {
    return false;
  }
  return resolve(candidateRoot) === resolve(repoRoot);
}

function findLatestCopilotTurn(repoRoot, expectedPrompt) {
  for (const filePath of listRecentSessionFiles().slice(0, 20)) {
    const entries = parseJsonLines(filePath);
    if (!entries.length || !sessionFileMatchesRepo(entries, repoRoot)) {
      continue;
    }
    const turn = extractTurnFromEvents(entries, expectedPrompt);
    if (!turn) {
      continue;
    }
    return {
      ...turn,
      session_file: filePath,
    };
  }
  return null;
}

function findTurnFromPayload(payload, repoRoot, expectedPrompt) {
  const transcriptPath = extractTranscriptPath(payload);
  if (transcriptPath.length) {
    const entries = parseJsonLines(transcriptPath);
    const turn = extractTurnFromEvents(entries, expectedPrompt);
    if (turn) {
      return {
        ...turn,
        session_file: transcriptPath,
      };
    }
  }
  return findLatestCopilotTurn(repoRoot, expectedPrompt);
}

function resolveProjectTarget(cwd, stateDir) {
  const result = run(RECALLSTACK_BIN, ["project", "--cwd", cwd], { cwd });
  if (result.status !== 0) {
    appendError(stateDir, "Failed to resolve Recallstack target for Copilot hook.", {
      stdout: truncate(result.stdout || "", 800),
      stderr: truncate(result.stderr || "", 800),
    });
    return null;
  }

  const parsed = readJsonFromString(result.stdout, null);
  if (!parsed || typeof parsed.target !== "string" || !parsed.target.length || parsed.project_slug === "global") {
    return null;
  }
  return parsed;
}

function compactText(value, limit) {
  if (typeof value !== "string") return "";
  const normalized = value.replace(/\\s+/g, " ").trim();
  if (normalized.length <= limit) return normalized;
  return normalized.slice(0, limit - 3) + "...";
}

function compactList(values) {
  return Array.isArray(values)
    ? values.map((value) => (typeof value === "string" ? value.trim() : "")).filter(Boolean)
    : [];
}

function directUserIntent(turn) {
  const values = compactList(turn && turn.user_messages);
  if (!values.length) {
    return "";
  }
  return compactText(values[values.length - 1], 400);
}

function directAssistantOutcome(turn) {
  const values = compactList(turn && turn.assistant_messages);
  if (values.length) {
    return compactText(values[values.length - 1], 600);
  }
  return "";
}

function fallbackExtractedTurn(turn) {
  const userIntent = directUserIntent(turn);
  const outcome = directAssistantOutcome(turn);
  return {
    turn_kind: isPleasantryOnlyTurn(turn) ? "pleasantry" : "thin",
    user_intent: userIntent,
    agent_intent: outcome.length ? "Respond to the user's latest request and move the work forward." : "",
    key_actions: [],
    tradeoffs: [],
    outcome,
    open_questions: [],
  };
}

function isPleasantryOnlyTurn(turn) {
  const latestUserMessage = directUserIntent(turn);
  if (!latestUserMessage.length) {
    return false;
  }
  return /^(thanks|thank you|thx|got it(?:,? thanks)?|okay,? thanks|ok,? thanks|cool,? thanks|nice,? thanks|appreciate it|sounds good,? thanks|perfect,? thanks)[!. ]*$/i.test(latestUserMessage);
}

function buildTurnSnapshot(extracted, summary) {
  return {
    turn_kind: typeof extracted.turn_kind === "string" ? extracted.turn_kind : "thin",
    user_intent: compactText(extracted.user_intent, 400),
    agent_intent: compactText(extracted.agent_intent, 400),
    outcome: compactText(extracted.outcome, 600),
    summary: compactText(summary, 900),
  };
}

function normalizeExtractedTurn(extracted, turn) {
  if (!extracted || typeof extracted !== "object") {
    return fallbackExtractedTurn(turn);
  }
  const sourceUserMessage = directUserIntent(turn);
  const extractedKind = typeof extracted.turn_kind === "string" ? extracted.turn_kind.trim() : "";
  const normalizedKind = extractedKind === "substantial" || extractedKind === "thin" || extractedKind === "pleasantry"
    ? extractedKind
    : fallbackExtractedTurn(turn).turn_kind;
  const turnKind = isPleasantryOnlyTurn(turn) ? "pleasantry" : normalizedKind === "pleasantry" ? "thin" : normalizedKind;
  return {
    ...extracted,
    turn_kind: turnKind,
    user_intent:
      (() => {
        const extractedIntent = compactText(extracted.user_intent, 400);
        if (!sourceUserMessage.length) {
          return extractedIntent;
        }
        if (extractedIntent.length && !/(capture a recallstack memory|provided codex turn payload|provided claude code turn payload|provided github copilot turn payload|extraction task|hook task|json task|recallstack ingestion task)/i.test(extractedIntent)) {
          return extractedIntent;
        }
        return sourceUserMessage;
      })(),
    agent_intent: compactText(extracted.agent_intent, 400),
    outcome: compactText(extracted.outcome, 600) || directAssistantOutcome(turn),
  };
}

function buildExtractorPrompt(turn) {
  return [
    "You are extracting Recallstack turn memory from a completed GitHub Copilot turn.",
    "Return valid JSON that matches this schema exactly:",
    JSON.stringify(EXTRACT_SCHEMA),
    "Use only the provided turn payload.",
    "The source of truth for user_intent is the latest real user message in turn.user_messages.",
    "If multiple real user messages are present, treat the latest one as the active steering instruction and use earlier user messages only as supporting context.",
    "If the latest user message is referential or underspecified (for example: 'Let's go with Option 1', 'do that', 'go ahead'), you may use turn.previous_turn_snapshot to clarify the intent.",
    "Use at most that one-turn lookback. Do not reconstruct deeper history into user_intent.",
    "Rewrite that message for clarity only. Preserve its meaning, even if the user message is vague or has typos.",
    "Do not describe the extraction task, hook task, JSON task, or Recallstack ingestion task as user_intent.",
    "Capture the project-relevant facts that should be remembered later:",
    "- what the user wanted",
    "- what the agent set out to do",
    "- meaningful actions taken",
    "- tradeoffs or alternatives discussed",
    "- the current outcome or best answer",
    "- open questions or next steps when relevant",
    "Avoid raw terminal noise, ids, timestamps, and low-value housekeeping.",
    "Classify the turn with turn_kind:",
    "- substantial: concrete implementation, decisions, verification, meaningful research, or high-signal project progress",
    "- thin: short but still meaningful steering or confirmation, such as approval to proceed or a brief clarification that changes direction",
    "- pleasantry: pure courtesy/closure like thank you, noted, or you're welcome with no project signal",
    "",
    "TURN PAYLOAD",
    JSON.stringify(turn, null, 2),
  ].join("\\n");
}

function parseCopilotExtractorOutput(raw) {
  const direct = readJsonFromString(raw, null);
  if (direct && typeof direct === "object" && !Array.isArray(direct) && "turn_kind" in direct) {
    return direct;
  }
  return null;
}

function runExtractor(turn, cwd, stateDir) {
  const configDir = join(tmpdir(), "recallstack-copilot-worker-" + createHash("sha1").update(String(Date.now()) + ":" + Math.random()).digest("hex"));
  ensureDir(configDir);
  const workerModel = resolveWorkerModel(cwd);
  const args = [
    "--config-dir",
    configDir,
    "--no-custom-instructions",
    "--allow-all-tools",
    "--output-format",
    "text",
    "--silent",
    "-p",
    buildExtractorPrompt(turn),
  ];
  if (workerModel.length) {
    args.unshift(workerModel);
    args.unshift("--model");
  }

  const result = run(
    COPILOT_BIN,
    args,
    {
      cwd: configDir,
      env: {
        ...process.env,
        RECALLSTACK_HOOK_ACTIVE: "1",
      },
    },
  );

  rmSync(configDir, { recursive: true, force: true });

  if (result.status !== 0) {
    appendError(stateDir, "Copilot extractor failed.", {
      status: result.status,
      stdout: truncate(result.stdout || "", 1200),
      stderr: truncate(result.stderr || "", 1200),
      cwd,
      worker_model: workerModel || null,
    });
    return fallbackExtractedTurn(turn);
  }

  const parsed = normalizeExtractedTurn(parseCopilotExtractorOutput(result.stdout || ""), turn);
  if (!parsed) {
    appendError(stateDir, "Copilot extractor returned unreadable output.", {
      stdout: truncate(result.stdout || "", 1200),
      stderr: truncate(result.stderr || "", 1200),
      worker_model: workerModel || null,
    });
    return fallbackExtractedTurn(turn);
  }
  return parsed;
}

function formatSummary(extracted, turn) {
  const lines = [];
  const turnKind = typeof extracted.turn_kind === "string" ? extracted.turn_kind : "thin";
  if (typeof extracted.user_intent === "string" && extracted.user_intent.trim().length) {
    lines.push("User intent: " + extracted.user_intent.trim());
  }
  if (turnKind === "substantial" && typeof extracted.agent_intent === "string" && extracted.agent_intent.trim().length) {
    lines.push("Agent intent: " + extracted.agent_intent.trim());
  }
  const keyActions = compactList(extracted.key_actions);
  if (turnKind === "substantial" && keyActions.length) {
    lines.push("Actions: " + keyActions.join("; "));
  }
  const tradeoffs = compactList(extracted.tradeoffs);
  if (turnKind === "substantial" && tradeoffs.length) {
    lines.push("Tradeoffs: " + tradeoffs.join("; "));
  }
  if (typeof extracted.outcome === "string" && extracted.outcome.trim().length) {
    lines.push("Outcome: " + extracted.outcome.trim());
  }
  const openQuestions = compactList(extracted.open_questions);
  if (turnKind === "substantial" && openQuestions.length) {
    lines.push("Open questions: " + openQuestions.join("; "));
  }
  if (lines.length) {
    return lines.join("\\n").trim();
  }
  const fallback = fallbackExtractedTurn(turn);
  if (fallback.user_intent.length) {
    lines.push("User intent: " + fallback.user_intent);
  }
  if (fallback.outcome.length) {
    lines.push("Outcome: " + fallback.outcome);
  }
  return lines.join("\\n").trim();
}

function buildRetryState(retryState, ingest) {
  const now = Date.now();
  const createdAt = typeof retryState.created_at === "string" && retryState.created_at.length
    ? retryState.created_at
    : new Date(now).toISOString();
  const createdAtMs = Date.parse(createdAt);
  const expiresAtMs = Number.isFinite(createdAtMs) ? createdAtMs + RETRY_WINDOW_MS : now + RETRY_WINDOW_MS;
  const attemptCount = typeof retryState.attempt_count === "number" && retryState.attempt_count >= 0
    ? retryState.attempt_count + 1
    : 1;
  return {
    ...retryState,
    created_at: createdAt,
    expires_at: new Date(expiresAtMs).toISOString(),
    attempt_count: attemptCount,
    last_failed_at: new Date(now).toISOString(),
    next_attempt_at: new Date(Math.min(expiresAtMs, now + retryDelayMs(attemptCount))).toISOString(),
    last_error: {
      status: ingest.status,
      stdout: truncate(ingest.stdout || "", 1200),
      stderr: truncate(ingest.stderr || "", 1200),
    },
  };
}

function ingestMemory(cwd, target, summary, idempotencyKey, metadata) {
  return run(
    RECALLSTACK_BIN,
    [
      "memory",
      "ingest",
      "--project",
      target,
      "--stdin",
      "--idempotency-key",
      idempotencyKey,
      "--metadata",
      JSON.stringify(metadata),
    ],
    {
      cwd,
      input: summary,
    },
  );
}

function spawnDetachedRetryWorker(retryPath, cwd, stateDir) {
  const child = spawn(process.execPath, [process.argv[1], "--worker-retry", retryPath], {
    cwd,
    detached: true,
    stdio: "ignore",
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  child.unref();
  appendEvent(stateDir, "retry_worker_spawned", {
    retry_path: retryPath,
    pid: child.pid,
  });
}

function markCopilotIngested(stateDir, sessionKey, repoRoot, turn, fallbackPrompt, cwd) {
  if (typeof sessionKey !== "string" || !sessionKey.length || !turn || typeof turn.dedupe_key !== "string" || !turn.dedupe_key.length) {
    return;
  }
  updateSessionState(stateDir, sessionKey, {
    cwd,
    repo_root: repoRoot,
    last_ingested_turn_id: turn.dedupe_key,
    last_ingested_at: new Date().toISOString(),
    last_prompt: typeof turn.prompt === "string" && turn.prompt.length ? turn.prompt : fallbackPrompt || "",
  });
}

function queueRetry(stateDir, retryState, ingest) {
  const retryId = typeof retryState.retry_id === "string" && retryState.retry_id.length
    ? retryState.retry_id
    : createHash("sha1").update(String(retryState.idempotency_key || Date.now())).digest("hex");
  const retryPath = join(retryDirForState(stateDir), retryId + ".json");
  const nextState = buildRetryState({
    ...retryState,
    retry_id: retryId,
  }, ingest);
  writeJsonFile(retryPath, nextState);
  appendEvent(stateDir, "retry_queued", {
    retry_path: retryPath,
    session_key: nextState.session_key || null,
    dedupe_key: nextState.turn?.dedupe_key || null,
    next_attempt_at: nextState.next_attempt_at,
  });
  spawnDetachedRetryWorker(retryPath, nextState.cwd || process.cwd(), stateDir);
}

async function processRetryJob(retryPath) {
  let retryState = readJsonFile(retryPath, null);
  if (!retryState || typeof retryState !== "object") {
    return;
  }

  const cwd = typeof retryState.cwd === "string" && retryState.cwd.length ? retryState.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  while (true) {
    retryState = readJsonFile(retryPath, retryState);
    if (!retryState || typeof retryState !== "object") {
      return;
    }

    const expiresAtMs = Date.parse(typeof retryState.expires_at === "string" ? retryState.expires_at : "");
    const now = Date.now();
    if (Number.isFinite(expiresAtMs) && now >= expiresAtMs) {
      appendEvent(stateDir, "retry_expired", {
        retry_path: retryPath,
        session_key: retryState.session_key || null,
        dedupe_key: retryState.turn?.dedupe_key || null,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextAttemptAtMs = Date.parse(typeof retryState.next_attempt_at === "string" ? retryState.next_attempt_at : "");
    if (Number.isFinite(nextAttemptAtMs) && nextAttemptAtMs > now) {
      await sleep(nextAttemptAtMs - now);
      continue;
    }

    if (typeof retryState.target !== "string" || !retryState.target.length || typeof retryState.summary !== "string" || !retryState.summary.length || typeof retryState.idempotency_key !== "string" || !retryState.idempotency_key.length) {
      appendError(stateDir, "Copilot retry job is missing ingest payload.", {
        retry_path: retryPath,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const ingest = ingestMemory(cwd, retryState.target, retryState.summary, retryState.idempotency_key, retryState.metadata || {});
    if (ingest.status === 0) {
      markCopilotIngested(stateDir, retryState.session_key, retryState.repo_root, retryState.turn || {}, retryState.last_prompt || "", cwd);
      appendEvent(stateDir, "retry_ingested", {
        retry_path: retryPath,
        session_key: retryState.session_key || null,
        dedupe_key: retryState.turn?.dedupe_key || null,
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    if (!isRetryableIngestFailure(ingest)) {
      appendError(stateDir, "Copilot retry abandoned after non-retryable ingest failure.", {
        retry_path: retryPath,
        status: ingest.status,
        stdout: truncate(ingest.stdout || "", 1200),
        stderr: truncate(ingest.stderr || "", 1200),
        target: retryState.target,
      });
      rmSync(retryPath, { force: true });
      return;
    }

    const nextState = buildRetryState(retryState, ingest);
    writeJsonFile(retryPath, nextState);
    appendEvent(stateDir, "retry_rescheduled", {
      retry_path: retryPath,
      session_key: nextState.session_key || null,
      dedupe_key: nextState.turn?.dedupe_key || null,
      attempt_count: nextState.attempt_count,
      next_attempt_at: nextState.next_attempt_at,
    });
    await sleep(retryDelayMs(nextState.attempt_count));
  }
}

function enqueueStopJob(cwd, stateDir, payload, eventName) {
  const repoRoot = repoRootForCwd(cwd);
  const sessionKey = extractSessionKey(payload, cwd);
  const sessionState = readSessionState(stateDir, sessionKey);
  if (isHeadlessPrompt(sessionState.last_prompt || "")) {
    appendEvent(stateDir, "worker_stop_ignored", {
      session_key: sessionKey,
      event_name: eventName,
    });
    return;
  }
  const jobsDir = join(stateDir, "jobs");
  ensureDir(jobsDir);
  const jobId = createHash("sha1")
    .update(sessionKey + ":" + eventName + ":" + String(Date.now()))
    .digest("hex");
  const jobPath = join(jobsDir, jobId + ".json");

  writeJsonFile(jobPath, {
    cwd,
    repo_root: repoRoot,
    event_name: eventName,
    session_key: sessionKey,
    prompt: typeof sessionState.last_prompt === "string" ? sessionState.last_prompt : "",
    raw_payload: payload,
  });

  const child = spawn(process.execPath, [process.argv[1], "--worker", jobPath], {
    cwd,
    detached: true,
    stdio: "ignore",
    env: {
      ...process.env,
      RECALLSTACK_HOOK_ACTIVE: "1",
    },
  });
  child.unref();

  updateSessionState(stateDir, sessionKey, {
    cwd,
    repo_root: repoRoot,
    last_stop_event: eventName,
    last_stop_at: new Date().toISOString(),
    last_job_path: jobPath,
  });
  appendEvent(stateDir, "job_spawned", {
    session_key: sessionKey,
    event_name: eventName,
    job_path: jobPath,
  });
}

function runWorker(jobPath) {
  const job = readJsonFile(jobPath, null);
  if (!job || typeof job !== "object") {
    return;
  }

  const cwd = typeof job.cwd === "string" && job.cwd.length ? job.cwd : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  const project = resolveProjectTarget(cwd, stateDir);
  if (!project) {
    return;
  }

  const expectedPrompt = typeof job.prompt === "string" ? job.prompt : "";
  const repoRoot = typeof job.repo_root === "string" && job.repo_root.length ? job.repo_root : repoRootForCwd(cwd);
  const sessionKey = typeof job.session_key === "string" && job.session_key.length
    ? job.session_key
    : createHash("sha1").update(repoRoot).digest("hex");
  const currentState = readSessionState(stateDir, sessionKey);
  const turn = findTurnFromPayload(job.raw_payload || {}, repoRoot, expectedPrompt);
  if (!turn) {
    appendError(stateDir, "Copilot hook worker could not reconstruct the completed turn.", {
      job_path: jobPath,
      repo_root: repoRoot,
      expected_prompt: expectedPrompt || null,
    });
    return;
  }

  const contextualTurn = {
    ...turn,
    previous_turn_snapshot: currentState.previous_turn_snapshot || null,
  };
  const extracted = runExtractor(contextualTurn, cwd, stateDir);
  if (!extracted) {
    return;
  }

  if (currentState.last_ingested_turn_id === turn.dedupe_key) {
    appendEvent(stateDir, "job_duplicate_skipped", {
      job_path: jobPath,
      session_key: sessionKey,
      dedupe_key: turn.dedupe_key,
    });
    rmSync(jobPath, { force: true });
    return;
  }

  if (extracted.turn_kind === "pleasantry") {
    appendEvent(stateDir, "job_noop", {
      job_path: jobPath,
      session_key: sessionKey,
      dedupe_key: turn.dedupe_key,
    });
    rmSync(jobPath, { force: true });
    return;
  }

  const summary = formatSummary(extracted, contextualTurn);
  if (!summary.length) {
    appendEvent(stateDir, "job_empty_summary", {
      job_path: jobPath,
      session_key: sessionKey,
      dedupe_key: turn.dedupe_key,
    });
    rmSync(jobPath, { force: true });
    return;
  }

  updateSessionState(stateDir, sessionKey, {
    previous_turn_snapshot: buildTurnSnapshot(extracted, summary),
  });

  const metadata = {
    agent: "github_copilot",
    source: typeof job.event_name === "string" && job.event_name === "sessionEnd" ? "copilot_cli_hook" : "copilot_vscode_hook",
    extractor: "copilot_print",
    repo_root: repoRoot,
    session_file: turn.session_file || null,
    tool_names: Array.isArray(turn.tool_names) ? turn.tool_names : [],
    previous_turn_snapshot_used: Boolean(contextualTurn.previous_turn_snapshot),
    turn_kind: extracted.turn_kind || null,
    user_intent: extracted.user_intent || null,
    agent_intent: extracted.agent_intent || null,
    key_actions: compactList(extracted.key_actions),
    tradeoffs: compactList(extracted.tradeoffs),
    open_questions: compactList(extracted.open_questions),
  };

  const idempotencyKey = "copilot-hook-" + createHash("sha1").update(repoRoot + ":" + turn.dedupe_key).digest("hex");
  const ingest = ingestMemory(cwd, project.target, summary, idempotencyKey, metadata);

  if (ingest.status !== 0) {
    if (isRetryableIngestFailure(ingest)) {
      queueRetry(stateDir, {
        cwd,
        target: project.target,
        summary,
        metadata,
        idempotency_key: idempotencyKey,
        session_key: sessionKey,
        repo_root: repoRoot,
        turn,
        last_prompt: currentState.last_prompt || "",
      }, ingest);
      return;
    }
    appendError(stateDir, "Recallstack ingest failed from Copilot hook.", {
      status: ingest.status,
      stdout: truncate(ingest.stdout || "", 1200),
      stderr: truncate(ingest.stderr || "", 1200),
      target: project.target,
      dedupe_key: turn.dedupe_key,
      job_path: jobPath,
    });
    return;
  }

  markCopilotIngested(stateDir, sessionKey, repoRoot, turn, currentState.last_prompt || "", cwd);
  appendEvent(stateDir, "job_ingested", {
    session_key: sessionKey,
    dedupe_key: turn.dedupe_key,
    target: project.target,
    job_path: jobPath,
  });
  rmSync(jobPath, { force: true });
}

async function main() {
  if (process.argv[2] === "--worker") {
    const jobPath = process.argv[3];
    if (typeof jobPath === "string" && jobPath.length) {
      runWorker(jobPath);
    }
    return;
  }
  if (process.argv[2] === "--worker-retry") {
    const retryPath = process.argv[3];
    if (typeof retryPath === "string" && retryPath.length) {
      await processRetryJob(retryPath);
    }
    return;
  }

  const raw = await readStdin();
  const payload = readJsonFromString(raw, {});
  const cwd = typeof payload.cwd === "string" && payload.cwd.trim().length
    ? payload.cwd.trim()
    : process.cwd();
  const stateDir = stateDirForCwd(cwd);
  ensureDir(stateDir);

  const eventName = normalizeEventName(process.argv[2] || payload.hookEventName || payload.hook_event_name || "");
  const sessionKey = extractSessionKey(payload, cwd);

  if (eventName === "sessionStart") {
    updateSessionState(stateDir, sessionKey, {
      cwd,
      repo_root: repoRootForCwd(cwd),
      last_session_start_at: new Date().toISOString(),
    });
    appendEvent(stateDir, "session_started", { session_key: sessionKey });
    return;
  }

  if (eventName === "userPromptSubmitted") {
    const prompt = extractPrompt(payload);
    if (isHeadlessPrompt(prompt)) {
      appendEvent(stateDir, "worker_prompt_ignored", { session_key: sessionKey });
      return;
    }
    if (prompt.length) {
      updateSessionState(stateDir, sessionKey, {
        cwd,
        repo_root: repoRootForCwd(cwd),
        last_prompt: prompt,
        last_prompt_at: new Date().toISOString(),
      });
      appendEvent(stateDir, "prompt_captured", { session_key: sessionKey });
    }
    return;
  }

  if (eventName === "sessionEnd" || eventName === "agentStop") {
    enqueueStopJob(cwd, stateDir, payload, eventName);
  }
}

main().catch((error) => {
  const cwd = process.cwd();
  const stateDir = stateDirForCwd(cwd);
  appendError(stateDir, "Unhandled Copilot hook failure.", {
    error: error instanceof Error ? error.stack || error.message : String(error),
  });
  process.exit(0);
});
`;

const COPILOT_MEMORY_GUIDANCE = `# Recallstack Memory (GitHub Copilot Local)

Required setup (once per repository):

1. \`recallstack login <CODE>\`
2. \`recallstack workspace use\`
3. \`recallstack project use\`
4. \`recallstack project\` (verify target)
5. Optional per-command override:
   add \`--project <projectSlug|workspaceSlug/projectSlug>\`

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - use the current thread for already-established answers
   - inspect code, logs, config, and tests for directly verifiable current-state facts
   - query Recallstack for rationale, prior decisions, historical context, related prior work, recurring issue history, user preference history, or non-local context
   - keep Recallstack operations in the background and avoid narrating query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer
2. If memory is the right source, call:
   \`recallstack memory query --query "<state/proof-oriented task>" --mode standard\`
   Add \`--synthesize\` to draft a local answer from the retrieved evidence, or pass \`--worker-model <model>\` to both enable synthesis and choose the local worker model.
3. Use \`mode=quick\` for fast checks, \`standard\` for default recall, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
4. Do not use manual turn ids for GitHub Copilot retrieval. Hooks own turn ingestion and there is no manual query/ingest pairing to preserve.
5. Raw writes to \`global\` are forbidden. Ensure the configured target is non-global.

GitHub Copilot hook behavior (installed by \`recallstack agent install copilot\`):

1. This integration supports GitHub Copilot CLI and the GitHub Copilot agent in VS Code.
2. Copilot CLI installs repo-local hook config under \`.github/hooks/\` for \`sessionStart\`, \`userPromptSubmitted\`, and \`sessionEnd\`.
3. VS Code installs repo-local hook config under \`.github/hooks/\` for \`SessionStart\`, \`UserPromptSubmit\`, and \`Stop\`.
4. Both variants point to the same Recallstack hook worker.
5. The worker reconstructs the finished turn from Copilot transcript/session-state artifacts, then runs a headless \`copilot -p\` extraction pass from an isolated temp config dir so the user's normal Copilot history is not polluted.
6. Set the background extraction model by reinstalling with \`recallstack agent install copilot --worker-model <model>\`.
7. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours.
8. Stop/session-end processing is queued in the background so the agent loop is not delayed.
9. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance:

1. GitHub Copilot hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s postprocessing pass is responsible for condensing actions and consequences into retrievable memory.
4. Use \`recallstack memory source ingest\` for durable plans, handovers, specs, checklists, research notes, copied docs, meeting notes, and verified issue-remedy runbooks.
5. Durable ingest is the default when the user says \`remember this\`, \`store this\`, \`save this\`, or asks for a long-lived plan, handover, checklist, spec, or reference note.
6. Use \`memory_query\` for blended recall only when the question is about history, rationale, prior related work, recurring issue history, user preference history, or non-local context. Prefer \`mode=standard\` by default, \`quick\` for low-latency checks, \`deep\` for fuller context, and \`forensic\` for investigative retrieval.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer \`what already exists for X\`, \`find prior related work on X\`, \`why did we choose X\`, or \`is X already implemented\`.
9. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search.
10. Use \`memory_query_direct\` for exact durable-source passages or existing durable artifacts.
11. Check durable memory before drafting a new durable artifact if prior documentation may already exist.
12. After a remedy is verified and likely reusable, ingest it durably as a concise issue-remedy note with a searchable title and metadata.
13. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
14. Use \`memory_get_source\` for full-body drilldown.
15. Do not pollute the thread with memory activity. Recallstack use should usually be silent. If memory activity must be mentioned, keep it to one short sentence.
`;

export const COPILOT_INSTRUCTIONS_TEMPLATE = COPILOT_MEMORY_GUIDANCE;

export const COPILOT_PLUGIN_SKILL_TEMPLATE = `---
description: Use Recallstack only for prior decisions, non-local history, recurring issue context, and other context that is not cheaper to verify directly in the current workspace.
---

${COPILOT_MEMORY_GUIDANCE}`;
