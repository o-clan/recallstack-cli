import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";

export type SecureTokenName = "accessToken" | "refreshToken" | "apiKey";

const SERVICE_NAME = "Recallstack CLI";

function tokenStoreMode(): "auto" | "file" | "keychain" {
  const value = (process.env.RECALLSTACK_TOKEN_STORE || "auto").trim().toLowerCase();
  if (value === "file" || value === "keychain") return value;
  return "auto";
}

export function secureTokenStoreAvailable(): boolean {
  const mode = tokenStoreMode();
  if (mode === "file") return false;
  if (process.platform !== "darwin" && mode !== "keychain") return false;
  const probe = spawnSync("security", ["-h"], { encoding: "utf8", stdio: ["ignore", "ignore", "ignore"] });
  return probe.status === 0 || mode === "keychain";
}

function accountFor(baseUrl: string, tokenName: SecureTokenName): string {
  const fingerprint = createHash("sha256").update(baseUrl).digest("hex").slice(0, 32);
  return `recallstack:${fingerprint}:${tokenName}`;
}

export function readSecureToken(baseUrl: string, tokenName: SecureTokenName): string | undefined {
  if (!secureTokenStoreAvailable()) return undefined;
  const result = spawnSync("security", [
    "find-generic-password",
    "-a",
    accountFor(baseUrl, tokenName),
    "-s",
    SERVICE_NAME,
    "-w",
  ], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "ignore"],
  });
  if (result.status !== 0) return undefined;
  const value = result.stdout.trim();
  return value.length ? value : undefined;
}

export function writeSecureToken(baseUrl: string, tokenName: SecureTokenName, value: string): boolean {
  if (!secureTokenStoreAvailable()) return false;
  const result = spawnSync("security", [
    "add-generic-password",
    "-a",
    accountFor(baseUrl, tokenName),
    "-s",
    SERVICE_NAME,
    "-w",
    value,
    "-U",
    "-T",
    "/usr/bin/security",
  ], {
    encoding: "utf8",
    stdio: ["ignore", "ignore", "ignore"],
  });
  return result.status === 0;
}

export function deleteSecureToken(baseUrl: string, tokenName: SecureTokenName): void {
  if (!secureTokenStoreAvailable()) return;
  spawnSync("security", [
    "delete-generic-password",
    "-a",
    accountFor(baseUrl, tokenName),
    "-s",
    SERVICE_NAME,
  ], {
    encoding: "utf8",
    stdio: ["ignore", "ignore", "ignore"],
  });
}
