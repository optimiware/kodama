import { readFileSync } from "node:fs";
import { basename } from "node:path";
import { type Config, expandTemplate } from "./config.ts";
import type { Task } from "./tasks.ts";

export const REPORT_TOOLS = {
  planner: "openforge_submit_plan",
  implementer: "openforge_submit_handoff",
  fixer: "openforge_submit_handoff",
  reviewer: "openforge_submit_review",
} as const;

export function plannerPrompt(task: Task, config: Config): string {
  const ctx = config.context.join(", ");
  return [
    `Plan task ${task.id} (${task.title}).`,
    `Read ${ctx} and the task file ${basename(task.path)} using read-only tools.`,
    `Owned paths: ${task.owns.join(", ")}.`,
    task.acceptance.length > 0 ? `Required acceptance tests: ${task.acceptance.join(", ")}.` : "",
    `Submit your plan by calling \`${REPORT_TOOLS.planner}\` once with \`{ markdown, blockers }\`. Set \`blockers\` only if a specification conflict or unowned dependency prevents implementation.`,
    `# Task file\n${readFileSync(task.path, "utf8")}`,
  ]
    .filter(Boolean)
    .join("\n\n");
}

export function implementerPrompt(task: Task, config: Config, planMarkdown?: string): string {
  const ctx = config.context.join(", ");
  const checkCmd = expandTemplate(task.check ?? config.check, task);
  return [
    `Implement task ${task.id} (${task.title}) in this worktree.`,
    `1. Read ${ctx} and the task file ${basename(task.path)}${planMarkdown ? ", plus the plan below" : ""}.`,
    `2. Edit only owned paths: ${task.owns.join(", ")}. Protected paths (${config.protected.join(", ") || "none"}) may only be modified when listed in owned paths.`,
    task.acceptance.length > 0
      ? `3. Implement and pass the required acceptance tests: ${task.acceptance.join(", ")}.`
      : `3. Implement the task deliverables and tests.`,
    `4. Run the task check command while working: \`${checkCmd}\`. Commit your changes on the current task branch. Never push, rebase, merge, or touch another worktree. The orchestrator provisions and removes the task's development environment scope: never run \`make dev-up\`, \`make dev-down\` or any teardown for it, even when you finish.`,
    `5. Finish by calling \`${REPORT_TOOLS.implementer}\` once with \`{ summary, tests_run, blockers }\`. Use \`blockers\` only for a decision or scope problem that stops you from completing the task (it parks the task for a human). Put caveats, environment limits and deferred follow-ups in \`summary\` and leave \`blockers\` empty.`,
    planMarkdown ? `# Plan\n${planMarkdown}` : "",
    `# Task file\n${readFileSync(task.path, "utf8")}`,
  ]
    .filter(Boolean)
    .join("\n\n");
}

export function fixerPrompt(task: Task, config: Config, reason: string, detail: string): string {
  const checkCmd = expandTemplate(task.check ?? config.check, task);
  return [
    `Repair task ${task.id} inside owned paths (${task.owns.join(", ")}).`,
    `Problem: ${reason}`,
    detail,
    `Fix the issue, verify with \`${checkCmd}\`, commit your changes on the current branch, and call \`${REPORT_TOOLS.fixer}\` once with \`{ summary, tests_run, blockers }\`. Do not weaken or delete required tests. Never run \`make dev-up\`, \`make dev-down\` or any teardown of the task's development environment scope; the orchestrator owns it. Use \`blockers\` only for a problem that stops you from completing the fix; put caveats in \`summary\`.`,
  ]
    .filter(Boolean)
    .join("\n\n");
}

export function reviewerPrompt(
  task: Task,
  handoffSummary: string,
  stat: string,
  diff: string,
  openFindings?: string | null,
): string {
  const followUp = openFindings
    ? `This is a follow-up review. Verify whether these previous blocking findings are resolved and check for any new material defects in the updated diff:\n${openFindings}`
    : `Review the candidate against the task file and its required acceptance tests (${task.acceptance.join(", ") || "see task"}).`;
  return [
    `Independently review task ${task.id} (${task.title}). You are read-only and did not write this change.`,
    followUp,
    `Rate actual defects in required behavior, broken invariants, scope/boundary violations, or missing acceptance coverage as \`p1\`, \`blocker\`, or \`major\`. Mark minor or style notes as \`minor\` or \`nit\`.`,
    `Block only on defects inside this task's owned paths and contract: wrong or missing required behavior, broken invariants, scope violations, or missing acceptance coverage. In a follow-up review, concentrate on whether the previous findings are fixed and on defects introduced by the latest changes; do not escalate new hardening of residual edge cases. A concern that needs changes outside the owned paths, or work that belongs to another task or a missing prerequisite, is a \`minor\` follow-up (name the owning task); it is not a reason to request changes.`,
    `Submit your verdict by calling \`${REPORT_TOOLS.reviewer}\` once with \`{ verdict: "approve" | "changes_requested", summary, findings }\`.`,
    `# Task file\n${readFileSync(task.path, "utf8")}`,
    `# Handoff summary\n${handoffSummary}`,
    `# Diff stat\n${stat}`,
    `# Diff\n${diff}`,
  ].join("\n\n");
}
