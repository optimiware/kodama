import { describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseConfig } from "../src/config.ts";
import { type LandingJournal, reconcileLandingJournal } from "../src/git.ts";
import type { AgentRequest, AgentRunOutput, AgentRuntime } from "../src/runtime/sdk.ts";
import { schedule } from "../src/scheduler.ts";
import { loadMainState, loadParked, unparkTask } from "../src/state.ts";
import { FileLock, git, head, writeJSON } from "../src/system.ts";
import { runMainCheck, TaskRunner } from "../src/task-run.ts";
import { loadTasks, READY_END, READY_START, writeIndex } from "../src/tasks.ts";

async function createFixtureRepo() {
  const root = mkdtempSync(join(tmpdir(), "openforge-workflow-"));
  await git(root, "init", "-b", "main");
  await git(root, "config", "user.name", "Tester");
  await git(root, "config", "user.email", "tester@example.com");
  mkdirSync(join(root, "docs/tasks"), { recursive: true });
  mkdirSync(join(root, "pkg/a"), { recursive: true });
  mkdirSync(join(root, "pkg/b"), { recursive: true });
  writeFileSync(join(root, "AGENTS.md"), "# Instructions\n");

  writeFileSync(
    join(root, "docs/tasks/T-001-alpha.md"),
    `---
id: T-001
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [pkg/a/]
check: printf '{"passed":["TestAlpha"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS" && test -f pkg/a/alpha.txt
acceptance: [alpha]
---
# T-001 — Alpha feature
`,
  );

  writeFileSync(
    join(root, "docs/tasks/T-002-beta.md"),
    `---
id: T-002
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [pkg/b/]
check: test -f pkg/b/beta.txt
---
# T-002 — Beta feature
`,
  );

  const readmePath = join(root, "docs/tasks/README.md");
  writeFileSync(
    readmePath,
    `# Tasks\n\n${READY_START}\n${READY_END}\n\n## Dependency index\n\n| Task | Dependencies | Status / decision |\n| --- | --- | --- |\n`,
  );
  const config = parseConfig({
    mainCheck: "true",
    mainCheckEvery: 2,
    mainCheckOnDrain: true,
    resources: { cpu: 2 },
  });
  writeIndex(loadTasks(root), readmePath, false, config);
  writeFileSync(join(root, "openforge.json"), JSON.stringify(config, null, 2));
  await git(root, "add", "-A");
  await git(root, "commit", "-m", "initial");
  return { root, config };
}

