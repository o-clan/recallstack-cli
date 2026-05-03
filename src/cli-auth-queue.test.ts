import test from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
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

    const config = await runCli(["config", "show"]);
    assert.equal(config.status, 0, config.stderr || config.stdout);
    const configOut = JSON.parse(config.stdout) as {
      auth_status?: {
        status?: string;
        pending_writes?: number;
        auth_blocked_pending_writes?: number;
        login_required?: boolean;
      };
    };
    assert.equal(configOut.auth_status?.status, "login_required_with_pending_replay");
    assert.equal(configOut.auth_status?.pending_writes, 1);
    assert.equal(configOut.auth_status?.auth_blocked_pending_writes, 1);
    assert.equal(configOut.auth_status?.login_required, true);

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

test("CLI refresh recovers when another process already rotated the refresh token", async () => {
  const tmp = mkdtempSync(join(tmpdir(), "recallstack-cli-refresh-race-"));
  const home = join(tmp, "home");
  const workspace = join(tmp, "workspace");
  mkdirSync(home, { recursive: true });
  mkdirSync(join(workspace, ".recallstack"), { recursive: true });
  writeFileSync(join(workspace, ".recallstack", "workspace.json"), `${JSON.stringify({
    workspace_id: "ws-race",
    workspace_slug: "race-ws",
    workspace_name: "Race Workspace",
    project_id: "proj-race-id",
    project_slug: "race-project",
    project_name: "Race Project",
    project_mode: "PROJECT",
  }, null, 2)}\n`, "utf8");

  const requests: string[] = [];
  let baseUrl = "";

  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (chunk) => {
      body += String(chunk);
    });
    req.on("end", () => {
      const auth = typeof req.headers.authorization === "string" ? req.headers.authorization : "";
      requests.push(`${req.method} ${req.url} ${auth}`);

      if (req.url === "/v1/telemetry/events") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ status: "ok" }));
        return;
      }

      if (req.url === "/v1/auth/me" && req.method === "GET") {
        if (auth === "Bearer access-new") {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify({
            id: "user-race",
            email: "race@example.com",
            auth_type: "jwt",
            workspace: { id: "ws-race", kind: "PERSONAL", role: "ADMIN" },
          }));
          return;
        }
        res.writeHead(401, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { code: "UNAUTHORIZED", message: "expired" } }));
        return;
      }

      if (req.url === "/v1/auth/token/refresh" && req.method === "POST") {
        writeFileSync(join(home, ".recallstack", "config.json"), `${JSON.stringify({
          baseUrl,
          profiles: {
            [baseUrl]: {
              accessToken: "access-new",
              refreshToken: "refresh-new",
              activeWorkspaceId: "ws-race",
              activeWorkspaceSlug: "race-ws",
              activeWorkspaceName: "Race Workspace",
            },
          },
        }, null, 2)}\n`, "utf8");
        res.writeHead(409, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { code: "REFRESH_RACE_DETECTED", message: "race" } }));
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
    baseUrl = `http://127.0.0.1:${address.port}`;
    mkdirSync(join(home, ".recallstack"), { recursive: true });
    writeFileSync(join(home, ".recallstack", "config.json"), `${JSON.stringify({
      baseUrl,
      profiles: {
        [baseUrl]: {
          accessToken: "access-old",
          refreshToken: "refresh-old",
          activeWorkspaceId: "ws-race",
          activeWorkspaceSlug: "race-ws",
          activeWorkspaceName: "Race Workspace",
        },
      },
    }, null, 2)}\n`, "utf8");

    const cliEntry = join(process.cwd(), "dist", "index.js");
    const child = spawn(process.execPath, [cliEntry, "whoami"], {
      cwd: workspace,
      env: {
        ...process.env,
        HOME: home,
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

    const result = await new Promise<{ status: number | null }>((resolve, reject) => {
      child.on("error", reject);
      child.on("close", (status) => resolve({ status }));
    });

    assert.equal(result.status, 0, stderr || stdout);
    assert.equal(JSON.parse(stdout).email, "race@example.com");
    assert.ok(requests.some((entry) => entry.includes("Bearer access-old")));
    assert.ok(requests.some((entry) => entry.includes("Bearer access-new")));
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()));
    });
  }
});

