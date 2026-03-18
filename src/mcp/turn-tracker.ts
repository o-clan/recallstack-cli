import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";

export type DiffProofFile = {
  path: string;
  status: "added" | "modified" | "deleted" | "renamed";
  additions: number;
  deletions: number;
  excerpt?: string;
};

export type DiffProofPayload = {
  summary: {
    files_changed: number;
    additions: number;
    deletions: number;
  };
  files: DiffProofFile[];
  repo?: {
    vcs?: string;
    head_start?: string;
    head_end?: string;
    dirty_at_start?: boolean;
    source?: "git" | "manifest_fallback";
  };
  generated_at?: string;
};

type TurnSnapshot = {
  version: 1;
  turnId: string;
  cwd: string;
  repoRoot?: string;
  startedAt: string;
  headStart?: string;
  dirtyAtStart: boolean;
  dirtyPathsStart: string[];
  startHashes: Record<string, string>;
};

const TURN_TTL_MS = 24 * 60 * 60 * 1000;

function turnsDir(cwd: string): string {
  return join(cwd, ".recallstack", "turns");
}

function snapshotPath(cwd: string, turnId: string): string {
  return join(turnsDir(cwd), `${turnId}.json`);
}

function hashContent(content: Buffer): string {
  return createHash("sha256").update(content).digest("hex");
}

function safeReadFileHash(path: string): string | undefined {
  try {
    return hashContent(readFileSync(path));
  } catch {
    return undefined;
  }
}

function runGit(cwd: string, args: string[]): string | undefined {
  const proc = spawnSync("git", args, {
    cwd,
    encoding: "utf8",
  });
  if (proc.status !== 0) return undefined;
  return (proc.stdout || "").trim();
}

function resolveRepoRoot(cwd: string): string | undefined {
  const out = runGit(cwd, ["rev-parse", "--show-toplevel"]);
  if (!out) return undefined;
  return out.split("\n").filter(Boolean)[0];
}

function headSha(repoRoot: string): string | undefined {
  const out = runGit(repoRoot, ["rev-parse", "HEAD"]);
  if (!out) return undefined;
  return out.split("\n").filter(Boolean)[0];
}

function normalizeRepoPath(value: string): string {
  return value.replaceAll("\\", "/");
}

function parseStatusLine(line: string): string | undefined {
  if (!line.trim()) return undefined;
  const status = line.slice(0, 2);
  const tail = line.slice(3).trim();
  if (!tail) return undefined;
  if (tail.includes(" -> ")) {
    const parts = tail.split(" -> ");
    return normalizeRepoPath(parts[parts.length - 1] || "");
  }
  if (status.startsWith("R") || status.startsWith("C")) {
    const parts = tail.split("\t");
    return normalizeRepoPath(parts[parts.length - 1] || "");
  }
  return normalizeRepoPath(tail);
}

function gitDirtyPaths(repoRoot: string): string[] {
  const status = runGit(repoRoot, ["status", "--porcelain"]) || "";
  return Array.from(
    new Set(
      status
        .split("\n")
        .map((line) => parseStatusLine(line))
        .filter((line): line is string => Boolean(line)),
    ),
  );
}

function cleanupSnapshots(cwd: string): void {
  const dir = turnsDir(cwd);
  if (!existsSync(dir)) return;
  try {
    const files = readdirSync(dir)
      .filter((name) => name.endsWith(".json"))
      .map((name) => join(dir, name));
    for (const file of files) {
      try {
        const raw = readFileSync(file, "utf8");
        const snapshot = JSON.parse(raw) as Partial<TurnSnapshot>;
        const started = snapshot.startedAt ? Date.parse(snapshot.startedAt) : 0;
        if (!started || Date.now() - started > TURN_TTL_MS) {
          rmSync(file, { force: true });
        }
      } catch {
        rmSync(file, { force: true });
      }
    }
  } catch {
    // noop
  }
}

