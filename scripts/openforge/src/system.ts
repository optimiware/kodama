import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { dirname, isAbsolute, resolve } from "node:path";

export class Paused extends Error {
  constructor(message: string) {
    super(message);
    this.name = "Paused";
  }
}

export class Park extends Error {
  constructor(message: string) {
    super(message);
    this.name = "Park";
  }
}

export function hash(text: string | Uint8Array): string {
  return createHash("sha256").update(text).digest("hex");
}

export function canonicalJSON(value: unknown): string {
  const sort = (item: unknown): unknown => {
    if (Array.isArray(item)) return item.map(sort);
    if (item && typeof item === "object") {
      return Object.fromEntries(
        Object.keys(item as Record<string, unknown>)
          .sort()
          .map((k) => [k, sort((item as Record<string, unknown>)[k])]),
      );
    }
    return item;
  };
  return JSON.stringify(sort(value));
}

export function readJSON<T>(path: string, fallback?: T): T {
  if (!existsSync(path)) {
    if (fallback !== undefined) return fallback;
    throw new Error(`Missing JSON file: ${path}`);
  }
  return JSON.parse(readFileSync(path, "utf8")) as T;
}

export function writeJSON(path: string, value: unknown): void {
  mkdirSync(dirname(path), { recursive: true });
  const tmp = `${path}.${process.pid}.${Date.now()}.tmp`;
  writeFileSync(tmp, `${JSON.stringify(value, null, 2)}\n`);
  renameSync(tmp, path);
}

export function isPidAlive(pid: number): boolean {
  if (!Number.isInteger(pid) || pid <= 0) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === "EPERM";
  }
}

export function lockHolder(path: string): { pid: number; alive: boolean } | null {
  if (!existsSync(path)) return null;
  try {
    const raw = readFileSync(path, "utf8").trim();
    if (!raw) return null;
    const info = JSON.parse(raw) as { pid?: number };
    if (typeof info.pid !== "number") return null;
    return { pid: info.pid, alive: isPidAlive(info.pid) };
  } catch {
    return null;
  }
}

export class FileLock {
  private released = false;
  private constructor(readonly path: string) {}

  static tryAcquire(path: string): FileLock | null {
    mkdirSync(dirname(path), { recursive: true });
    for (let attempt = 0; attempt < 2; attempt++) {
      try {
        const fd = openSync(path, "wx");
        try {
          writeFileSync(fd, JSON.stringify({ pid: process.pid, startedAt: Date.now() }));
        } finally {
          closeSync(fd);
        }
        return new FileLock(path);
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err;
        try {
          const raw = readFileSync(path, "utf8").trim();
          if (!raw) {
            if (Date.now() - statSync(path).mtimeMs > 1000) {
              unlinkSync(path);
              continue;
            }
          } else {
            const info = JSON.parse(raw) as { pid?: number };
            if (typeof info.pid === "number" && !isPidAlive(info.pid)) {
              unlinkSync(path);
              continue;
            }
          }
        } catch {
          // Concurrent lock holder writing or removing; treat as held.
        }
        return null;
      }
    }
    return null;
  }

  release(): void {
    if (this.released) return;
    this.released = true;
    try {
      rmSync(this.path, { force: true });
    } catch {
      // Ignore cleanup race.
    }
  }
}

export async function withLock<T>(
  path: string,
  fn: () => Promise<T>,
  options: { wait?: boolean; signal?: AbortSignal; haltFile?: string } = {},
): Promise<T> {
  const wait = options.wait ?? true;
  for (;;) {
    if (options.signal?.aborted || (options.haltFile && existsSync(options.haltFile))) {
      throw new Paused("Queue halted");
    }
    const lock = FileLock.tryAcquire(path);
    if (lock) {
      try {
        return await fn();
      } finally {
        lock.release();
      }
    }
    if (!wait) throw new Error(`Lock busy: ${path}`);
    await new Promise((r) => setTimeout(r, 25));
  }
}

export class SemaphorePool {
  private readonly active = new Map<string, number>();
  private readonly waiters: Array<() => void> = [];

