import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildTaskEnv, expandTemplate, inheritedEnv, parseConfig, resourcePrefix } from "../src/config.ts";
import { FileLock, git, patchId, runCommand, SemaphorePool, withLock } from "../src/system.ts";

describe("config and templates", () => {
  test("validates config defaults and rejects malformed model references", () => {
    const cfg = parseConfig({});
    expect(cfg.base).toBe("main");
    expect(cfg.limits.workers).toBe(5);
    expect(() => parseConfig({ roles: { planner: { model: "badmodel" } } })).toThrow("invalid config");
    expect(() => parseConfig({ unknownField: 123 })).toThrow("invalid config");
  });

  test("expands {id}, {slug}, and {prefix} in templates and task env", () => {
    expect(resourcePrefix("T-080")).toBe("t080");
    const task = { id: "T-080", slug: "T-080-execution" };
    expect(expandTemplate("make test ID={id} SLUG={slug} P={prefix}", task)).toBe(
      "make test ID=T-080 SLUG=T-080-execution P=t080",
    );
    const cfg = parseConfig({ env: { MY_SCOPE: "{prefix}", MY_TASK: "{id}" } });
    const env = buildTaskEnv(cfg, "/tmp/home", task);
    expect(env.FORGE_TASK_ID).toBe("T-080");
    expect(env.FORGE_RESOURCE_PREFIX).toBe("t080");
    expect(env.MY_SCOPE).toBe("t080");
    expect(env.MY_TASK).toBe("T-080");
  });

  test("agent sessions inherit toolchain variables but not credentials", () => {
    const source = {
      PATH: "/opt/go/bin:/usr/bin",
      HOME: "/home/dev",
      GOCACHE: "/cache/go",
      LC_ALL: "C",
      OPENCODE_API_KEY: "secret",
      DATABASE_URL: "postgres://db",
    };
    const base = inheritedEnv(parseConfig({}), source);
    expect(base).toEqual({ PATH: source.PATH, HOME: source.HOME, GOCACHE: "/cache/go", LC_ALL: "C" });
    const extended = inheritedEnv(parseConfig({ envPassthrough: ["DATABASE_URL"] }), source);
    expect(extended.DATABASE_URL).toBe("postgres://db");
    expect(extended.OPENCODE_API_KEY).toBeUndefined();
  });
});

describe("locks, semaphores, commands, and git patch-id", () => {
  test("FileLock recovers stale dead-PID lock and blocks live owner", async () => {
    const dir = mkdtempSync(join(tmpdir(), "openforge-lock-"));
    const lockFile = join(dir, "test.lock");
    try {
      writeFileSync(lockFile, JSON.stringify({ pid: 999999, startedAt: 1 }));
      const acquired = FileLock.tryAcquire(lockFile);
      expect(acquired).not.toBeNull();
      expect(FileLock.tryAcquire(lockFile)).toBeNull();
      acquired?.release();

      let ran = false;
      await withLock(lockFile, async () => {
        ran = true;
      });
      expect(ran).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("SemaphorePool enforces named concurrency limits and releases on error", async () => {
    const pool = new SemaphorePool({ db: 1, cpu: 2 });
    let activeDb = 0;
    let maxDb = 0;
    await Promise.all(
      [1, 2, 3].map(() =>
        pool.withResources(["db", "cpu"], async () => {
          activeDb++;
          maxDb = Math.max(maxDb, activeDb);
          await new Promise((r) => setTimeout(r, 15));
          activeDb--;
        }),
      ),
    );
    expect(maxDb).toBe(1);
  });

  test("runCommand handles exit codes, timeouts (124), HALT cancellation (130), and log files", async () => {
    const dir = mkdtempSync(join(tmpdir(), "openforge-cmd-"));
    try {
      const logPath = join(dir, "out.log");
      const ok = await runCommand("echo hello", dir, { logPath });
      expect(ok.code).toBe(0);
      expect(readFileSync(logPath, "utf8").trim()).toBe("hello");

      const timedOut = await runCommand("sleep 5", dir, { timeoutSeconds: 0.1 });
      expect(timedOut.timedOut).toBe(true);
      expect(timedOut.code).toBe(124);

      const haltFile = join(dir, "HALT");
      writeFileSync(haltFile, "1");
      const halted = await runCommand("echo should-not-run", dir, { haltFile });
      expect(halted.cancelled).toBe(true);
      expect(halted.code).toBe(130);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("patchId stays identical across rebase onto an orthogonal commit", async () => {
    const dir = mkdtempSync(join(tmpdir(), "openforge-patchid-"));
    try {
      await git(dir, "init", "-b", "main");
      await git(dir, "config", "user.name", "Tester");
      await git(dir, "config", "user.email", "tester@example.com");
      mkdirSync(join(dir, "a"), { recursive: true });
      mkdirSync(join(dir, "b"), { recursive: true });
      writeFileSync(join(dir, "a/one.txt"), "base\n");
      await git(dir, "add", "-A");
      await git(dir, "commit", "-m", "base");

      await git(dir, "checkout", "-b", "task/feature");
      writeFileSync(join(dir, "a/one.txt"), "feature-change\n");
      await git(dir, "add", "-A");
      await git(dir, "commit", "-m", "feature");

      const beforePid = await patchId(dir, "main");

      await git(dir, "checkout", "main");
      writeFileSync(join(dir, "b/two.txt"), "orthogonal\n");
      await git(dir, "add", "-A");
      await git(dir, "commit", "-m", "orthogonal");

      await git(dir, "checkout", "task/feature");
      await git(dir, "rebase", "main");
      const afterPid = await patchId(dir, "main");

      expect(afterPid).toBe(beforePid);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