test("CLI stores macOS-style secure tokens outside JSON config when keychain storage is enabled", async () => {
  const tmp = mkdtempSync(join(tmpdir(), "recallstack-cli-keychain-"));
  const home = join(tmp, "home");
  const workspace = join(tmp, "workspace");
  const fakeBin = join(tmp, "bin");
  const keychainPath = join(tmp, "keychain.json");
  mkdirSync(home, { recursive: true });
  mkdirSync(fakeBin, { recursive: true });
  mkdirSync(join(workspace, ".recallstack"), { recursive: true });
  writeFileSync(join(workspace, ".recallstack", "workspace.json"), `${JSON.stringify({
    workspace_id: "ws-secure",
    workspace_slug: "secure-ws",
    workspace_name: "Secure Workspace",
    project_id: "proj-secure-id",
    project_slug: "secure-project",
    project_name: "Secure Project",
    project_mode: "PROJECT",
  }, null, 2)}\n`, "utf8");

  const fakeSecurityPath = join(fakeBin, "security");
  writeFileSync(fakeSecurityPath, `#!/usr/bin/env node
const fs = require("node:fs");
const path = process.env.FAKE_KEYCHAIN_PATH;
const args = process.argv.slice(2);
if (args[0] === "-h") process.exit(0);
const read = () => {
  try { return JSON.parse(fs.readFileSync(path, "utf8")); } catch { return {}; }
};
const write = (value) => fs.writeFileSync(path, JSON.stringify(value, null, 2) + "\\n", "utf8");
const arg = (flag) => {
  const index = args.indexOf(flag);
  return index >= 0 ? args[index + 1] : "";
};
const key = arg("-s") + "|" + arg("-a");
const data = read();
if (args[0] === "add-generic-password") {
  data[key] = arg("-w");
  write(data);
  process.exit(0);
}
if (args[0] === "find-generic-password") {
  if (!data[key]) process.exit(44);
  process.stdout.write(data[key] + "\\n");
  process.exit(0);
}
if (args[0] === "delete-generic-password") {
  delete data[key];
  write(data);
  process.exit(0);
}
process.exit(2);
`, "utf8");
  chmodSync(fakeSecurityPath, 0o755);

  const server = http.createServer((req, res) => {
    let body = "";
    req.on("data", (chunk) => {
      body += String(chunk);
    });
    req.on("end", () => {
      const auth = typeof req.headers.authorization === "string" ? req.headers.authorization : "";
      if (req.url === "/v1/telemetry/events") {
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ status: "ok" }));
        return;
      }
      if (req.url === "/v1/auth/cli/code/exchange" && req.method === "POST") {
        const parsed = JSON.parse(body || "{}") as { code?: string };
        if (parsed.code !== "SECURE-CODE") {
          res.writeHead(400, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: { code: "INVALID", message: "bad code" } }));
          return;
        }
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ access_token: "access-secure", refresh_token: "refresh-secure" }));
        return;
      }
      if (req.url === "/v1/auth/me" && req.method === "GET") {
        if (auth === "Bearer access-secure") {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify({
            id: "user-secure",
            email: "secure@example.com",
            auth_type: "jwt",
            workspace: { id: "ws-secure", kind: "PERSONAL", role: "ADMIN" },
          }));
          return;
        }
        res.writeHead(401, { "content-type": "application/json" });
        res.end(JSON.stringify({ error: { code: "UNAUTHORIZED", message: "unauthorized" } }));
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
    mkdirSync(join(home, ".recallstack"), { recursive: true });
    writeFileSync(join(home, ".recallstack", "config.json"), `${JSON.stringify({ baseUrl, profiles: {} }, null, 2)}\n`, "utf8");

    const cliEntry = join(process.cwd(), "dist", "index.js");
    const env = {
      ...process.env,
      HOME: home,
      FAKE_KEYCHAIN_PATH: keychainPath,
      PATH: `${fakeBin}:${process.env.PATH || ""}`,
      RECALLSTACK_TOKEN_STORE: "keychain",
    };
    const runCli = async (args: string[]) => {
      const child = spawn(process.execPath, [cliEntry, ...args], {
        cwd: workspace,
        env,
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
      return new Promise<{ status: number | null; stdout: string; stderr: string }>((resolve, reject) => {
        child.on("error", reject);
        child.on("close", (status) => resolve({ status, stdout, stderr }));
      });
    };

    const login = await runCli(["login", "SECURE-CODE"]);
    assert.equal(login.status, 0, login.stderr || login.stdout);

    const config = JSON.parse(readFileSync(join(home, ".recallstack", "config.json"), "utf8")) as {
      profiles?: Record<string, { accessToken?: string; refreshToken?: string; tokenStorage?: string }>;
    };
    assert.equal(config.profiles?.[baseUrl]?.tokenStorage, "keychain");
    assert.equal(config.profiles?.[baseUrl]?.accessToken, undefined);
    assert.equal(config.profiles?.[baseUrl]?.refreshToken, undefined);

    const keychain = JSON.parse(readFileSync(keychainPath, "utf8")) as Record<string, string>;
    assert.ok(Object.values(keychain).includes("access-secure"));
    assert.ok(Object.values(keychain).includes("refresh-secure"));

    const whoami = await runCli(["whoami"]);
    assert.equal(whoami.status, 0, whoami.stderr || whoami.stdout);
    assert.equal(JSON.parse(whoami.stdout).email, "secure@example.com");
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()));
    });
  }
});
