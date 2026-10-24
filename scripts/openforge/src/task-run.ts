import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { buildTaskEnv, type Config, inheritedEnv, type Role } from "./config.ts";
import {
  formatBlockingFindings,
  type HandoffResult,
  type PlanResult,
  type ReviewResult,
  runTaskCheck,
} from "./gate.ts";
import {
  commitOwnedChanges,
  ensureWorktree,
  LandingGated,
  landTask,
  RebaseChangedPatch,
  rebaseOnto,
  reconcileLandingJournal,
} from "./git.ts";
import { fixerPrompt, implementerPrompt, plannerPrompt, reviewerPrompt } from "./prompts.ts";
import { AgentError, type AgentRuntime } from "./runtime/sdk.ts";
import { appendEvent, loadGenerations, loadMainState, parkTask, StateStore, saveMainState } from "./state.ts";
import {
  canonicalJSON,
  currentBranch,
  git,
  hash,
  head,
  isClean,
  Park,
  Paused,
  readJSON,
  runCommand,
  type SemaphorePool,
  withLock,
  writeJSON,
} from "./system.ts";
import { parseTask, type Task } from "./tasks.ts";

export type StopPoint = "plan" | "implement" | "check" | "review" | "merge";

const STOP_RANK: Record<StopPoint, number> = {
  plan: 1,
  implement: 2,
  check: 3,
  review: 4,
  merge: 5,
};

const PHASE_RANK: Record<string, number> = {
  claimed: 0,
  planned: 1,
  implemented: 2,
  verified: 3,
  approved: 4,
  merged: 5,
};

export class TaskRunner {
  readonly wt: string;
  readonly hdir: string;
  readonly store: StateStore;
  readonly home: string;
  private initialized = false;

  constructor(
    readonly primary: string,
    public task: Task,
    readonly config: Config,
    readonly runtime: AgentRuntime,
    readonly pool: SemaphorePool,
    readonly stopAfter: StopPoint = "merge",
    readonly signal?: AbortSignal,
    readonly log: (taskId: string, msg: string) => void = (id, msg) =>
      console.log(`[${new Date().toISOString()}] ${id}: ${msg}`),
  ) {
    this.wt = join(primary, ".work", task.slug);
    this.hdir = join(this.wt, ".harness");
    this.store = new StateStore(this.wt);
    this.home = join(primary, ".work", ".harness");
  }

  get state() {
    return this.store.load();
  }

  private qualificationContext(): string {
    const task = parseTask(this.task.path);
    const { path: _path, ...contract } = task;
    const contextPaths = [...this.config.context, ".opencode/agents/reviewer.md"];
    const contents = contextPaths.map((path) => {
      return [
        path,
        ...[this.primary, this.wt].map((root) => {
          const full = join(root, path);
          return existsSync(full) ? readFileSync(full, "utf8") : null;
        }),
      ];
    });
    return hash(canonicalJSON({ task: contract, config: this.config, contents }));
  }

  isStopped(): boolean {
    const phase = this.store.exists() ? this.state.phase : "claimed";
    if (phase === "merged") return true;
    const rank = PHASE_RANK[phase] ?? 0;
    if (this.stopAfter === "plan" && !this.task.plan && rank >= 0 && this.initialized) {
      return true;
    }
    return rank >= STOP_RANK[this.stopAfter];
  }

  async init(): Promise<void> {
    if (this.initialized) return;
    const branch = `task/${this.task.slug}`;
    const { created, baseCommit } = await ensureWorktree(this.primary, this.wt, branch, this.config.base);
    mkdirSync(this.hdir, { recursive: true });
    if (created || !this.store.exists()) {
      this.store.save({ phase: "claimed", baseCommit });
    }
    const gen = loadGenerations(this.primary)[this.task.id] ?? 0;
    if (gen > this.state.requeueGeneration) {
      this.store.save({
        repairs: 0,
        reviewRounds: 0,
        infraRetries: 0,
        repairReason: null,
        repairDetail: null,
        openFindings: null,
        requeueGeneration: gen,
      });
    }
    await this.runSetup();
    this.initialized = true;
  }

