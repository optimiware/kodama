import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseConfig } from "../src/config.ts";
import {
  checkScope,
  handoffSchema,
  planSchema,
  reviewSchema,
  runTaskCheck,
  verifyAcceptanceResults,
} from "../src/gate.ts";
import { changedFiles, git, SemaphorePool } from "../src/system.ts";
import type { Task } from "../src/tasks.ts";

async function makeGitRepo(): Promise<string> {
  const dir = mkdtempSync(join(tmpdir(), "openforge-gate-"));
  await git(dir, "init", "-b", "main");
  await git(dir, "config", "user.name", "Test");
  await git(dir, "config", "user.email", "test@example.com");
  writeFileSync(join(dir, "README.md"), "# test\n");
  await git(dir, "add", "README.md");
  await git(dir, "commit", "-m", "initial");
  return dir;
}

const dummyTask = (overrides: Partial<Task> = {}): Task => ({
  id: "T-100",
  path: "docs/tasks/T-100.md",
  slug: "T-100",
  milestone: "M1",
  status: "planned",
  dependsOn: [],
  blockers: [],
  owns: ["src/"],
  acceptance: [],
  resources: [],
  plan: false,
  review: "agent",
  title: "T-100 — Test",
  body: "",
  unknownKeys: [],
  ...overrides,
});

describe("gates and schemas", () => {
  test("checkScope enforces owned and protected paths", () => {
    const config = parseConfig({ protected: ["lock.json"] });
    const task = dummyTask({ owns: ["src/"] });
    expect(checkScope(["src/index.ts"], task, config)).toEqual([]);
    expect(checkScope(["other/file.ts"], task, config)).toEqual(["unowned path modified: other/file.ts"]);
    expect(checkScope(["lock.json"], task, config)).toEqual([
      "protected path modified without ownership: lock.json",
    ]);
    const ownerTask = dummyTask({ owns: ["src/", "lock.json"] });
    expect(checkScope(["lock.json"], ownerTask, config)).toEqual([]);
  });

  test("verifyAcceptanceResults matches exact and normalized names and rejects skipped or failed", () => {
    expect(
      verifyAcceptanceResults(["unit_dispatch_replay", "cancel_fencing"], {
        passed: ["Test_unit_dispatch_replay", "TestCancelFencing"],
        failed: [],
        skipped: [],
      }),
    ).toEqual([]);

    expect(
      verifyAcceptanceResults(["unit_dispatch_replay", "missing_one"], {
        passed: ["Test_unit_dispatch_replay"],
        failed: [],
        skipped: ["Test_unit_dispatch_replay"],
      }),
    ).toEqual(["unit_dispatch_replay (failed or skipped)", "missing_one"]);
  });

  test("runTaskCheck routes exit 0, exit 75 (infra_pause), timeout, and FORGE_RESULTS validation", async () => {
    const dir = await makeGitRepo();
    try {
      const config = parseConfig({ base: "main", limits: { checkTimeoutSeconds: 1 } });
      const pool = new SemaphorePool();
      const home = join(dir, ".work/.harness");

      writeFileSync(join(dir, "src_file.txt"), "hello\n");
      const badScope = await runTaskCheck(dir, home, dummyTask({ owns: ["src/"] }), config, pool);
      expect(badScope.status).toBe("failed");

      rmSync(join(dir, "src_file.txt"));

      // Exit 75 -> infra_pause
      const pauseOut = await runTaskCheck(
        dir,
        home,
        dummyTask({ check: "echo 'env down' && exit 75" }),
        config,
        pool,
      );
      expect(pauseOut.status).toBe("infra_pause");

      // Missing FORGE_RESULTS when acceptance is declared -> setup_error
      const setupErr = await runTaskCheck(
        dir,
        home,
        dummyTask({ check: "true", acceptance: ["my_test"] }),
        config,
        pool,
      );
      expect(setupErr.status).toBe("setup_error");

      // Valid FORGE_RESULTS -> passed
      const passOut = await runTaskCheck(
        dir,
        home,
        dummyTask({
          check: `printf '{"passed":["TestMyTest"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"`,
          acceptance: ["my_test"],
        }),
        config,
        pool,
      );
      expect(passOut.status).toBe("passed");

      // Make-style wrapper exiting 2 after writing infra_pause to FORGE_RESULTS -> infra_pause
      const makePauseOut = await runTaskCheck(
        dir,
        home,
        dummyTask({
          check: `printf '{"infra_pause":"podman unavailable","passed":[],"failed":[],"skipped":[]}' > "$FORGE_RESULTS" && exit 2`,
          acceptance: ["my_test"],
        }),
        config,
        pool,
      );
      expect(makePauseOut.status).toBe("infra_pause");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("changedFiles detects committed renames of protected files, unstaged edits, and paths with spaces", async () => {
    const dir = await makeGitRepo();
    try {
      const config = parseConfig({ base: "main", protected: ["go.mod"] });
      writeFileSync(join(dir, "go.mod"), "module example\n");
      await git(dir, "add", "go.mod");
      await git(dir, "commit", "-m", "add go.mod");

      await git(dir, "checkout", "-b", "task/rename-test");
      await git(dir, "mv", "-f", "go.mod", "README.md");
      await git(dir, "commit", "-m", "move protected file into owned path");

      // Must include both source (go.mod) and destination (README.md)
      const moved = await changedFiles(dir, "main");
      expect(moved).toEqual(["README.md", "go.mod"]);
      const problems = checkScope(moved, dummyTask({ owns: ["README.md"] }), config);
      expect(problems).toEqual(["protected path modified without ownership: go.mod"]);

      // Unstaged tracked modification and path with spaces must not lose leading characters
      writeFileSync(join(dir, "README.md"), "# updated\n");
      writeFileSync(join(dir, "file with spaces.txt"), "hello\n");
      const withDirty = await changedFiles(dir, "main");
      expect(withDirty).toContain("README.md");
      expect(withDirty).toContain("file with spaces.txt");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("report schemas validate plan, handoff, and review constraints", () => {
    expect(planSchema.parse({ markdown: "# Plan" }).blockers).toEqual([]);
    expect(handoffSchema.parse({ summary: "Done", tests_run: ["make test"] }).blockers).toEqual([]);

    expect(() =>
      reviewSchema.parse({
        verdict: "approve",
        summary: "Looks good",
        findings: [{ severity: "p1", file: "a.ts", issue: "broken" }],
      }),
    ).toThrow();

    expect(() =>
      reviewSchema.parse({
        verdict: "changes_requested",
        summary: "Needs work",
        findings: [{ severity: "nit", file: "a.ts", issue: "typo" }],
      }),
    ).toThrow();

    const validApprove = reviewSchema.parse({
      verdict: "approve",
      summary: "Approved with minor note",
      findings: [{ severity: "nit", file: "a.ts", issue: "style" }],
    });
    expect(validApprove.verdict).toBe("approve");
  });
});