function safeExcerptFromDiff(repoRoot: string, path: string, baseRange?: string): string | undefined {
  const args = ["diff", "--unified=0"];
  if (baseRange) args.push(baseRange);
  args.push("--", path);
  const out = runGit(repoRoot, args);
  if (out) return out.slice(0, 1200);
  const abs = join(repoRoot, path);
  if (!existsSync(abs)) return undefined;
  try {
    return readFileSync(abs, "utf8").slice(0, 1200);
  } catch {
    return undefined;
  }
}

function parseNumstat(lines: string): Map<string, { additions: number; deletions: number }> {
  const out = new Map<string, { additions: number; deletions: number }>();
  for (const line of lines.split("\n")) {
    if (!line.trim()) continue;
    const parts = line.split("\t");
    if (parts.length < 3) continue;
    const rawPath = parts.slice(2).join("\t");
    const path = normalizeRepoPath(rawPath);
    const add = Number(parts[0]) || 0;
    const del = Number(parts[1]) || 0;
    const prev = out.get(path) || { additions: 0, deletions: 0 };
    out.set(path, { additions: prev.additions + add, deletions: prev.deletions + del });
  }
  return out;
}

function mergeNumstat(into: Map<string, { additions: number; deletions: number }>, next?: string): void {
  if (!next) return;
  const parsed = parseNumstat(next);
  for (const [path, value] of parsed.entries()) {
    const prev = into.get(path) || { additions: 0, deletions: 0 };
    into.set(path, {
      additions: prev.additions + value.additions,
      deletions: prev.deletions + value.deletions,
    });
  }
}

function mergeStatus(into: Map<string, DiffProofFile["status"]>, output?: string): void {
  if (!output) return;
  for (const line of output.split("\n")) {
    if (!line.trim()) continue;
    const parts = line.split("\t");
    const code = (parts[0] || "").trim();
    const lastPath = normalizeRepoPath(parts[parts.length - 1] || "");
    if (!lastPath) continue;
    const nextStatus: DiffProofFile["status"] =
      code.startsWith("A") ? "added" :
        code.startsWith("D") ? "deleted" :
          code.startsWith("R") ? "renamed" : "modified";
    into.set(lastPath, nextStatus);
  }
}

function buildManifestFallback(snapshot: TurnSnapshot): DiffProofPayload | undefined {
  if (!snapshot.repoRoot) return undefined;
  const repoRoot = snapshot.repoRoot;
  const endDirty = gitDirtyPaths(repoRoot);
  const candidates = Array.from(new Set([...snapshot.dirtyPathsStart, ...endDirty]));
  const files: DiffProofFile[] = [];
  for (const path of candidates) {
    const abs = join(repoRoot, path);
    const startHash = snapshot.startHashes[path];
    const endHash = safeReadFileHash(abs);
    if (startHash === endHash) continue;
    let status: DiffProofFile["status"] = "modified";
    if (!startHash && endHash) status = "added";
    if (startHash && !endHash) status = "deleted";
    files.push({
      path,
      status,
      additions: 0,
      deletions: 0,
      excerpt: endHash ? safeExcerptFromDiff(repoRoot, path) : undefined,
    });
  }
  return {
    summary: {
      files_changed: files.length,
      additions: 0,
      deletions: 0,
    },
    files,
    repo: {
      vcs: "git",
      head_start: snapshot.headStart,
      head_end: headSha(repoRoot),
      dirty_at_start: true,
      source: "manifest_fallback",
    },
    generated_at: new Date().toISOString(),
  };
}

