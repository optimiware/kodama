import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";
import type { Config } from "../config.ts";
import { rebaseOnto, reconcileLandingJournal } from "../git.ts";
import { appendEvent } from "../state.ts";
import {
  currentBranch,
  git,
  head,
  isClean,
  Park,
  readJSON,
  runCommand,
  withLock,
  writeJSON,
} from "../system.ts";
import { loadTasks, parseList, validateTasks, writeIndex } from "../tasks.ts";
import {
  nextDecisionId,
  nextTaskId,
  renderTask,
  replaceFrontmatterValue,
  taskSlug,
  upsertDecisionRow,
} from "./format.ts";

const idPattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,60}$/;

export const prepareTaskInput = z.strictObject({
  operation_id: z.string().regex(idPattern),
  title: z.string().min(1).max(120),
  goal: z.string().min(1).max(4000),
  contract: z.string().min(1).max(8000),
  owns: z.array(z.string().min(1)).min(1).max(20),
  check: z.string().min(1).max(500),
  acceptance: z.array(z.string().min(1)).max(30).default([]),
  resources: z.array(z.string().min(1)).max(10).default([]),
  depends_on: z
    .array(z.string().regex(/^T-\d+$/))
    .max(30)
    .default([]),
  milestone: z.string().min(1).max(20).default("M1"),
  block_task: z
    .string()
    .regex(/^T-\d+$/)
    .optional(),
  out_of_scope: z.string().max(2000).optional(),
});

export const prepareDecisionInput = z.strictObject({
  operation_id: z.string().regex(idPattern),
  text: z.string().min(1).max(2000),
  needed_by: z.string().min(1).max(120),
  id: z
    .string()
    .regex(/^O-\d+$/)
    .optional(),
  task_id: z
    .string()
    .regex(/^T-\d+$/)
    .optional(),
  task_status: z.enum(["blocked", "planned"]).optional(),
});

export interface ChangeRecord {
  id: string;
  kind: "task" | "decision";
  targetId?: string;
  status: "prepared" | "published" | "failed";
  worktree: string;
  branch: string;
  commit: string | null;
  summary: string;
  publishedCommit?: string;
  error?: string;
}

const recordsPath = (primary: string) => join(primary, ".work", ".harness", "operator", "changes.json");
const mergeLockPath = (primary: string) => join(primary, ".work", ".harness", "merge.lock");

function loadRecords(primary: string): Record<string, ChangeRecord> {
  return readJSON(recordsPath(primary), {});
}

function saveRecords(primary: string, records: Record<string, ChangeRecord>): void {
  writeJSON(recordsPath(primary), records);
}

function reservedTargets(records: Record<string, ChangeRecord>, kind: ChangeRecord["kind"]): Set<string> {
  const reserved = new Set<string>();
  for (const record of Object.values(records)) {
    if (record.kind === kind && record.status === "prepared" && record.targetId) {
      reserved.add(record.targetId);
    }
  }
  return reserved;
}