  private async runSetup(): Promise<void> {
    if (!this.config.setup) return;
    const receiptPath = join(this.hdir, "setup.json");
    const cmdHash = hash(this.config.setup);
    if (readJSON<{ cmdHash?: string }>(receiptPath, {}).cmdHash === cmdHash) return;
    const res = await runCommand(this.config.setup, this.wt, {
      env: buildTaskEnv(this.config, this.home, this.task),
      timeoutSeconds: 600,
      signal: this.signal,
      logPath: join(this.hdir, "setup.log"),
    });
    if (res.cancelled) throw new Paused("Queue halted");
    if (res.code !== 0) {
      throw new Park(`setup command failed (${res.code}): ${res.output.slice(-1000)}`);
    }
    writeJSON(receiptPath, { cmdHash });
  }

  private async ask<T>(
    role: Role,
    prompt: string,
    sessionID: string | null,
    onSession: (id: string) => void,
  ): Promise<T> {
    const attempts = this.config.limits.agentAttempts;
    let lastErr: unknown;
    for (let attempt = 1; attempt <= attempts; attempt++) {
      this.store.save({ activity: `running ${role} (attempt ${attempt})` });
      const started = Date.now();
      try {
        const out = await this.runtime.run({
          task: this.task.id,
          role,
          prompt,
          worktree: this.wt,
          owns: this.task.owns,
          protectedPaths: this.config.protected.filter((p) => this.task.owns.includes(p)),
          sessionID,
          environment: { ...inheritedEnv(this.config), ...buildTaskEnv(this.config, this.home, this.task) },
          timeoutSeconds: this.config.limits.agentTimeoutSeconds,
          signal: this.signal,
          onSession,
        });
        if ((await currentBranch(this.wt)) !== `task/${this.task.slug}`) {
          throw new Park("Agent switched away from assigned task branch");
        }
        await appendEvent(this.primary, "agent_finished", this.task.id, {
          role,
          sessionID: out.sessionID,
          durationSeconds: (Date.now() - started) / 1000,
        });
        return out.result as T;
      } catch (err) {
        if (err instanceof Paused || err instanceof Park) throw err;
        if (!(err instanceof AgentError) || !err.retryable) {
          throw new Park(err instanceof Error ? err.message : String(err));
        }
        lastErr = err;
        await appendEvent(this.primary, "agent_retry", this.task.id, { role, attempt });
      } finally {
        this.store.save({ activity: "idle" });
      }
    }
    throw new Park(`${role} exhausted ${attempts} attempts: ${String(lastErr)}`);
  }

