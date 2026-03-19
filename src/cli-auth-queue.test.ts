import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import http from "node:http";

test("CLI queues auth-blocked memory ingest and replays it after login", async () => {
  const tmp = mkdtempSync(join(tmpdir(), "recallstack-cli-auth-queue-"));
  const home = join(tmp, "home");
  const workspace = join(tmp, "workspace");
  mkdirSync(home, { recursive: true });
  mkdirSync(join(workspace, ".recallstack"), { recursive: true });
  writeFileSync(join(workspace, ".recallstack", "workspace.json"), `${JSON.stringify({
    workspace_id: "ws-smoke",
    workspace_slug: "smoke-ws",
    workspace_name: "Smoke Workspace",
    project_id: "proj-smoke-id",
    project_slug: "smoke-project",
    project_name: "Smoke Project",
    project_mode: "PROJECT",
  }, null, 2)}\n`, "utf8");

  const serverState = {
    eventAttempts: [] as Array<{ auth: string; body: string }>,
    replayed: false,
    telemetry: 0,
  };

  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (chunk) => {
      body += String(chunk);
    });
    req.on("end", () => {
      const auth = typeof req.headers.authorization === "string" ? req.headers.authorization : "";
      if (req.url === "/v1/telemetry/events") {
        serverState.telemetry += 1;
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ status: "ok" }));
        return;
      }

      if (req.url === "/v1/memory/events" && req.method === "POST") {
        serverState.eventAttempts.push({ auth, body });
        if (auth !== "Bearer access-good") {
          res.writeHead(401, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: { code: "UNAUTHORIZED", message: "unauthorized" } }));
          return;
        }
        serverState.replayed = true;
        res.writeHead(201, { "content-type": "application/json" });
        res.end(JSON.stringify({
          memory: { id: "mem-1" },
          deduped: false,
          validation: { applied: false },
        }));
        return;
      }

      if (req.url === "/v1/auth/cli/code/exchange" && req.method === "POST") {
        const parsed = JSON.parse(body || "{}") as { code?: string };
        if (parsed.code !== "SMOKE-CODE") {
          res.writeHead(400, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: { code: "INVALID", message: "bad code" } }));
          return;
        }
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ access_token: "access-good", refresh_token: "refresh-good" }));
        return;
      }

      if (req.url === "/v1/auth/token/refresh" && req.method === "POST") {
        res.writeHead(401, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { code: "UNAUTHORIZED", message: "refresh blocked" } }));
        return;
      }

      if (req.url === "/v1/auth/logout" && req.method === "POST") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ status: "ok" }));
        return;
      }

      res.writeHead(404, { "content-type": "application/json" });
      res.end(JSON.stringify({ error: { code: "NOT_FOUND", message: req.url || "" } }));
    });
  });

  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = server.address();
    assert.ok(address && typeof address === "object");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const cliEntry = join(process.cwd(), "dist", "index.js");

    mkdirSync(join(home, ".recallstack"), { recursive: true });
    writeFileSync(join(home, ".recallstack", "config.json"), `${JSON.stringify({
      baseUrl,
      profiles: {
        [baseUrl]: {
          accessToken: "expired-token",
          refreshToken: "expired-refresh",
          activeWorkspaceId: "ws-smoke",
          activeWorkspaceSlug: "smoke-ws",
          activeWorkspaceName: "Smoke Workspace",
        },
      },
    }, null, 2)}\n`, "utf8");

    const runCli = async (args: string[], input = "") => {
      const child = spawn(process.execPath, [cliEntry, ...args], {
        cwd: workspace,
        env: {
          ...process.env,
          HOME: home,
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
      if (input.length > 0) {
        child.stdin.write(input);
      }
      child.stdin.end();

      const timeout = setTimeout(() => {
        child.kill("SIGTERM");
      }, 15000);

      const result = await new Promise<{ status: number | null; stdout: string; stderr: string }>((resolve, reject) => {
        child.on("error", (error) => {
          clearTimeout(timeout);
          reject(error);
        });
        child.on("close", (status) => {
          clearTimeout(timeout);
          resolve({ status, stdout, stderr });
        });
      });
      return result;
    };

    const ingest = await runCli(["memory", "ingest", "--stdin"], "queued during auth loss\n");
    assert.equal(ingest.status, 0, ingest.stderr || ingest.stdout);
    const ingestOut = JSON.parse(ingest.stdout) as {
      status?: string;
      queued_local?: boolean;
      queue_id?: string;
      sync_status?: string;
    };
    assert.equal(ingestOut.status, "queued_local");
    assert.equal(ingestOut.queued_local, true);
    assert.equal(ingestOut.sync_status, "pending_login");
    assert.ok(typeof ingestOut.queue_id === "string" && ingestOut.queue_id.length > 0);

    const queueDir = join(home, ".recallstack", "pending-writes");
    assert.equal(readdirSync(queueDir).filter((entry) => entry.endsWith(".json")).length, 1);

    const login = await runCli(["login", "SMOKE-CODE"]);
    assert.equal(login.status, 0, login.stderr || login.stdout);
    assert.match(login.stdout, /Replayed 1 queued memory write\./);
    assert.match(login.stdout, /Login successful/);
    assert.equal(readdirSync(queueDir).filter((entry) => entry.endsWith(".json")).length, 0);

    assert.equal(serverState.eventAttempts.length, 2);
    assert.equal(serverState.replayed, true);
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()));
    });
  }
});