describe("end-to-end workflow and scheduler", () => {
  const approvingRuntime: AgentRuntime = {
    async run(req) {
      const sessionID = req.sessionID ?? `ses_${req.task}_${req.role}`;
      req.onSession?.(sessionID);
      if (req.role === "reviewer") {
        return { sessionID, result: { verdict: "approve", summary: "OK", findings: [] } };
      }
      mkdirSync(join(req.worktree, "pkg/b"), { recursive: true });
      writeFileSync(join(req.worktree, "pkg/b/beta.txt"), 'value = "a b"\n');
      return { sessionID, result: { summary: "Implemented", tests_run: [], blockers: [] } };
    },
    async close() {},
  };

  test("rechecks and reviews an approved candidate after a string whitespace amendment", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      const options = {
        primary: root,
        config,
        runtime: approvingRuntime,
        only: new Set(["T-002"]),
        log: () => {},
      };
      await schedule({ ...options, stopAfter: "review" });
      const wt = join(root, ".work/T-002-beta");
      writeFileSync(join(wt, "pkg/b/beta.txt"), 'value = "ab"\n');
      await git(wt, "commit", "-am", "amend string");
      let reviews = 0;
      const runtime: AgentRuntime = {
        ...approvingRuntime,
        async run(req) {
          if (req.role === "reviewer") reviews++;
          return approvingRuntime.run(req);
        },
      };
      await schedule({ ...options, runtime });
      expect(reviews).toBe(1);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("renewed review policy invalidates saved approval on resume", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      const options = {
        primary: root,
        config,
        runtime: approvingRuntime,
        only: new Set(["T-002"]),
        log: () => {},
      };
      await schedule({ ...options, stopAfter: "review" });
      const path = join(root, "docs/tasks/T-002-beta.md");
      writeFileSync(
        path,
        readFileSync(path, "utf8").replace("status: planned", "status: planned\nreview: human"),
      );
      await git(root, "add", "docs/tasks/T-002-beta.md");
      await git(root, "commit", "-m", "require human review");
      const result = await schedule(options);
      expect(result.merged).toEqual([]);
      expect(result.parked["T-002"]).toContain("human review");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("main check rejects the wrong branch, dirty input, and check-induced drift", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      await git(root, "checkout", "-b", "feature");
      expect((await runMainCheck(root, config)).ok).toBe(false);
      await git(root, "checkout", "main");
      writeFileSync(join(root, "AGENTS.md"), "uncommitted fix\n");
      expect((await runMainCheck(root, config)).ok).toBe(false);
      await git(root, "add", "AGENTS.md");
      await git(root, "restore", "--worktree", "--source=HEAD", "AGENTS.md");
      expect((await runMainCheck(root, config)).ok).toBe(false);
      await git(root, "restore", "--staged", "AGENTS.md");
      await git(root, "restore", "AGENTS.md");
      const mutating = { ...config, mainCheck: "echo changed >> AGENTS.md" };
      expect((await runMainCheck(root, mutating)).ok).toBe(false);
      expect(loadMainState(root).lastGreenCommit).toBeNull();
      await git(root, "restore", "AGENTS.md");
      expect(
        (await runMainCheck(root, { ...config, mainCheck: "git commit --allow-empty -m drift" })).ok,
      ).toBe(false);
      expect(loadMainState(root).lastGreenCommit).toBeNull();
      expect((await runMainCheck(root, config)).ok).toBe(true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test.each(["task", "instructions", "legacy"])("renews approval after %s context drift", async (kind) => {
    const { root, config } = await createFixtureRepo();
    try {
      const options = {
        primary: root,
        config,
        runtime: approvingRuntime,
        only: new Set(["T-002"]),
        log: () => {},
      };
      await schedule({ ...options, stopAfter: "review" });
      if (kind === "legacy") {
        const path = join(root, ".work/T-002-beta/.harness/state.json");
        const state = JSON.parse(readFileSync(path, "utf8"));
        delete state.qualificationContext;
        writeJSON(path, state);
      } else {
        const path = kind === "task" ? "docs/tasks/T-002-beta.md" : "AGENTS.md";
        writeFileSync(
          join(root, path),
          `${readFileSync(join(root, path), "utf8")}\nNew review requirement.\n`,
        );
        await git(root, "add", path);
        await git(root, "commit", "-m", "change review context");
      }
      let reviews = 0;
      const runtime: AgentRuntime = {
        ...approvingRuntime,
        async run(req) {
          if (req.role === "reviewer") {
            reviews++;
            if (kind === "instructions")
              expect(readFileSync(join(req.worktree, "AGENTS.md"), "utf8")).toContain(
                "New review requirement",
              );
          }
          return approvingRuntime.run(req);
        },
      };
      const result = await schedule({ ...options, runtime });
      expect(result.merged).toEqual(["T-002"]);
      expect(reviews).toBe(1);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("worker failure cancels and drains peers before releasing the queue lock", async () => {
    const { root, config } = await createFixtureRepo();
    const original = TaskRunner.prototype.step;
    let peerStarted!: () => void;
    const started = new Promise<void>((resolve) => {
      peerStarted = resolve;
    });
    let peerFinished = false;
    let peerCancelled = false;
    let lockHeld = false;
    TaskRunner.prototype.step = async function () {
      if (this.task.id === "T-001") {
        await started;
        throw new Error("git failure");
      }
      peerStarted();
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, 100);
        this.signal?.addEventListener(
          "abort",
          () => {
            clearTimeout(timer);
            resolve();
          },
          { once: true },
        );
      });
      const lock = FileLock.tryAcquire(join(root, ".work/.harness/run.lock"));
      peerCancelled = Boolean(this.signal?.aborted);
      lockHeld = lock === null;
      lock?.release();
      peerFinished = true;
      return "paused";
    };
    try {
      await expect(
        schedule({ primary: root, config, runtime: approvingRuntime, workers: 2 }),
      ).rejects.toThrow("git failure");
      expect(peerFinished).toBe(true);
      expect(peerCancelled).toBe(true);
      expect(lockHeld).toBe(true);
    } finally {
      TaskRunner.prototype.step = original;
      await new Promise((resolve) => setTimeout(resolve, 120));
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("implements, repairs in the same writer session, reviews, lands, and runs periodic mainCheck", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      const sessions: Array<{ task: string; role: string; sessionID: string }> = [];
      let alphaAttempts = 0;
      let reviewAttempts = 0;

      const runtime: AgentRuntime = {
        async run(req: AgentRequest): Promise<AgentRunOutput> {
          const sid = req.sessionID ?? `ses_${req.task}_${req.role}_${sessions.length + 1}`;
          req.onSession?.(sid);
          sessions.push({ task: req.task, role: req.role, sessionID: sid });

          if (req.task === "T-001") {
            if (req.role === "implementer") {
              alphaAttempts++;
              // First attempt forgets to create pkg/a/alpha.txt so check fails and triggers fixer!
              return {
                sessionID: sid,
                result: { summary: "Initial attempt", tests_run: ["check"], blockers: [] },
              };
            }
            if (req.role === "fixer") {
              mkdirSync(join(req.worktree, "pkg/a"), { recursive: true });
              writeFileSync(join(req.worktree, "pkg/a/alpha.txt"), `v${alphaAttempts}\n`);
              return {
                sessionID: sid,
                result: { summary: "Fixed alpha", tests_run: ["check"], blockers: [] },
              };
            }
            if (req.role === "reviewer") {
              reviewAttempts++;
              if (reviewAttempts === 1) {
                return {
                  sessionID: sid,
                  result: {
                    verdict: "changes_requested",
                    summary: "Needs update",
                    findings: [{ severity: "major", file: "pkg/a/alpha.txt", issue: "update content" }],
                  },
                };
              }
              return {
                sessionID: sid,
                result: { verdict: "approve", summary: "LGTM", findings: [] },
              };
            }
          }

          if (req.task === "T-002") {
            if (req.role === "implementer") {
              mkdirSync(join(req.worktree, "pkg/b"), { recursive: true });
              writeFileSync(join(req.worktree, "pkg/b/beta.txt"), "beta\n");
              return {
                sessionID: sid,
                result: { summary: "Implemented beta", tests_run: ["check"], blockers: [] },
              };
            }
            if (req.role === "reviewer") {
              return {
                sessionID: sid,
                result: { verdict: "approve", summary: "Approved beta", findings: [] },
              };
            }
          }
          throw new Error(`Unexpected role ${req.role} for ${req.task}`);
        },
        async close() {},
      };

      const summary = await schedule({
        primary: root,
        config,
        runtime,
        workers: 2,
        log: () => {},
      });

      expect(summary.merged.sort()).toEqual(["T-001", "T-002"]);
      expect(existsSync(join(root, "pkg/a/alpha.txt"))).toBe(true);
      expect(existsSync(join(root, "pkg/b/beta.txt"))).toBe(true);

      // Verify fixer reused the exact same writerSessionID as implementer for T-001
      const t1WriterSessions = sessions
        .filter((s) => s.task === "T-001" && (s.role === "implementer" || s.role === "fixer"))
        .map((s) => s.sessionID);
      expect(t1WriterSessions.length).toBe(3); // 1 implementer + 1 check-fixer + 1 review-fixer
      expect(new Set(t1WriterSessions).size).toBe(1);

      // Verify reviewer reused the exact same reviewerSessionID across rounds
      const t1ReviewerSessions = sessions
        .filter((s) => s.task === "T-001" && s.role === "reviewer")
        .map((s) => s.sessionID);
      expect(t1ReviewerSessions.length).toBe(2);
      expect(new Set(t1ReviewerSessions).size).toBe(1);

      // Both task worktrees were cleaned up after landing, and archive exists
      expect(existsSync(join(root, ".work/T-001-alpha"))).toBe(false);
      expect(existsSync(join(root, ".work/T-002-beta"))).toBe(false);
      expect(existsSync(join(root, ".work/.harness/archive/T-001-alpha/review.json"))).toBe(true);

      // Tasks are marked done in main and mainCheck ran after 2 merges
      const updatedTasks = loadTasks(root);
      expect(updatedTasks.get("T-001")?.status).toBe("done");
      expect(updatedTasks.get("T-002")?.status).toBe("done");
      const mainState = loadMainState(root);
      expect(mainState.blocked).toBe(false);
      expect(mainState.lastGreenCommit).toBeTruthy();
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("parks on blocker and unpark resets state for retry", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      const runtime: AgentRuntime = {
        async run(req: AgentRequest): Promise<AgentRunOutput> {
          const sid = req.sessionID ?? "ses_1";
          req.onSession?.(sid);
          return {
            sessionID: sid,
            result: { summary: "Blocked", tests_run: [], blockers: ["Need spec decision"] },
          };
        },
        async close() {},
      };

      const summary = await schedule({
        primary: root,
        config,
        runtime,
        only: new Set(["T-002"]),
        log: () => {},
      });
      expect(summary.parked["T-002"]).toContain("Need spec decision");

      await unparkTask(root, "T-002");
      expect(loadParked(root)["T-002"]).toBeUndefined();
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("supports opt-in planner (`plan: true`), `review: human`, and infra_pause without repair charge", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      writeFileSync(
        join(root, "docs/tasks/T-001-alpha.md"),
        `---
id: T-001
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [pkg/a/]
plan: true
review: human
check: true
---
# T-001 — Alpha with plan and human review
`,
      );
      writeFileSync(
        join(root, "docs/tasks/T-002-beta.md"),
        `---
id: T-002
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [pkg/b/]
check: exit 75
---
# T-002 — Beta with unavailable infra
`,
      );
      writeIndex(loadTasks(root), join(root, "docs/tasks/README.md"), false, config);
      await git(root, "add", "-A");
      await git(root, "commit", "-m", "update tasks");

      const rolesRun: string[] = [];
      const runtime: AgentRuntime = {
        async run(req: AgentRequest): Promise<AgentRunOutput> {
          const sid = req.sessionID ?? `ses_${req.role}`;
          req.onSession?.(sid);
          rolesRun.push(`${req.task}:${req.role}`);
          if (req.role === "planner") {
            return { sessionID: sid, result: { markdown: "# Step 1\nDo it.", blockers: [] } };
          }
          if (req.task === "T-001") {
            mkdirSync(join(req.worktree, "pkg/a"), { recursive: true });
            writeFileSync(join(req.worktree, "pkg/a/alpha.txt"), "done\n");
          } else {
            mkdirSync(join(req.worktree, "pkg/b"), { recursive: true });
            writeFileSync(join(req.worktree, "pkg/b/beta.txt"), "done\n");
          }
          return { sessionID: sid, result: { summary: "Done", tests_run: ["true"], blockers: [] } };
        },
        async close() {},
      };

      const summary = await schedule({
        primary: root,
        config,
        runtime,
        workers: 2,
        log: () => {},
      });

      expect(rolesRun).toContain("T-001:planner");
      expect(rolesRun).toContain("T-001:implementer");
      expect(summary.parked["T-001"]).toContain("human review");
      expect(summary.paused).toContain("T-002");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("honors --only with workers=1 on lower-priority task and terminates cleanly on --stop-after", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      // Dry-run with workers=1 and --only T-002 must select T-002 even though T-001 sorts first
      const dry = await schedule({
        primary: root,
        config,
        runtime: {
          async run() {
            throw new Error("unused");
          },
          async close() {},
        },
        workers: 1,
        only: new Set(["T-002"]),
        dryRun: true,
        log: () => {},
      });
      expect(dry.selected).toEqual(["T-002"]);

      // Live run with --stop-after review and workers=1 on T-002 must terminate without merging
      const runtime: AgentRuntime = {
        async run(req: AgentRequest): Promise<AgentRunOutput> {
          const sid = req.sessionID ?? `ses_${req.role}`;
          req.onSession?.(sid);
          if (req.role === "implementer") {
            mkdirSync(join(req.worktree, "pkg/b"), { recursive: true });
            writeFileSync(join(req.worktree, "pkg/b/beta.txt"), "beta\n");
            return { sessionID: sid, result: { summary: "Done", tests_run: ["check"], blockers: [] } };
          }
          return { sessionID: sid, result: { verdict: "approve", summary: "OK", findings: [] } };
        },
        async close() {},
      };

      const stopped = await schedule({
        primary: root,
        config,
        runtime,
        workers: 1,
        only: new Set(["T-002"]),
        stopAfter: "review",
        log: () => {},
      });
      expect(stopped.selected).toEqual(["T-002"]);
      expect(stopped.merged).toEqual([]);
      expect(existsSync(join(root, ".work/T-002-beta/.harness/review.json"))).toBe(true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("rejects post-rebase check that mutates owned files during landing", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      // T-002's check creates an untracked file only when main has advanced (T-001 already merged)
      writeFileSync(
        join(root, "docs/tasks/T-002-beta.md"),
        `---
id: T-002
milestone: M1
status: planned
depends_on: []
decision_blockers: []
owns: [pkg/b/]
check: if test -f pkg/a/alpha.txt; then echo sneak > pkg/b/sneak.txt; fi && test -f pkg/b/beta.txt
---
# T-002 — Beta feature
`,
      );
      writeIndex(loadTasks(root), join(root, "docs/tasks/README.md"), false, config);
      await git(root, "add", "-A");
      await git(root, "commit", "-m", "update T-002 check");

      const runtime: AgentRuntime = {
        async run(req: AgentRequest): Promise<AgentRunOutput> {
          const sid = req.sessionID ?? `ses_${req.task}_${req.role}`;
          req.onSession?.(sid);
          if (req.role === "implementer") {
            if (req.task === "T-001") {
              mkdirSync(join(req.worktree, "pkg/a"), { recursive: true });
              writeFileSync(join(req.worktree, "pkg/a/alpha.txt"), "alpha\n");
            } else {
              mkdirSync(join(req.worktree, "pkg/b"), { recursive: true });
              writeFileSync(join(req.worktree, "pkg/b/beta.txt"), "beta\n");
            }
            return { sessionID: sid, result: { summary: "Done", tests_run: ["check"], blockers: [] } };
          }
          return { sessionID: sid, result: { verdict: "approve", summary: "OK", findings: [] } };
        },
        async close() {},
      };

      // Stop T-002 at review first so its verifiedBaseCommit is before T-001 lands
      await schedule({
        primary: root,
        config,
        runtime,
        workers: 1,
        only: new Set(["T-002"]),
        stopAfter: "review",
        log: () => {},
      });
      // Merge T-001 so main moves forward
      await schedule({
        primary: root,
        config,
        runtime,
        workers: 1,
        only: new Set(["T-001"]),
        log: () => {},
      });
      // Now attempt to land T-002: post-rebase check creates pkg/b/sneak.txt, which must fail re-verification
      const summary = await schedule({
        primary: root,
        config: parseConfig({ ...config, limits: { ...config.limits, repairs: 0 } }),
        runtime,
        workers: 1,
        only: new Set(["T-002"]),
        log: () => {},
      });
      expect(summary.merged).toEqual([]);
      expect(existsSync(join(root, "pkg/b/sneak.txt"))).toBe(false);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("gates concurrent landings at mainCheckEvery inside publication.lock and unblocks dependent tasks on primary", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      // Make T-003 depend on T-002, and set mainCheckEvery = 1
      writeFileSync(
        join(root, "docs/tasks/T-003-gamma.md"),
        `---
id: T-003
milestone: M1
status: planned
depends_on: [T-002]
decision_blockers: []
owns: [pkg/c/]
check: test -f pkg/c/gamma.txt
---
# T-003 — Gamma dependent feature
`,
      );
      const strictConfig = parseConfig({
        ...config,
        mainCheck: "echo main-check-ok >> .work/.harness/main-runs.txt",
        mainCheckEvery: 1,
        mainCheckOnDrain: false,
      });
      writeIndex(loadTasks(root), join(root, "docs/tasks/README.md"), false, strictConfig);
      await git(root, "add", "-A");
      await git(root, "commit", "-m", "add T-003");

      // Create a separate caller checkout with stale task files to prove live runs read primary
      const staleDir = mkdtempSync(join(tmpdir(), "openforge-stale-caller-"));

      const runtime: AgentRuntime = {
        async run(req: AgentRequest): Promise<AgentRunOutput> {
          const sid = req.sessionID ?? `ses_${req.task}_${req.role}`;
          req.onSession?.(sid);
          if (req.role === "implementer") {
            const sub =
              req.task === "T-001" ? "a/alpha.txt" : req.task === "T-002" ? "b/beta.txt" : "c/gamma.txt";
            mkdirSync(join(req.worktree, "pkg", sub.split("/")[0] ?? ""), { recursive: true });
            writeFileSync(join(req.worktree, "pkg", sub), `${req.task}\n`);
            return { sessionID: sid, result: { summary: "Done", tests_run: ["check"], blockers: [] } };
          }
          return { sessionID: sid, result: { verdict: "approve", summary: "OK", findings: [] } };
        },
        async close() {},
      };

      // Pre-approve both T-001 and T-002 so both try to land concurrently with mainCheckEvery = 1
      await schedule({
        primary: root,
        config: strictConfig,
        runtime,
        workers: 2,
        only: new Set(["T-001", "T-002"]),
        stopAfter: "review",
        log: () => {},
      });

      const summary = await schedule({
        primary: root,
        taskRoot: staleDir,
        config: strictConfig,
        runtime,
        workers: 3,
        log: () => {},
      });
      rmSync(staleDir, { recursive: true, force: true });

      expect(summary.merged.sort()).toEqual(["T-001", "T-002", "T-003"]);
      const mainRuns = readFileSync(join(root, ".work/.harness/main-runs.txt"), "utf8").trim().split("\n");
      expect(mainRuns.length).toBeGreaterThanOrEqual(2);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("reconcileLandingJournal recovers both pre-merge rollback and post-merge cleanup/accounting", async () => {
    const { root, config } = await createFixtureRepo();
    try {
      const wt = join(root, ".work/T-002-beta");
      await git(root, "worktree", "add", "-b", "task/T-002-beta", wt, "main");
      mkdirSync(join(wt, "pkg/b"), { recursive: true });
      mkdirSync(join(wt, ".harness"), { recursive: true });
      writeFileSync(join(wt, ".harness/review.json"), '{"verdict":"approve"}\n');
      writeFileSync(join(wt, "pkg/b/beta.txt"), "approved-content\n");
      await git(wt, "add", "pkg/b/beta.txt");
      await git(wt, "commit", "-m", "candidate");
      const preSquashHead = await head(wt);

      // Simulate crash BEFORE merge --ff-only with dirty/squashed state in worktree
      writeFileSync(join(wt, "pkg/b/beta.txt"), "corrupted-during-squash\n");
      writeJSON(join(root, ".work/.harness/landing.json"), {
        taskId: "T-002",
        slug: "T-002-beta",
        worktree: wt,
        preSquashHead,
        mergedCommit: null,
      } satisfies LandingJournal);

      await reconcileLandingJournal(root, config);
      expect(existsSync(join(root, ".work/.harness/landing.json"))).toBe(false);
      expect(readFileSync(join(wt, "pkg/b/beta.txt"), "utf8")).toBe("approved-content\n");

      // Now simulate crash AFTER git merge --ff-only succeeded on main, with a complete archive and partially deleted .harness
      await git(root, "merge", "--ff-only", "--no-edit", preSquashHead);
      const archiveDir = join(root, ".work/.harness/archive/T-002-beta");
      mkdirSync(archiveDir, { recursive: true });
      writeFileSync(join(archiveDir, "review.json"), '{"verdict":"approve"}\n');
      writeFileSync(join(archiveDir, "check.log"), "full-check-log\n");
      writeFileSync(join(archiveDir, ".archived"), "ok\n");
      // Delete review.json from source .harness to simulate crash mid-deletion of .harness
      rmSync(join(wt, ".harness/review.json"));
      writeFileSync(join(wt, ".harness/leftover.tmp"), "partial\n");

      writeJSON(join(root, ".work/.harness/landing.json"), {
        taskId: "T-002",
        slug: "T-002-beta",
        worktree: wt,
        preSquashHead,
        mergedCommit: preSquashHead,
      } satisfies LandingJournal);

      // Mark both T-001 and T-002 done on main so queue drains on restart and must run drain mainCheck
      for (const file of ["docs/tasks/T-001-alpha.md", "docs/tasks/T-002-beta.md"]) {
        const p = join(root, file);
        writeFileSync(p, readFileSync(p, "utf8").replace("status: planned", "status: done"));
      }
      await git(root, "add", "-A");
      await git(root, "commit", "-m", "mark done");

      const drainConfig = parseConfig({
        ...config,
        mainCheck: "echo drain-check-ran > .work/.harness/drain-ran.txt",
        mainCheckEvery: 5,
        mainCheckOnDrain: true,
      });
      await schedule({
        primary: root,
        config: drainConfig,
        runtime: {
          async run() {
            throw new Error("unused");
          },
          async close() {},
        },
        log: () => {},
      });

      // Archive preserved complete evidence (not overwritten by partial source .harness)
      expect(existsSync(wt)).toBe(false);
      expect(existsSync(join(archiveDir, "review.json"))).toBe(true);
      expect(existsSync(join(archiveDir, "check.log"))).toBe(true);
      expect(await git(root, "branch", "--list", "task/T-002-beta")).toBe("");
      // Drain mainCheck ran for the recovered merge and reset mergesSinceCheck to 0
      expect(existsSync(join(root, ".work/.harness/drain-ran.txt"))).toBe(true);
      expect(loadMainState(root).mergesSinceCheck).toBe(0);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
