import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { homedir } from "node:os";

export type RecallstackProfile = {
  accessToken?: string;
  refreshToken?: string;
  apiKey?: string;
  activeWorkspaceId?: string;
  activeWorkspaceSlug?: string;
  activeWorkspaceName?: string;
};

export type RecallstackConfig = RecallstackProfile & {
  baseUrl?: string;
  effectiveBaseUrl: string;
};

export type ConfigScope = "global" | "project";

export type ResolvedConfig = {
  scope: ConfigScope;
  scopeRoot: string;
  path: string;
  globalPath: string;
};

type ConfigPathOptions = {
  scope?: ConfigScope | "resolved";
  cwd?: string;
};

type GlobalConfigFile = {
  baseUrl?: string;
  profiles: Record<string, RecallstackProfile>;
};

type ProjectConfigFile = {
  baseUrl?: string;
};

const globalScopeRoot = homedir();
const globalConfigPath = join(globalScopeRoot, ".recallstack", "config.json");

export const DEFAULT_BASE_URL = "https://api.recallstack.com";

function normalizeOptionalUrl(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  if (!trimmed.length) return undefined;
  return trimmed.replace(/\/+$/, "");
}

function normalizeOptionalToken(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  return trimmed.length ? trimmed : undefined;
}

function normalizeProfile(value: unknown): RecallstackProfile {
  const parsed = typeof value === "object" && value !== null ? value as Record<string, unknown> : {};
  return {
    accessToken: normalizeOptionalToken(parsed.accessToken),
    refreshToken: normalizeOptionalToken(parsed.refreshToken),
    apiKey: normalizeOptionalToken(parsed.apiKey),
    activeWorkspaceId: normalizeOptionalToken(parsed.activeWorkspaceId),
    activeWorkspaceSlug: normalizeOptionalToken(parsed.activeWorkspaceSlug),
    activeWorkspaceName: normalizeOptionalToken(parsed.activeWorkspaceName),
  };
}

function serializeProfile(profile: RecallstackProfile): Record<string, string | undefined> {
  return {
    accessToken: normalizeOptionalToken(profile.accessToken),
    refreshToken: normalizeOptionalToken(profile.refreshToken),
    apiKey: normalizeOptionalToken(profile.apiKey),
    activeWorkspaceId: normalizeOptionalToken(profile.activeWorkspaceId),
    activeWorkspaceSlug: normalizeOptionalToken(profile.activeWorkspaceSlug),
    activeWorkspaceName: normalizeOptionalToken(profile.activeWorkspaceName),
  };
}

function isProfileEmpty(profile: RecallstackProfile): boolean {
  return Object.values(serializeProfile(profile)).every((value) => value === undefined);
}

function normalizeProfiles(value: unknown): Record<string, RecallstackProfile> {
  if (typeof value !== "object" || value === null) return {};
  const parsed = value as Record<string, unknown>;
  const profiles: Record<string, RecallstackProfile> = {};

  for (const [key, profile] of Object.entries(parsed)) {
    const normalizedKey = normalizeOptionalUrl(key);
    if (!normalizedKey) continue;
    const normalizedProfile = normalizeProfile(profile);
    if (isProfileEmpty(normalizedProfile)) continue;
    profiles[normalizedKey] = normalizedProfile;
  }

  return profiles;
}

function readJsonRecord(filePath: string): Record<string, unknown> | undefined {
  try {
    return JSON.parse(readFileSync(filePath, "utf8")) as Record<string, unknown>;
  } catch {
    return undefined;
  }
}

function readGlobalConfigFile(filePath: string): GlobalConfigFile {
  const parsed = readJsonRecord(filePath);
  return {
    baseUrl: normalizeOptionalUrl(parsed?.baseUrl),
    profiles: normalizeProfiles(parsed?.profiles),
  };
}

function readProjectConfigFile(filePath: string): ProjectConfigFile | undefined {
  const parsed = readJsonRecord(filePath);
  if (!parsed) return undefined;
  return {
    baseUrl: normalizeOptionalUrl(parsed.baseUrl),
  };
}

function writeConfigFile(filePath: string, content: Record<string, unknown>): void {
  mkdirSync(dirname(filePath), { recursive: true });
  writeFileSync(filePath, `${JSON.stringify(content, null, 2)}\n`, "utf8");
}

