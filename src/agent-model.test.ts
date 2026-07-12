import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";

function makeWorkspace(): { home: string; workspace: string } {
  const tmp = mkdtempSync(join(tmpdir(), "recallstack-agent-model-"));
  const home = join(tmp, "home");
  const workspace = join(tmp, "workspace");
  mkdirSync(home, { recursive: true });
  mkdirSync(workspace, { recursive: true });
  return { home, workspace };
}

function runCli(input: { home: string; workspace: string }, args: string[]): Promise<{
  status: number | null;
  stdout: string;
  stderr: string;
}> {
  const cliEntry = join(process.cwd(), "dist", "index.js");
  const child = spawn(process.execPath, [cliEntry, ...args], {
    cwd: input.workspace,
    env: {
      ...process.env,
      HOME: input.home,
      RECALLSTACK_TOKEN_STORE: "file",
    },
    stdio: "pipe",
  });

  let stdout = "";
  let stderr = "";
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });

  return new Promise((resolve, reject) => {
    child.on("error", reject);
    child.on("close", (status) => resolve({ status, stdout, stderr }));
  });
}

test("agent model command sets, shows, reports, and clears synthesis models", async () => {
  const workspace = makeWorkspace();

  const set = await runCli(workspace, ["agent", "model", "codex", "gpt-5.4"]);
  assert.equal(set.status, 0, set.stderr || set.stdout);
  const setOut = JSON.parse(set.stdout) as {
    scope?: string;
    target?: string;
    worker_model?: string;
    model?: {
      worker_model?: string;
      worker_model_source?: string;
      change_command?: string;
      clear_command?: string;
    };
  };
  assert.equal(setOut.scope, "local");
  assert.equal(setOut.target, "codex");
  assert.equal(setOut.worker_model, "gpt-5.4");
  assert.equal(setOut.model?.worker_model, "gpt-5.4");
  assert.equal(setOut.model?.worker_model_source, "local");
  assert.equal(setOut.model?.change_command, "recallstack agent model codex <model>");
  assert.equal(setOut.model?.clear_command, "recallstack agent model codex --clear");

  const show = await runCli(workspace, ["agent", "model", "codex"]);
  assert.equal(show.status, 0, show.stderr || show.stdout);
  const showOut = JSON.parse(show.stdout) as { worker_model?: string; worker_model_source?: string };
  assert.equal(showOut.worker_model, "gpt-5.4");
  assert.equal(showOut.worker_model_source, "local");

  const list = await runCli(workspace, ["agent", "model"]);
  assert.equal(list.status, 0, list.stderr || list.stdout);
  const listOut = JSON.parse(list.stdout) as { models?: { codex?: { worker_model?: string } } };
  assert.equal(listOut.models?.codex?.worker_model, "gpt-5.4");

  const status = await runCli(workspace, ["agent", "status"]);
  assert.equal(status.status, 0, status.stderr || status.stdout);
  const statusOut = JSON.parse(status.stdout) as {
    codex?: {
      worker_model?: string;
      worker_model_source?: string;
      worker_model_change_command?: string;
      worker_model_clear_command?: string;
    };
  };
  assert.equal(statusOut.codex?.worker_model, "gpt-5.4");
  assert.equal(statusOut.codex?.worker_model_source, "local");
  assert.equal(statusOut.codex?.worker_model_change_command, "recallstack agent model codex <model>");
  assert.equal(statusOut.codex?.worker_model_clear_command, "recallstack agent model codex --clear");

  const clear = await runCli(workspace, ["agent", "model", "codex", "--clear"]);
  assert.equal(clear.status, 0, clear.stderr || clear.stdout);
  const clearOut = JSON.parse(clear.stdout) as { cleared?: boolean; model?: { worker_model?: string | null } };
  assert.equal(clearOut.cleared, true);
  assert.equal(clearOut.model?.worker_model, null);
});
