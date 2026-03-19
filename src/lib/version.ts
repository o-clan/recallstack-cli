import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

function readCliPackageVersion(): string {
  try {
    const currentDir = dirname(fileURLToPath(import.meta.url));
    const packagePath = resolve(currentDir, "../../package.json");
    const parsed = JSON.parse(readFileSync(packagePath, "utf8")) as { version?: unknown };
    if (typeof parsed.version === "string" && parsed.version.trim().length > 0) {
      return parsed.version.trim();
    }
  } catch {
    // Fall back to a safe default when package metadata is unavailable.
  }

  return "0.0.0";
}

export const CLI_PACKAGE_VERSION = readCliPackageVersion();
export const CLI_CLIENT_ID = `recallstack-cli/${CLI_PACKAGE_VERSION}`;