function safeSlug(id: string): string {
  return id
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

function taskDir(config: Config): string {
  return config.tasks.split("*")[0]?.replace(/\/[^/]*$/, "") || "docs/tasks";
}

async function discardUnlocked(primary: string, worktree: string, branch: string): Promise<void> {
  if (existsSync(worktree)) await git(primary, "worktree", "remove", "--force", worktree);
  const listed = await git(primary, "branch", "--list", branch);
  if (!listed) return;
  try {
    await git(primary, "branch", "-d", branch);
  } catch {
    await git(primary, "branch", "-D", branch);
  }
}

async function discard(primary: string, worktree: string, branch: string): Promise<void> {
  await withLock(mergeLockPath(primary), () => discardUnlocked(primary, worktree, branch));
}

async function ensureChangeWorktree(primary: string, config: Config, operationId: string) {
  const slug = `operator-${safeSlug(operationId)}`;
  const branch = `task/${slug}`;
  const worktree = join(primary, ".work", slug);
  await withLock(mergeLockPath(primary), async () => {
    const baseCommit = await git(primary, "rev-parse", config.base);
    if (!existsSync(worktree)) {
      const listed = await git(primary, "branch", "--list", branch);
      if (listed) await git(primary, "worktree", "add", worktree, branch);
      else await git(primary, "worktree", "add", "-b", branch, worktree, baseCommit);
    }
  });
  return { worktree, branch };
}

async function commitPaths(worktree: string, paths: string[], message: string): Promise<string> {
  await git(worktree, "add", "-A", "--", ...paths);
  const staged = await git(worktree, "diff", "--cached", "--name-only");
  if (!staged) throw new Error("Change produced no files to commit");
  const unexpected = staged.split("\n").filter((path) => !paths.includes(path));
  if (unexpected.length > 0) throw new Error(`Refusing to commit unexpected files: ${unexpected.join(", ")}`);
  await git(worktree, "commit", "--no-verify", "-m", message);
  return head(worktree);
}

export async function prepareTaskChange(
  primary: string,
  config: Config,
  input: z.infer<typeof prepareTaskInput>,
): Promise<ChangeRecord> {
  const records = loadRecords(primary);
  const existing = records[input.operation_id];
  if (existing?.status === "prepared" || existing?.status === "published") return existing;
  const tasks = loadTasks(primary, config.tasks);
  for (const dep of input.depends_on) {
    if (!tasks.has(dep)) throw new Error(`Unknown dependency ${dep}`);
  }
  if (input.block_task && !tasks.has(input.block_task)) throw new Error(`Unknown task ${input.block_task}`);
  const id = nextTaskId(tasks, reservedTargets(records, "task"));
  const title = input.title.replace(/^T-\d+\s+[—-]\s+/, "");
  const slug = taskSlug(id, title);
  const relativeDir = taskDir(config);
  const relativePath = join(relativeDir, `${slug}.md`);
  const { worktree, branch } = await ensureChangeWorktree(primary, config, input.operation_id);
  const paths = [relativePath];
  try {
    writeFileSync(
      join(worktree, relativePath),
      renderTask(
        {
          id,
          title,
          milestone: input.milestone,
          status: "planned",
          dependsOn: input.depends_on,
          blockers: [],
          owns: input.owns,
          check: input.check,
          resources: input.resources,
          acceptance: input.acceptance,
        },
        { goal: input.goal, contract: input.contract, outOfScope: input.out_of_scope },
      ),
    );
    if (input.block_task) {
      const blocked = tasks.get(input.block_task);
      if (!blocked) throw new Error(`Unknown task ${input.block_task}`);
      const blockedPath = join(relativeDir, `${blocked.slug}.md`);
      const content = readFileSync(join(worktree, blockedPath), "utf8");
      const deps = [...new Set([...blocked.dependsOn, id])];
      writeFileSync(
        join(worktree, blockedPath),
        replaceFrontmatterValue(content, "depends_on", `[${deps.join(", ")}]`),
      );
      paths.push(blockedPath);
    }
    const written = loadTasks(worktree, config.tasks);
    writeIndex(written, join(worktree, config.index), true, config);
    const errors = validateTasks(written, undefined, config);
    if (errors.length > 0) throw new Error(errors.join("\n"));
    const commit = await commitPaths(worktree, paths, `docs: add ${id} (${input.operation_id})`);
    const summary = `Prepared ${id} (${slug}) at ${commit.slice(0, 10)}. Not published.`;
    const record: ChangeRecord = {
      id: input.operation_id,
      kind: "task",
      targetId: id,
      status: "prepared",
      worktree,
      branch,
      commit,
      summary,
    };
    const latest = loadRecords(primary);
    latest[input.operation_id] = record;
    saveRecords(primary, latest);
    return record;
  } catch (error) {
    await discard(primary, worktree, branch);
    throw error;
  }
}

export async function prepareDecisionChange(
  primary: string,
  config: Config,
  input: z.infer<typeof prepareDecisionInput>,
): Promise<ChangeRecord> {
  const decisions = config.operator.decisions;
  if (!decisions) throw new Error("operator.decisions is not configured");
  const records = loadRecords(primary);
  const existing = records[input.operation_id];
  if (existing?.status === "prepared" || existing?.status === "published") return existing;
  const { worktree, branch } = await ensureChangeWorktree(primary, config, input.operation_id);
  const paths = [decisions];
  try {
    const decisionPath = join(worktree, decisions);
    if (!existsSync(decisionPath)) throw new Error(`Missing decisions file: ${decisions}`);
    const current = readFileSync(decisionPath, "utf8");
    const id = input.id ?? nextDecisionId(current, reservedTargets(records, "decision"));
    writeFileSync(
      decisionPath,
      upsertDecisionRow(current, { id, status: input.text, neededBy: input.needed_by }),
    );
    if (input.task_id) {
      const tasks = loadTasks(worktree, config.tasks);
      const task = tasks.get(input.task_id);
      if (!task) throw new Error(`Unknown task ${input.task_id}`);
      const relativePath = join(taskDir(config), `${task.slug}.md`);
      let content = readFileSync(join(worktree, relativePath), "utf8");
      if (input.task_status) {
        content = replaceFrontmatterValue(content, "status", input.task_status);
      }
      if (input.task_status === "blocked") {
        content = replaceFrontmatterValue(content, "decision_blockers", `[${id}]`);
      } else if (input.task_status === "planned") {
        const blockers = parseList(/^decision_blockers:\s*(.+)$/m.exec(content)?.[1] ?? "[]").filter(
          (item) => item !== id,
        );
        content = replaceFrontmatterValue(content, "decision_blockers", `[${blockers.join(", ")}]`);
      }
      writeFileSync(join(worktree, relativePath), content);
      paths.push(relativePath);
      const written = loadTasks(worktree, config.tasks);
      writeIndex(written, join(worktree, config.index), true, config);
      const errors = validateTasks(written, undefined, config);
      if (errors.length > 0) throw new Error(errors.join("\n"));
    }
    const commit = await commitPaths(worktree, paths, `docs: record ${id} (${input.operation_id})`);
    const record: ChangeRecord = {
      id: input.operation_id,
      kind: "decision",
      targetId: id,
      status: "prepared",
      worktree,
      branch,
      commit,
      summary: `Prepared ${id} at ${commit.slice(0, 10)}. Not published.`,
    };
    const latest = loadRecords(primary);
    latest[input.operation_id] = record;
    saveRecords(primary, latest);
    return record;
  } catch (error) {
    await discard(primary, worktree, branch);
    throw error;
  }
}

export async function publishChange(primary: string, config: Config, operationId: string): Promise<string> {
  const home = join(primary, ".work", ".harness");
  return withLock(join(home, "publication.lock"), async () => {
    await reconcileLandingJournal(primary, config);
    const records = loadRecords(primary);
    const record = records[operationId];
    if (!record) throw new Error(`Unknown prepared change ${operationId}`);
    if (record.status === "published")
      return `Already published ${record.publishedCommit?.slice(0, 10) ?? ""}`.trim();
    if (record.status !== "prepared" || !record.commit)
      throw new Error(`Change ${operationId} is not prepared`);
    if ((await currentBranch(primary)) !== config.base || !(await isClean(primary))) {
      throw new Error(`Primary checkout must be clean on ${config.base}`);
    }
    const tip = await git(primary, "rev-parse", config.base);
    try {
      await rebaseOnto(record.worktree, tip);
    } catch (error) {
      if (error instanceof Park) throw new Error(error.message);
      throw error;
    }
    const rebasedCommit = await head(record.worktree);
    let commit = rebasedCommit;
    try {
      const written = loadTasks(record.worktree, config.tasks);
      writeIndex(written, join(record.worktree, config.index), false, config);
      const errors = validateTasks(written, join(record.worktree, config.index), config);
      if (errors.length > 0) throw new Error(errors.join("\n"));
      if (config.operator.docsCheck) {
        const check = await runCommand(config.operator.docsCheck, record.worktree, { timeoutSeconds: 1800 });
        if (check.code !== 0) {
          throw new Error(`Configured check failed (${check.code}): ${check.output.slice(-1500)}`);
        }
      }
      await git(record.worktree, "add", "-A", "--", config.index);
      const staged = await git(record.worktree, "diff", "--cached", "--name-only");
      if (staged) {
        await git(record.worktree, "commit", "--amend", "--no-edit", "--no-verify");
      }
      if (!(await isClean(record.worktree))) {
        throw new Error("Check left uncommitted changes in operator worktree");
      }
      commit = await head(record.worktree);
    } catch (error) {
      await git(record.worktree, "reset", "--hard", rebasedCommit);
      await git(record.worktree, "clean", "-fd");
      record.commit = rebasedCommit;
      records[operationId] = record;
      saveRecords(primary, records);
      throw error;
    }
    await withLock(mergeLockPath(primary), async () => {
      if ((await currentBranch(primary)) !== config.base || !(await isClean(primary))) {
        throw new Error(`Primary checkout must be clean on ${config.base}`);
      }
      await git(primary, "merge", "--ff-only", "--no-edit", commit);
      await discardUnlocked(primary, record.worktree, record.branch);
    });
    record.status = "published";
    record.publishedCommit = commit;
    record.commit = commit;
    records[operationId] = record;
    saveRecords(primary, records);
    await appendEvent(primary, "operator_published", operationId, { commit });
    return `Published ${commit.slice(0, 10)} (${record.summary})`;
  });
}