  async step(): Promise<"continue" | "done" | "parked" | "paused" | "yield"> {
    try {
      await this.init();
      this.task = parseTask(this.task.path);
      let context = this.qualificationContext();
      if (
        (this.state.phase === "verified" || this.state.phase === "approved") &&
        this.state.qualificationContext !== context
      ) {
        const baseCommit = await git(this.primary, "rev-parse", this.config.base);
        await rebaseOnto(this.wt, baseCommit);
        context = this.qualificationContext();
        this.store.save({
          baseCommit,
          phase: "implemented",
          verifiedPatchId: null,
          approvedPatchId: null,
          reviewerSessionID: null,
          openFindings: null,
          repairReason: null,
          repairDetail: null,
        });
      }
      if (this.isStopped()) return "done";

      const st = this.state;
      if (st.phase === "claimed") {
        if (this.task.plan) {
          this.log(this.task.id, "planning");
          const plan = await this.ask<PlanResult>(
            "planner",
            plannerPrompt(this.task, this.config),
            null,
            () => {},
          );
          writeFileSync(join(this.hdir, "plan.md"), `${plan.markdown.trim()}\n`);
          if (plan.blockers.length > 0) {
            throw new Park(`Planner blockers: ${plan.blockers.join("; ")}`);
          }
          this.store.save({ phase: "planned" });
          return this.isStopped() ? "done" : "continue";
        }
        this.store.save({ phase: "planned" });
        if (this.stopAfter === "plan") return "done";
      }

      if (this.state.phase === "planned") {
        this.log(this.task.id, "implementing");
        const planPath = join(this.hdir, "plan.md");
        const planMd = existsSync(planPath) ? readFileSync(planPath, "utf8") : undefined;
        const handoff = await this.ask<HandoffResult>(
          "implementer",
          implementerPrompt(this.task, this.config, planMd),
          this.state.writerSessionID,
          (id) => this.store.save({ writerSessionID: id }),
        );
        await commitOwnedChanges(
          this.wt,
          this.config.base,
          this.task,
          this.config,
          `feat(${this.task.id}): implement task`,
        );
        writeFileSync(join(this.hdir, "handoff.md"), `${handoff.summary.trim()}\n`);
        writeJSON(join(this.hdir, "handoff.json"), handoff);
        if (handoff.blockers.length > 0) {
          throw new Park(`Implementer blockers: ${handoff.blockers.join("; ")}`);
        }
        this.store.save({
          phase: "implemented",
          candidateCommit: await head(this.wt),
        });
        return this.isStopped() ? "done" : "continue";
      }

      if (this.state.phase === "implemented") {
        if (this.state.repairReason) {
          this.log(this.task.id, `fixing (${this.state.repairReason})`);
          const handoff = await this.ask<HandoffResult>(
            "fixer",
            fixerPrompt(this.task, this.config, this.state.repairReason, this.state.repairDetail ?? ""),
            this.state.writerSessionID,
            (id) => this.store.save({ writerSessionID: id }),
          );
          await commitOwnedChanges(
            this.wt,
            this.config.base,
            this.task,
            this.config,
            `fix(${this.task.id}): address check or review findings`,
          );
          writeFileSync(join(this.hdir, "handoff.md"), `${handoff.summary.trim()}\n`);
          writeJSON(join(this.hdir, "handoff.json"), handoff);
          if (handoff.blockers.length > 0) {
            throw new Park(`Fixer blockers: ${handoff.blockers.join("; ")}`);
          }
          this.store.save({
            repairReason: null,
            repairDetail: null,
            candidateCommit: await head(this.wt),
          });
        }

        this.log(this.task.id, "running check");
        this.store.save({ activity: "running check" });
        const outcome = await runTaskCheck(
          this.wt,
          this.home,
          this.task,
          this.config,
          this.pool,
          this.signal,
        );
        this.store.save({ activity: "idle" });

        if (outcome.status === "cancelled") throw new Paused(outcome.reason);
        if (outcome.status === "setup_error") throw new Park(outcome.reason);
        if (outcome.status === "infra_pause") {
          const retries = this.state.infraRetries + 1;
          this.store.save({ infraRetries: retries });
          this.log(this.task.id, `infrastructure unavailable: ${outcome.reason}`);
          await appendEvent(this.primary, "paused", this.task.id, { reason: outcome.reason });
          throw new Paused(outcome.reason);
        }
        if (outcome.status === "failed") {
          const repairs = this.state.repairs + 1;
          if (repairs > this.config.limits.repairs) {
            throw new Park(`Checks failing after ${this.config.limits.repairs} repairs: ${outcome.reason}`);
          }
          this.store.save({
            repairs,
            repairReason: outcome.reason,
            repairDetail: outcome.detail,
          });
          return "continue";
        }

        this.store.save({
          infraRetries: 0,
          candidateCommit: outcome.commit,
          patchId: outcome.patchId,
          verifiedPatchId: outcome.patchId,
          qualificationContext: context,
          phase: "verified",
        });
        return this.isStopped() ? "done" : "continue";
      }

      if (this.state.phase === "verified") {
        if (this.task.review === "human") {
          throw new Park("Verified and awaiting human review (`review: human`)");
        }
        this.log(this.task.id, "reviewing");
        const handoffPath = join(this.hdir, "handoff.md");
        const handoffText = existsSync(handoffPath) ? readFileSync(handoffPath, "utf8") : "";
        const stat = await git(this.wt, "diff", "--stat", `${this.config.base}...HEAD`);
        const diff = (await git(this.wt, "diff", `${this.config.base}...HEAD`)).slice(0, 60_000);
        const review = await this.ask<ReviewResult>(
          "reviewer",
          reviewerPrompt(this.task, handoffText, stat, diff, this.state.openFindings),
          this.state.reviewerSessionID,
          (id) => this.store.save({ reviewerSessionID: id }),
        );
        writeJSON(join(this.hdir, "review.json"), {
          ...review,
          candidateCommit: this.state.candidateCommit,
          patchId: this.state.verifiedPatchId,
          qualificationContext: this.state.qualificationContext,
        });

        if (review.verdict === "approve") {
          this.store.save({
            approvedPatchId: this.state.verifiedPatchId,
            openFindings: null,
            phase: "approved",
          });
          return this.isStopped() ? "done" : "continue";
        }

        const rounds = this.state.reviewRounds + 1;
        const findingsText = formatBlockingFindings(review);
        if (rounds >= this.config.limits.reviewRounds) {
          throw new Park(`Review not approved after ${rounds} rounds:\n${findingsText}`);
        }
        this.store.save({
          reviewRounds: rounds,
          openFindings: findingsText,
          repairReason: "Reviewer requested changes",
          repairDetail: findingsText,
          verifiedPatchId: null,
          phase: "implemented",
        });
        return "continue";
      }

      if (this.state.phase === "approved") {
        this.log(this.task.id, `landing onto ${this.config.base}`);
        const verifiedBase = this.state.baseCommit;
        const approvedPatch = this.state.approvedPatchId;
        if (!approvedPatch) {
          this.store.save({ phase: "implemented" });
          return "continue";
        }
        try {
          const mergedCommit = await landTask({
            primary: this.primary,
            worktree: this.wt,
            task: this.task,
            config: this.config,
            approvedPatchId: approvedPatch,
            validateApproval: () => {
              if (this.qualificationContext() !== this.state.qualificationContext) {
                throw new RebaseChangedPatch("review-context-changed");
              }
            },
            verifiedBaseCommit: verifiedBase,
            reverifyAfterRebase: async () => {
              const check = await runTaskCheck(
                this.wt,
                this.home,
                this.task,
                this.config,
                this.pool,
                this.signal,
              );
              if (check.status !== "passed") {
                throw new RebaseChangedPatch("check-failed-after-rebase");
              }
            },
            signal: this.signal,
          });
          await appendEvent(this.primary, "merged", this.task.id, { commit: mergedCommit });
          this.log(this.task.id, `merged (${mergedCommit.slice(0, 10)})`);
          return "done";
        } catch (err) {
          if (err instanceof LandingGated) {
            return "yield";
          }
          if (err instanceof RebaseChangedPatch) {
            this.log(this.task.id, "rebase changed candidate or required re-check; re-qualifying");
            this.store.save({
              baseCommit: await git(this.primary, "rev-parse", this.config.base),
              verifiedPatchId: null,
              approvedPatchId: null,
              phase: "implemented",
            });
            return "continue";
          }
          throw err;
        }
      }

      return "done";
    } catch (err) {
      if (err instanceof Paused) {
        this.log(this.task.id, `PAUSED: ${err.message}`);
        return "paused";
      }
      if (err instanceof Park) {
        this.log(this.task.id, `PARKED: ${err.message}`);
        await parkTask(this.primary, this.task.id, err.message);
        return "parked";
      }
      throw err;
    }
  }
}

