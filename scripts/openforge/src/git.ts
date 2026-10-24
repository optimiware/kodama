import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import type { Config } from "./config.ts";
import { checkScope } from "./gate.ts";
import { loadMainState, saveMainState } from "./state.ts";
import {
  changedFiles,
  currentBranch,
  dirtyFiles,
  git,
  gitRaw,
  head,
  isClean,
  Park,
  patchId,
  readJSON,
  withLock,
  writeJSON,
} from "./system.ts";
import { loadTasks, type Task, writeIndex } from "./tasks.ts";

export class RebaseChangedPatch extends Error {
  constructor(readonly newPatchId: string) {
    super("Rebase changed candidate patch-id; re-verification required");
    this.name = "RebaseChangedPatch";
  }
}

export class LandingGated extends Error {
  constructor(message = "Periodic mainCheck is due before further landings") {
    super(message);
    this.name = "LandingGated";
  }
}

export interface LandingJournal {
  taskId: string;
  slug: string;
  worktree: string;
  preSquashHead: string;
  mergedCommit: string | null;
}

const landingJournalPath = (primary: string) => join(primary, ".work", ".harness", "landing.json");

function isCommitOnBase(primary: string, commit: string, base: string): boolean {
  const proc = Bun.spawnSync(["git", "merge-base", "--is-ancestor", commit, base], {
    cwd: primary,
    stdout: "pipe",
    stderr: "pipe",
  });
  return proc.exitCode === 0;
}

export async function finalizeMergedWorktree(primary: string, worktree: string, slug: string): Promise<void> {
  const home = join(primary, ".work", ".harness");
  const archiveDir = join(home, "archive", slug);
  const completeMarker = join(archiveDir, ".archived");
  const hdir = join(worktree, ".harness");
  if (existsSync(hdir)) {
    if (!existsSync(completeMarker)) {
      rmSync(archiveDir, { recursive: true, force: true });
      mkdirSync(archiveDir, { recursive: true });
      cpSync(hdir, archiveDir, { recursive: true });
      writeFileSync(completeMarker, "ok\n");
    }
    rmSync(hdir, { recursive: true, force: true });
  }
  if (existsSync(worktree)) {
    await git(primary, "worktree", "remove", "--force", worktree);
  }
  const branchRef = await git(primary, "branch", "--list", `task/${slug}`);
  if (branchRef) {
    await git(primary, "branch", "-d", `task/${slug}`);
  }
}

function recordMergedInMainState(primary: string, mergedCommit: string): void {
  const curMain = loadMainState(primary);
  if (curMain.lastCountedCommit === mergedCommit) return;
  saveMainState(primary, {
    ...curMain,
    lastCountedCommit: mergedCommit,
    mergesSinceCheck: curMain.mergesSinceCheck + 1,
  });
}

export async function reconcileLandingJournal(primary: string, config: Config): Promise<void> {
  const jpath = landingJournalPath(primary);
  if (!existsSync(jpath)) return;
  const journal = readJSON<LandingJournal>(jpath);
  if (journal.mergedCommit && isCommitOnBase(primary, journal.mergedCommit, config.base)) {
    recordMergedInMainState(primary, journal.mergedCommit);
    await finalizeMergedWorktree(primary, journal.worktree, journal.slug);
    rmSync(jpath, { force: true });
    return;
  }
  if (existsSync(journal.worktree)) {
    await git(journal.worktree, "reset", "--hard", journal.preSquashHead);
    await git(journal.worktree, "clean", "-fd", "-e", ".harness");
  }
  rmSync(jpath, { force: true });
}

export async function ensureWorktree(
  primary: string,
  worktree: string,
  branch: string,
  baseBranch: string,
): Promise<{ created: boolean; baseCommit: string }> {
  const home = join(primary, ".work", ".harness");
  return withLock(join(home, "merge.lock"), async () => {
    const baseCommit = await git(primary, "rev-parse", baseBranch);
    if (!existsSync(worktree)) {
      await git(primary, "worktree", "add", "-b", branch, worktree, baseCommit);
      return { created: true, baseCommit };
    }
    const actual = await currentBranch(worktree);
    if (actual !== branch) {
      throw new Park(`Worktree ${worktree} is on branch ${actual}, expected ${branch}`);
    }
    return { created: false, baseCommit };
  });
}

export async function dropHarnessFromIndex(worktree: string): Promise<void> {
  const tracked = await git(worktree, "ls-files", "--", ".harness");
  if (tracked) {
    await git(worktree, "rm", "-r", "--cached", "--quiet", "--", ".harness");
  }
}

export async function commitOwnedChanges(
  worktree: string,
  base: string,
  task: Task,
  config: Config,
  message: string,
): Promise<boolean> {
  await dropHarnessFromIndex(worktree);
  if (await isClean(worktree)) return false;
  const files = await changedFiles(worktree, base);
  const scopeErrors = checkScope(files, task, config);
  if (scopeErrors.length > 0) {
    throw new Park(`Cannot commit unowned changes: ${scopeErrors.join("; ")}`);
  }
  await git(worktree, "add", "-A", "--", ...task.owns);
  await dropHarnessFromIndex(worktree);
  const staged = await git(worktree, "diff", "--cached", "--name-only");
  if (!staged) return false;
  await git(worktree, "commit", "--no-verify", "-m", message);
  return true;
}

export async function rebaseOnto(worktree: string, target: string): Promise<void> {
  await dropHarnessFromIndex(worktree);
  if (!(await isClean(worktree))) {
    throw new Park("Worktree has uncommitted changes before rebase");
  }
  const mergeBase = await git(worktree, "merge-base", target, "HEAD");
  const targetSha = await git(worktree, "rev-parse", target);
  if (mergeBase === targetSha) return;
  try {
    await git(worktree, "rebase", targetSha);
  } catch (err) {
    try {
      await git(worktree, "rebase", "--abort");
    } catch {
      // Ignore abort error.
    }
    throw new Park(`Rebase conflict onto ${target}: ${err instanceof Error ? err.message : String(err)}`);
  }
}