function buildGitProof(snapshot: TurnSnapshot): DiffProofPayload | undefined {
  if (!snapshot.repoRoot) return undefined;
  const repoRoot = snapshot.repoRoot;
  const headEnd = headSha(repoRoot);
  const numstat = new Map<string, { additions: number; deletions: number }>();
  const status = new Map<string, DiffProofFile["status"]>();

  mergeNumstat(numstat, runGit(repoRoot, ["diff", "--numstat"]));
  mergeNumstat(numstat, runGit(repoRoot, ["diff", "--cached", "--numstat"]));
  mergeStatus(status, runGit(repoRoot, ["diff", "--name-status"]));
  mergeStatus(status, runGit(repoRoot, ["diff", "--cached", "--name-status"]));

  if (snapshot.headStart && headEnd && snapshot.headStart !== headEnd) {
    const range = `${snapshot.headStart}..${headEnd}`;
    mergeNumstat(numstat, runGit(repoRoot, ["diff", "--numstat", range]));
    mergeStatus(status, runGit(repoRoot, ["diff", "--name-status", range]));
  }

  const untracked = (runGit(repoRoot, ["ls-files", "--others", "--exclude-standard"]) || "")
    .split("\n")
    .map((line) => normalizeRepoPath(line.trim()))
    .filter(Boolean);
  for (const path of untracked) {
    if (!status.has(path)) status.set(path, "added");
    if (!numstat.has(path)) numstat.set(path, { additions: 0, deletions: 0 });
  }

  const files: DiffProofFile[] = [];
  const allPaths = new Set<string>([...numstat.keys(), ...status.keys()]);
  for (const path of allPaths) {
    const counts = numstat.get(path) || { additions: 0, deletions: 0 };
    const range = snapshot.headStart && headEnd && snapshot.headStart !== headEnd ? `${snapshot.headStart}..${headEnd}` : undefined;
    files.push({
      path,
      status: status.get(path) || "modified",
      additions: counts.additions,
      deletions: counts.deletions,
      excerpt: safeExcerptFromDiff(repoRoot, path, range),
    });
  }

  const additions = files.reduce((acc, file) => acc + file.additions, 0);
  const deletions = files.reduce((acc, file) => acc + file.deletions, 0);

  return {
    summary: {
      files_changed: files.length,
      additions,
      deletions,
    },
    files,
    repo: {
      vcs: "git",
      head_start: snapshot.headStart,
      head_end: headEnd,
      dirty_at_start: snapshot.dirtyAtStart,
      source: "git",
    },
    generated_at: new Date().toISOString(),
  };
}

export function startTurnSnapshot(cwd: string, turnId: string): void {
  cleanupSnapshots(cwd);
  const dir = turnsDir(cwd);
  mkdirSync(dir, { recursive: true });
  const path = snapshotPath(cwd, turnId);
  const repoRoot = resolveRepoRoot(cwd);
  const dirtyPaths = repoRoot ? gitDirtyPaths(repoRoot) : [];
  const startHashes: Record<string, string> = {};
  if (repoRoot && dirtyPaths.length > 0) {
    for (const relPath of dirtyPaths) {
      const abs = join(repoRoot, relPath);
      const hash = safeReadFileHash(abs);
      if (hash) startHashes[relPath] = hash;
    }
  }

  const snapshot: TurnSnapshot = {
    version: 1,
    turnId,
    cwd: resolve(cwd),
    repoRoot: repoRoot ? resolve(repoRoot) : undefined,
    startedAt: new Date().toISOString(),
    headStart: repoRoot ? headSha(repoRoot) : undefined,
    dirtyAtStart: dirtyPaths.length > 0,
    dirtyPathsStart: dirtyPaths,
    startHashes,
  };
  writeFileSync(path, JSON.stringify(snapshot, null, 2));
}

export function closeTurnSnapshot(cwd: string, turnId: string): {
  proof?: DiffProofPayload;
  found: boolean;
  reason?: string;
} {
  const path = snapshotPath(cwd, turnId);
  if (!existsSync(path)) {
    return { found: false, reason: "turn_snapshot_missing" };
  }

  try {
    const snapshot = JSON.parse(readFileSync(path, "utf8")) as TurnSnapshot;
    const proof = snapshot.dirtyAtStart ? buildManifestFallback(snapshot) : buildGitProof(snapshot);
    rmSync(path, { force: true });
    return {
      found: true,
      proof,
      reason: proof ? undefined : "no_git_repository",
    };
  } catch {
    rmSync(path, { force: true });
    return { found: false, reason: "turn_snapshot_corrupt" };
  }
}

export function proofBytes(proof?: DiffProofPayload): number {
  return Buffer.byteLength(JSON.stringify(proof || {}), "utf8");
}
