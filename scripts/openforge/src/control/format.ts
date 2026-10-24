import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import type { Config } from "../config.ts";
import type { statusSnapshot } from "../scheduler.ts";
import { loadParked, StateStore } from "../state.ts";
import { loadTasks, type TaskMap } from "../tasks.ts";

export interface QueueLiveness {
  running: boolean;
  pid: number | null;
  halted: boolean;
}

export function truncate(text: string, max = 500): string {
  const trimmed = text.trim();
  return trimmed.length <= max ? trimmed : `${trimmed.slice(0, max)}…`;
}

export function tailText(text: string, maxChars = 1500): string {
  const trimmed = text.trim();
  return trimmed.length <= maxChars ? trimmed : `...\n${trimmed.slice(-maxChars)}`;
}

export function formatStatus(snap: ReturnType<typeof statusSnapshot>, queue: QueueLiveness): string {
  const lines = [
    `Queue: ${queue.running ? `running${queue.pid ? ` pid ${queue.pid}` : ""}` : "stopped"}${queue.halted ? " (halt requested)" : ""}`,
    `Ready: ${snap.ready.join(", ") || "none"}`,
    `Active (${snap.active.length}):`,
  ];
  for (const item of snap.active) {
    lines.push(`  ${item.id} [${item.phase}] ${item.activity}, repairs=${item.repairs}`);
  }
  if (snap.active.length === 0) lines.push("  none");
  const parked = Object.entries(snap.parked);
  lines.push(`Parked (${parked.length}):`);
  for (const [id, reason] of parked) lines.push(`  ${id}: ${truncate(reason, 240)}`);
  if (parked.length === 0) lines.push("  none");
  const green = snap.main.lastGreenCommit?.slice(0, 10) ?? "none";
  lines.push(`Main check: ${snap.main.blocked ? "blocked" : "clear"}, last green ${green}`);
  return lines.join("\n");
}

export function taskDetail(primary: string, config: Config, taskId: string): string {
  const tasks = loadTasks(primary, config.tasks);
  const task = tasks.get(taskId);
  if (!task) return `Unknown task ${taskId}`;
  const parked = loadParked(primary)[task.id];
  const store = new StateStore(join(primary, ".work", task.slug));
  const state = store.exists() ? store.load() : null;
  const logPath = join(primary, ".work", task.slug, ".harness", "check.log");
  const log = existsSync(logPath) ? tailText(readFileSync(logPath, "utf8")) : "";
  const lines = [
    `${task.id} ${task.title}`,
    `status: ${task.status}`,
    `depends_on: ${task.dependsOn.join(", ") || "none"}`,
    `decision_blockers: ${task.blockers.join(", ") || "none"}`,
    `owns: ${task.owns.join(", ") || "none"}`,
    state
      ? `phase: ${state.phase}, activity: ${state.activity}, repairs=${state.repairs}, reviews=${state.reviewRounds}`
      : "phase: not claimed",
  ];
  if (state?.repairReason) lines.push(`repair: ${truncate(state.repairReason, 400)}`);
  if (parked) lines.push(`parked: ${truncate(parked, 800)}`);
  if (log) lines.push(`check log:\n${log}`);
  return lines.join("\n");
}

export function nextTaskId(tasks: TaskMap, reserved: ReadonlySet<string> = new Set()): string {
  let max = 0;
  let width = 3;
  for (const id of [...tasks.keys(), ...reserved]) {
    const match = /^T-(\d+)$/.exec(id);
    if (!match?.[1]) continue;
    max = Math.max(max, Number(match[1]));
    width = Math.max(width, match[1].length);
  }
  return `T-${String(max + 1).padStart(width, "0")}`;
}

export function taskSlug(id: string, title: string): string {
  const words = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40)
    .replace(/-+$/g, "");
  return `${id}-${words || "task"}`;
}

export function renderTask(
  task: {
    id: string;
    title: string;
    milestone: string;
    status: string;
    dependsOn: string[];
    blockers: string[];
    owns: string[];
    check?: string;
    resources: string[];
    acceptance: string[];
  },
  sections: { goal: string; contract: string; outOfScope?: string },
): string {
  const acceptance = task.acceptance.length > 0 ? task.acceptance.join(", ") : "see contract";
  return `---
id: ${task.id}
milestone: ${task.milestone || "M1"}
status: ${task.status}
depends_on: [${task.dependsOn.join(", ")}]
decision_blockers: [${task.blockers.join(", ")}]
owns: [${task.owns.join(", ")}]
check: ${task.check ?? ""}
resources: [${task.resources.join(", ")}]
acceptance: [${task.acceptance.join(", ")}]
---
# ${task.id} — ${task.title}

## Goal
${sections.goal.trim()}

## Contract
${sections.contract.trim()}

## Deliverables
Implement the contract inside the owned paths and cover the named acceptance checks.

## Acceptance
${acceptance}

## Out of scope
${sections.outOfScope?.trim() || "Product changes outside the owned paths."}
`;
}

export function replaceFrontmatterValue(content: string, key: string, value: string): string {
  const pattern = new RegExp(`^(---\\n[\\s\\S]*?\\n${key}:\\s*)(.*)$`, "m");
  if (!pattern.test(content)) throw new Error(`frontmatter lacks ${key}`);
  return content.replace(pattern, `$1${value}`);
}

export function nextDecisionId(markdown: string, reserved: ReadonlySet<string> = new Set()): string {
  let max = 0;
  for (const match of markdown.matchAll(/\bO-(\d+)\b/g)) {
    max = Math.max(max, Number(match[1]));
  }
  for (const id of reserved) {
    const match = /^O-(\d+)$/.exec(id);
    if (match?.[1]) max = Math.max(max, Number(match[1]));
  }
  return `O-${String(max + 1).padStart(2, "0")}`;
}

export function upsertDecisionRow(
  markdown: string,
  row: { id: string; status: string; neededBy: string },
): string {
  const cell = (value: string) => value.replace(/\|/g, "/").replace(/\s+/g, " ").trim();
  const line = `| ${cell(row.id)} | ${cell(row.status)} | ${cell(row.neededBy)} |`;
  const lines = markdown.split("\n");
  const header = lines.findIndex((item) => item.startsWith("| ID |") && item.includes("Needed by"));
  if (header < 0 || !lines[header + 1]?.startsWith("| ---")) {
    throw new Error("decisions file has no ID / Status / Needed by table");
  }
  const existing = lines.findIndex((item, index) => index > header + 1 && item.startsWith(`| ${row.id} |`));
  if (existing >= 0) {
    lines[existing] = line;
    return lines.join("\n");
  }
  let insertAt = header + 2;
  for (let i = header + 2; i < lines.length; i++) {
    if (!lines[i]?.startsWith("| ")) break;
    insertAt = i + 1;
  }
  lines.splice(insertAt, 0, line);
  return lines.join("\n");
}
