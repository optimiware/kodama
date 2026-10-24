import { existsSync, rmSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";
import { buildTaskEnv, type Config, expandTemplate } from "./config.ts";
import {
  changedFiles,
  dirtyFiles,
  head,
  patchId,
  readJSON,
  runCommand,
  type SemaphorePool,
} from "./system.ts";
import type { Task } from "./tasks.ts";

export const EX_TEMPFAIL = 75;

export const SEVERITIES = ["p1", "blocker", "major", "minor", "nit"] as const;
export const BLOCKING_SEVERITIES = new Set(["p1", "blocker", "major"]);

export function isOwnedPath(file: string, owns: readonly string[]): boolean {
  return owns.some((o) => file === o || (o.endsWith("/") && file.startsWith(o)));
}

export function checkScope(
  files: readonly string[],
  task: Pick<Task, "owns">,
  config: Pick<Config, "protected">,
  extraAllowed: readonly string[] = [],
): string[] {
  const allowed = [...task.owns, ...extraAllowed];
  const problems: string[] = [];
  for (const file of files) {
    if (config.protected.includes(file) && !isOwnedPath(file, allowed)) {
      problems.push(`protected path modified without ownership: ${file}`);
      continue;
    }
    if (!isOwnedPath(file, allowed)) {
      problems.push(`unowned path modified: ${file}`);
    }
  }
  return problems;
}

export function normalizeTestName(name: string): string {
  const stripped = name.toLowerCase().replace(/[^a-z0-9]/g, "");
  return stripped.startsWith("test") ? stripped.slice(4) : stripped;
}

export const resultsFileSchema = z.object({
  infra_pause: z.string().optional(),
  passed: z.array(z.string()).default([]),
  failed: z.array(z.string()).default([]),
  skipped: z.array(z.string()).default([]),
});

export function verifyAcceptanceResults(
  declared: readonly string[],
  results: z.infer<typeof resultsFileSchema>,
): string[] {
  if (declared.length === 0) return [];
  const passedRaw = new Set(results.passed);
  const passedNorm = new Set(results.passed.map(normalizeTestName));
  const invalidRaw = new Set([...results.failed, ...results.skipped]);
  const invalidNorm = new Set([...results.failed, ...results.skipped].map(normalizeTestName));

  const missing: string[] = [];
  for (const target of declared) {
    const norm = normalizeTestName(target);
    if (invalidRaw.has(target) || (norm && invalidNorm.has(norm))) {
      missing.push(`${target} (failed or skipped)`);
      continue;
    }
    if (!passedRaw.has(target) && !(norm && passedNorm.has(norm))) {
      missing.push(target);
    }
  }
  return missing;
}

export type CheckOutcome =
  | { status: "passed"; commit: string; patchId: string; logPath: string }
  | { status: "failed"; reason: string; detail: string; logPath: string }
  | { status: "infra_pause"; reason: string; logPath: string }
  | { status: "setup_error"; reason: string; logPath: string }
  | { status: "cancelled"; reason: string };

function tailLines(text: string, maxChars = 4000): string {
  const trimmed = text.trim();
  return trimmed.length <= maxChars ? trimmed : `...\n${trimmed.slice(-maxChars)}`;
}

export async function runTaskCheck(
  worktree: string,
  home: string,
  task: Task,
  config: Config,
  pool: SemaphorePool,
  signal?: AbortSignal,
): Promise<CheckOutcome> {
  const files = await changedFiles(worktree, config.base);
  const scopeErrors = checkScope(files, task, config);
  const hdir = join(worktree, ".harness");
  const logPath = join(hdir, "check.log");
  if (scopeErrors.length > 0) {
    return {
      status: "failed",
      reason: "scope check failed",
      detail: scopeErrors.join("\n"),
      logPath,
    };
  }

  const rawCmd = task.check ?? config.check;
  const cmd = expandTemplate(rawCmd, task);
  const resultsPath = join(hdir, "results.json");
  if (existsSync(resultsPath)) rmSync(resultsPath, { force: true });

  const env = buildTaskEnv(config, home, task);
  env.FORGE_RESULTS = resultsPath;

  const run = await pool.withResources(
    task.resources,
    () =>
      runCommand(cmd, worktree, {
        env,
        timeoutSeconds: config.limits.checkTimeoutSeconds,
        signal,
        logPath,
      }),
    signal,
  );

  if (run.cancelled) {
    return { status: "cancelled", reason: "Queue halted" };
  }
  if (run.timedOut || run.code === 124) {
    return {
      status: "infra_pause",
      reason: `check timed out after ${config.limits.checkTimeoutSeconds}s: ${cmd}`,
      logPath,
    };
  }
  if (existsSync(resultsPath)) {
    try {
      const pre = resultsFileSchema.parse(readJSON<unknown>(resultsPath));
      if (pre.infra_pause) {
        return { status: "infra_pause", reason: pre.infra_pause, logPath };
      }
    } catch {
      // Handled below if exit code is 0.
    }
  }
  if (run.code === EX_TEMPFAIL) {
    return {
      status: "infra_pause",
      reason: tailLines(run.output) || `environment unavailable (exit 75): ${cmd}`,
      logPath,
    };
  }
  if (run.code !== 0) {
    return {
      status: "failed",
      reason: `check failed (exit ${run.code}): ${cmd}`,
      detail: tailLines(run.output),
      logPath,
    };
  }

  if (task.acceptance.length > 0) {
    if (!existsSync(resultsPath)) {
      return {
        status: "setup_error",
        reason: `check command exited 0 but did not write FORGE_RESULTS (${resultsPath})`,
        logPath,
      };
    }
    let parsed: z.infer<typeof resultsFileSchema>;
    try {
      parsed = resultsFileSchema.parse(readJSON<unknown>(resultsPath));
    } catch (err) {
      return {
        status: "setup_error",
        reason: `invalid FORGE_RESULTS JSON: ${err instanceof Error ? err.message : String(err)}`,
        logPath,
      };
    }
    const missing = verifyAcceptanceResults(task.acceptance, parsed);
    if (missing.length > 0) {
      return {
        status: "failed",
        reason: `acceptance tests not satisfied: ${missing.join(", ")}`,
        detail: tailLines(run.output),
        logPath,
      };
    }
  }

  const dirty = await dirtyFiles(worktree);
  if (dirty.length > 0) {
    return {
      status: "failed",
      reason: `worktree has uncommitted or check-generated files: ${dirty.join(", ")}`,
      detail: dirty.join("\n"),
      logPath,
    };
  }

  const commit = await head(worktree);
  const pid = await patchId(worktree, config.base);
  return { status: "passed", commit, patchId: pid, logPath };
}

export const planSchema = z.strictObject({
  markdown: z.string().refine((s) => s.trim().length > 0, "plan markdown must not be empty"),
  blockers: z.array(z.string().min(1)).default([]),
});
export type PlanResult = z.infer<typeof planSchema>;

export const handoffSchema = z.strictObject({
  summary: z.string().refine((s) => s.trim().length > 0, "handoff summary must not be empty"),
  tests_run: z.array(z.string().min(1)).default([]),
  blockers: z.array(z.string().min(1)).default([]),
});
export type HandoffResult = z.infer<typeof handoffSchema>;

export const findingSchema = z.strictObject({
  severity: z.enum(SEVERITIES),
  file: z.string().min(1),
  line: z.number().int().nonnegative().optional(),
  issue: z.string().min(1),
  suggestion: z.string().optional(),
});
export type Finding = z.infer<typeof findingSchema>;

export const reviewSchema = z
  .strictObject({
    verdict: z.enum(["approve", "changes_requested"]),
    summary: z.string().refine((s) => s.trim().length > 0, "review summary must not be empty"),
    findings: z.array(findingSchema).default([]),
  })
  .superRefine((rev, ctx) => {
    const blocking = rev.findings.filter((f) => BLOCKING_SEVERITIES.has(f.severity));
    if (rev.verdict === "approve" && blocking.length > 0) {
      ctx.addIssue({
        code: "custom",
        path: ["verdict"],
        message: "approve verdict cannot contain blocking findings (p1, blocker, major)",
      });
    }
    if (rev.verdict === "changes_requested" && blocking.length === 0) {
      ctx.addIssue({
        code: "custom",
        path: ["findings"],
        message: "changes_requested verdict requires at least one blocking finding (p1, blocker, major)",
      });
    }
  });
export type ReviewResult = z.infer<typeof reviewSchema>;

export function formatBlockingFindings(review: ReviewResult): string {
  return review.findings
    .filter((f) => BLOCKING_SEVERITIES.has(f.severity))
    .map(
      (f) =>
        `- [${f.severity}] ${f.file}${f.line ? `:${f.line}` : ""}: ${f.issue}${f.suggestion ? ` (${f.suggestion})` : ""}`,
    )
    .join("\n");
}