  constructor(private readonly limits: Record<string, number> = {}) {}

  async withResources<T>(names: readonly string[], fn: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    const unique = [...new Set(names)].filter((n) => (this.limits[n] ?? 0) > 0).sort();
    if (unique.length === 0) return fn();

    for (;;) {
      if (signal?.aborted) throw new Paused("Queue halted");
      const canAcquire = unique.every((n) => (this.active.get(n) ?? 0) < (this.limits[n] as number));
      if (canAcquire) {
        for (const n of unique) this.active.set(n, (this.active.get(n) ?? 0) + 1);
        break;
      }
      await new Promise<void>((resolve) => {
        const wake = () => {
          signal?.removeEventListener("abort", wake);
          resolve();
        };
        signal?.addEventListener("abort", wake, { once: true });
        this.waiters.push(wake);
      });
    }

    try {
      return await fn();
    } finally {
      for (const n of unique) {
        const next = (this.active.get(n) ?? 1) - 1;
        if (next <= 0) this.active.delete(n);
        else this.active.set(n, next);
      }
      const pending = this.waiters.splice(0, this.waiters.length);
      for (const wake of pending) wake();
    }
  }
}

export interface RunResult {
  code: number;
  output: string;
  timedOut: boolean;
  cancelled: boolean;
}

export interface RunOptions {
  env?: Record<string, string>;
  timeoutSeconds?: number;
  signal?: AbortSignal;
  haltFile?: string;
  logPath?: string;
}

export function killProcessGroup(pid: number | undefined): void {
  if (!pid || pid <= 0) return;
  try {
    process.kill(-pid, "SIGTERM");
  } catch {
    try {
      process.kill(pid, "SIGTERM");
    } catch {
      return;
    }
  }
  setTimeout(() => {
    try {
      process.kill(-pid, "SIGKILL");
    } catch {
      try {
        process.kill(pid, "SIGKILL");
      } catch {
        // Already exited.
      }
    }
  }, 150).unref?.();
}

export async function runCommand(
  command: string | readonly string[],
  cwd: string,
  options: RunOptions = {},
): Promise<RunResult> {
  const haltFile = options.haltFile ?? options.env?.FORGE_HALT_FILE;
  if (options.signal?.aborted || (haltFile && existsSync(haltFile))) {
    return { code: 130, output: "Queue halted\n", timedOut: false, cancelled: true };
  }
  if (options.logPath) {
    mkdirSync(dirname(options.logPath), { recursive: true });
    writeFileSync(options.logPath, "");
  }

  const isShell = typeof command === "string";
  const file = isShell ? "/bin/sh" : (command[0] as string);
  const args = isShell ? ["-c", command] : command.slice(1);

  return new Promise<RunResult>((resolveResult) => {
    const child = spawn(file, args, {
      cwd,
      env: { ...process.env, ...options.env },
      detached: true,
      stdio: ["ignore", "pipe", "pipe"],
    });

    let output = "";
    let timedOut = false;
    let cancelled = false;
    let finished = false;

    const append = (chunk: Buffer) => {
      const text = chunk.toString("utf8");
      output += text;
      if (options.logPath) {
        try {
          writeFileSync(options.logPath, text, { flag: "a" });
        } catch {
          // Ignore log write failure.
        }
      }
    };

    child.stdout?.on("data", append);
    child.stderr?.on("data", append);

    const stop = (reason: "timeout" | "cancel") => {
      if (finished) return;
      if (reason === "timeout") timedOut = true;
      else cancelled = true;
      killProcessGroup(child.pid);
    };

    const timer =
      options.timeoutSeconds && options.timeoutSeconds > 0
        ? setTimeout(() => stop("timeout"), options.timeoutSeconds * 1000)
        : null;

    const haltPoll = haltFile
      ? setInterval(() => {
          if (existsSync(haltFile)) stop("cancel");
        }, 100)
      : null;

    const onAbort = () => stop("cancel");
    options.signal?.addEventListener("abort", onAbort, { once: true });

    const cleanup = () => {
      finished = true;
      if (timer) clearTimeout(timer);
      if (haltPoll) clearInterval(haltPoll);
      options.signal?.removeEventListener("abort", onAbort);
    };

    child.on("error", (err) => {
      cleanup();
      append(Buffer.from(`${err.message}\n`));
      resolveResult({ code: 127, output, timedOut, cancelled });
    });

    child.on("close", (code) => {
      cleanup();
      killProcessGroup(child.pid);
      resolveResult({
        code: timedOut ? 124 : cancelled ? 130 : (code ?? 1),
        output,
        timedOut,
        cancelled,
      });
    });
  });
}