export async function runMainCheck(
  primary: string,
  config: Config,
  signal?: AbortSignal,
): Promise<{ ok: boolean; commit: string; message: string; logPath: string }> {
  const home = join(primary, ".work", ".harness");
  mkdirSync(home, { recursive: true });
  return withLock(
    join(home, "publication.lock"),
    async () => {
      await reconcileLandingJournal(primary, config);
      const commit = await git(primary, "rev-parse", config.base);
      const logPath = join(home, "main-check.log");
      const commands = Array.isArray(config.mainCheck) ? config.mainCheck : [config.mainCheck];
      const env = buildTaskEnv(config, home);
      const record = (ok: boolean, message: string) => {
        const prev = loadMainState(primary);
        saveMainState(primary, {
          ...prev,
          ...(ok ? { lastGreenCommit: commit, mergesSinceCheck: 0 } : {}),
          blocked: !ok,
          lastResult: { ok, commit, finishedAt: new Date().toISOString(), message, logPath },
        });
        return { ok, commit, message, logPath };
      };
      const matchesCommit = async () =>
        (await currentBranch(primary)) === config.base &&
        (await head(primary)) === commit &&
        (await git(primary, "rev-parse", config.base)) === commit &&
        (await isClean(primary));

      for (const cmd of commands) {
        if (!(await matchesCommit())) {
          return record(false, `mainCheck requires a clean ${config.base} checkout at ${commit}`);
        }
        const res = await runCommand(cmd, primary, {
          env,
          timeoutSeconds: config.limits.checkTimeoutSeconds,
          signal,
          logPath,
        });
        if (res.code !== 0) {
          return record(false, `mainCheck failed (${res.code}) on ${cmd}`);
        }
        if (!(await matchesCommit())) {
          return record(false, `mainCheck checkout changed while checking ${commit}`);
        }
      }

      return record(true, `PASS mainCheck at ${commit}`);
    },
    { signal, haltFile: join(home, "HALT") },
  );
}