function markTaskDoneInFile(taskPath: string): void {
  const content = readFileSync(taskPath, "utf8");
  const updated = content.replace(/^(---\n[\s\S]*?\nstatus:\s*)(\S+)(\n[\s\S]*?\n---\n)/, "$1done$3");
  writeFileSync(taskPath, updated);
}

function commitSubject(task: Task): string {
  const prefix = `${task.id} — `;
  const short = task.title.startsWith(prefix) ? task.title.slice(prefix.length).trim() : task.title.trim();
  const lowered = short ? `${short[0]?.toLowerCase() ?? ""}${short.slice(1)}` : task.id;
  return `feat: ${lowered} (${task.id})`;
}

export async function landTask(options: {
  primary: string;
  worktree: string;
  task: Task;
  config: Config;
  approvedPatchId: string;
  verifiedBaseCommit: string;
  reverifyAfterRebase: () => Promise<void>;
  validateApproval?: () => void;
  signal?: AbortSignal;
}): Promise<string> {
  const {
    primary,
    worktree,
    task,
    config,
    approvedPatchId,
    verifiedBaseCommit,
    reverifyAfterRebase,
    signal,
  } = options;
  const home = join(primary, ".work", ".harness");
  const haltFile = join(home, "HALT");

  return withLock(
    join(home, "publication.lock"),
    async () => {
      await reconcileLandingJournal(primary, config);
      options.validateApproval?.();

      const mainState = loadMainState(primary);
      if (mainState.blocked) {
        throw new LandingGated("Main branch check is failing; run `batch --retry` before landing");
      }
      if (config.mainCheckEvery > 0 && mainState.mergesSinceCheck >= config.mainCheckEvery) {
        throw new LandingGated("Periodic mainCheck is due before further landings");
      }
      if ((await currentBranch(primary)) !== config.base || !(await isClean(primary))) {
        throw new Park(`Primary checkout must be clean on ${config.base}`);
      }

      const targetTip = await git(primary, "rev-parse", config.base);
      await rebaseOnto(worktree, targetTip);

      const postRebasePatch = await patchId(worktree, targetTip);
      if (postRebasePatch !== approvedPatchId) {
        throw new RebaseChangedPatch(postRebasePatch);
      }

      if (targetTip !== verifiedBaseCommit) {
        await reverifyAfterRebase();
        const dirtyAfterCheck = await dirtyFiles(worktree);
        if (dirtyAfterCheck.length > 0) {
          await git(worktree, "reset", "--hard", "HEAD");
          await git(worktree, "clean", "-fd", "-e", ".harness");
          throw new Park(`Post-rebase check mutated worktree files: ${dirtyAfterCheck.join(", ")}`);
        }
        if ((await patchId(worktree, targetTip)) !== approvedPatchId) {
          throw new RebaseChangedPatch("post-check-patch-drift");
        }
      }

      const taskRelPath = join(
        config.tasks.split("*")[0]?.replace(/\/[^/]*$/, "") || "docs/tasks",
        basename(task.path),
      );
      const bookkeeping = [config.index, taskRelPath];
      const files = await changedFiles(worktree, targetTip);
      const scopeErrors = checkScope(files, task, config, bookkeeping);
      if (scopeErrors.length > 0) {
        throw new Park(`Scope violation at landing: ${scopeErrors.join("; ")}`);
      }

      const preSquashHead = await head(worktree);
      const jpath = landingJournalPath(primary);
      writeJSON(jpath, {
        taskId: task.id,
        slug: task.slug,
        worktree,
        preSquashHead,
        mergedCommit: null,
      } satisfies LandingJournal);

      let mergedCommit: string;
      try {
        const wtTaskPath = join(worktree, taskRelPath);
        markTaskDoneInFile(wtTaskPath);
        const wtTasks = loadTasks(worktree, config.tasks);
        writeIndex(wtTasks, join(worktree, config.index), false, config);

        await dropHarnessFromIndex(worktree);
        await git(worktree, "reset", "--soft", targetTip);
        await git(worktree, "add", "-A", "--", ...task.owns, ...bookkeeping);
        await dropHarnessFromIndex(worktree);
        await git(worktree, "commit", "--no-verify", "-m", commitSubject(task));

        mergedCommit = await head(worktree);
        const squashDiffRaw = await gitRaw(
          worktree,
          "diff",
          "--no-renames",
          "--name-only",
          "-z",
          preSquashHead,
          mergedCommit,
        );
        const squashDiff = squashDiffRaw.split("\0").filter(Boolean);
        const unexpected = squashDiff.filter((p) => !bookkeeping.includes(p));
        if (unexpected.length > 0 || !(await isClean(worktree))) {
          throw new Park(
            `Squash commit altered files outside bookkeeping: ${unexpected.join(", ") || "dirty tree"}`,
          );
        }

        writeJSON(jpath, {
          taskId: task.id,
          slug: task.slug,
          worktree,
          preSquashHead,
          mergedCommit,
        } satisfies LandingJournal);

        await withLock(join(home, "merge.lock"), async () => {
          options.validateApproval?.();
          await git(primary, "merge", "--ff-only", "--no-edit", mergedCommit);
          recordMergedInMainState(primary, mergedCommit);
        });
      } catch (err) {
        await reconcileLandingJournal(primary, config);
        throw err;
      }

      await finalizeMergedWorktree(primary, worktree, task.slug);
      rmSync(jpath, { force: true });
      return mergedCommit;
    },
    { signal, haltFile },
  );
}