export async function gitRaw(cwd: string, ...args: string[]): Promise<string> {
  const proc = Bun.spawnSync(["git", ...args], { cwd, stdout: "pipe", stderr: "pipe" });
  if (proc.exitCode !== 0) {
    const err = proc.stderr.toString("utf8").trim() || proc.stdout.toString("utf8").trim();
    throw new Error(`git ${args.join(" ")} failed (${proc.exitCode}): ${err}`);
  }
  return proc.stdout.toString("utf8");
}

export async function git(cwd: string, ...args: string[]): Promise<string> {
  return (await gitRaw(cwd, ...args)).trim();
}

export async function primaryRoot(cwd: string): Promise<string> {
  const common = await git(cwd, "rev-parse", "--git-common-dir");
  const abs = isAbsolute(common) ? common : resolve(cwd, common);
  return dirname(abs);
}

export async function currentBranch(cwd: string): Promise<string> {
  return git(cwd, "rev-parse", "--abbrev-ref", "HEAD");
}

export async function head(cwd: string): Promise<string> {
  return git(cwd, "rev-parse", "HEAD");
}

function isIgnoredWorktreePath(p: string): boolean {
  return p === ".harness" || p.startsWith(".harness/") || p === ".work" || p.startsWith(".work/");
}

export async function dirtyFiles(cwd: string): Promise<string[]> {
  const tracked = await gitRaw(cwd, "diff", "--no-renames", "--name-only", "-z", "HEAD");
  const staged = await gitRaw(cwd, "diff", "--cached", "--no-renames", "--name-only", "-z", "HEAD");
  const untracked = await gitRaw(cwd, "ls-files", "--others", "--exclude-standard", "-z");
  const files = new Set<string>();
  for (const raw of `${tracked}\0${staged}\0${untracked}`.split("\0")) {
    if (raw && !isIgnoredWorktreePath(raw)) files.add(raw);
  }
  return [...files].sort();
}

export async function isClean(cwd: string): Promise<boolean> {
  return (await dirtyFiles(cwd)).length === 0;
}

export async function changedFiles(cwd: string, base: string): Promise<string[]> {
  const committed = await gitRaw(cwd, "diff", "--no-renames", "--name-only", "-z", `${base}...HEAD`);
  const dirty = await dirtyFiles(cwd);
  const files = new Set<string>(dirty);
  for (const raw of committed.split("\0")) {
    if (raw && !isIgnoredWorktreePath(raw)) files.add(raw);
  }
  return [...files].sort();
}

export async function patchId(cwd: string, base: string): Promise<string> {
  // Unlike git patch-id, this retains whitespace in strings and indentation.
  // Fixed diff options also avoid user configuration changing the receipt.
  const diff = Bun.spawnSync(
    [
      "git",
      "diff",
      "--binary",
      "--full-index",
      "--no-renames",
      "--no-ext-diff",
      "--no-textconv",
      "--no-color",
      "--src-prefix=a/",
      "--dst-prefix=b/",
      "--unified=3",
      `${base}...HEAD`,
    ],
    {
      cwd,
      stdout: "pipe",
      stderr: "pipe",
    },
  );
  if (diff.exitCode !== 0) {
    throw new Error(`git diff ${base}...HEAD failed: ${diff.stderr.toString("utf8").trim()}`);
  }
  return `sha256:${hash(diff.stdout)}`;
}