function writeGlobalConfigFile(filePath: string, config: GlobalConfigFile): void {
  const profiles = Object.fromEntries(
    Object.entries(config.profiles)
      .map(([key, profile]) => [key, Object.fromEntries(
        Object.entries(serializeProfile(profile)).filter(([, value]) => value !== undefined),
      )])
      .filter(([, profile]) => Object.keys(profile as Record<string, string>).length > 0),
  );

  writeConfigFile(filePath, {
    ...(config.baseUrl ? { baseUrl: config.baseUrl } : {}),
    ...(Object.keys(profiles).length ? { profiles } : {}),
  });
}

function writeProjectConfigFile(filePath: string, config: ProjectConfigFile): void {
  writeConfigFile(filePath, {
    ...(config.baseUrl ? { baseUrl: config.baseUrl } : {}),
  });
}

function getProjectConfigPath(root: string): string {
  return join(root, ".recallstack", "config.json");
}

function hasProjectConfigMarker(root: string): boolean {
  if (resolve(root) === resolve(globalScopeRoot)) {
    return false;
  }
  return existsSync(join(root, ".recallstack", "config.json"))
    || existsSync(join(root, ".recallstack", "workspace.json"));
}

function findProjectRoot(startCwd: string): string | undefined {
  let cursor = resolve(startCwd);
  while (true) {
    if (hasProjectConfigMarker(cursor)) {
      return cursor;
    }
    const parent = dirname(cursor);
    if (parent === cursor) break;
    cursor = parent;
  }
  return undefined;
}

export function resolveConfig(options: ConfigPathOptions = {}): ResolvedConfig {
  const requestedScope = options.scope || "resolved";
  if (requestedScope === "global") {
    return {
      scope: "global",
      scopeRoot: globalScopeRoot,
      path: globalConfigPath,
      globalPath: globalConfigPath,
    };
  }

  const cwd = options.cwd || process.cwd();
  const projectRoot = findProjectRoot(cwd);
  if (!projectRoot) {
    return {
      scope: "global",
      scopeRoot: globalScopeRoot,
      path: globalConfigPath,
      globalPath: globalConfigPath,
    };
  }

  return {
    scope: "project",
    scopeRoot: projectRoot,
    path: getProjectConfigPath(projectRoot),
    globalPath: globalConfigPath,
  };
}

function resolveConfigPath(options: ConfigPathOptions = {}): string {
  return resolveConfig(options).path;
}

export function getEffectiveBaseUrl(config: Pick<RecallstackConfig, "effectiveBaseUrl" | "baseUrl">): string {
  if (normalizeOptionalUrl(config.effectiveBaseUrl)) {
    return normalizeOptionalUrl(config.effectiveBaseUrl) as string;
  }
  return normalizeOptionalUrl(config.baseUrl) || DEFAULT_BASE_URL;
}

export function loadConfig(options: ConfigPathOptions = {}): RecallstackConfig {
  const resolved = resolveConfig(options);
  const globalConfig = readGlobalConfigFile(resolved.globalPath);
  const projectConfig = resolved.scope === "project" ? readProjectConfigFile(resolved.path) : undefined;
  const baseUrl = projectConfig?.baseUrl ?? globalConfig.baseUrl;
  const effectiveBaseUrl = baseUrl || DEFAULT_BASE_URL;
  const profile = globalConfig.profiles[effectiveBaseUrl] || {};

  return {
    baseUrl,
    effectiveBaseUrl,
    ...profile,
  };
}

export function saveBaseUrlOverride(baseUrl: string | undefined, options: ConfigPathOptions = {}): void {
  const resolved = resolveConfig(options);
  const normalizedBaseUrl = normalizeOptionalUrl(baseUrl);

  if (resolved.scope === "project") {
    writeProjectConfigFile(resolved.path, { baseUrl: normalizedBaseUrl });
    return;
  }

  const globalConfig = readGlobalConfigFile(resolved.globalPath);
  writeGlobalConfigFile(resolved.globalPath, {
    ...globalConfig,
    baseUrl: normalizedBaseUrl,
  });
}

export function updateProfileForBaseUrl(baseUrl: string, patch: Partial<RecallstackProfile>): void {
  const targetKey = normalizeOptionalUrl(baseUrl) || DEFAULT_BASE_URL;
  const globalConfig = readGlobalConfigFile(globalConfigPath);
  const currentProfile = globalConfig.profiles[targetKey] || {};
  const nextProfile = normalizeProfile({
    ...currentProfile,
    ...patch,
  });

  if (isProfileEmpty(nextProfile)) {
    delete globalConfig.profiles[targetKey];
  } else {
    globalConfig.profiles[targetKey] = nextProfile;
  }

  writeGlobalConfigFile(globalConfigPath, globalConfig);
}

export function getConfigPath(options: ConfigPathOptions = {}): string {
  return resolveConfigPath(options);
}
