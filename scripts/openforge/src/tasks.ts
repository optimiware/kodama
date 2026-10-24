import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { basename, dirname, join, parse as parsePath, relative, resolve } from "node:path";
import type { Config } from "./config.ts";

export const STATUSES = ["planned", "blocked", "deferred", "done"] as const;
export type TaskStatus = (typeof STATUSES)[number];

export const READY_START = "<!-- harness:summary:start -->";
export const READY_END = "<!-- harness:summary:end -->";
export const INDEX_COMMAND = "bun scripts/openforge/src/cli.ts index --write";

const ALLOWED_KEYS = new Set([
  "id",
  "milestone",
  "status",
  "depends_on",
  "decision_blockers",
  "owns",
  "check",
  "acceptance",
  "resources",
  "plan",
  "review",
]);

export interface Task {
  id: string;
  path: string;
  slug: string;
  milestone: string;
  status: string;
  dependsOn: string[];
  blockers: string[];
  owns: string[];
  check?: string;
  acceptance: string[];
  resources: string[];
  plan: boolean;
  review: "agent" | "human";
  title: string;
  body: string;
  unknownKeys: string[];
}

export type TaskMap = Map<string, Task>;

const byCodePoint = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);

export function parseList(value: string): string[] {
  const text = value.trim();
  if (!(text.startsWith("[") && text.endsWith("]"))) {
    throw new Error(`expected a [list], got ${text}`);
  }
  return text
    .slice(1, -1)
    .split(",")
    .map((item) => item.trim())
    .filter((item) => item !== "");
}

function staysInsideCheckout(root: string, candidate: string): boolean {
  if (!candidate || candidate.startsWith("/") || candidate.startsWith("\\")) return false;
  const parts = candidate.split(/[\\/]+/);
  if (
    parts.includes("..") ||
    parts.includes(".git") ||
    parts.includes(".work") ||
    parts.includes(".harness")
  ) {
    return false;
  }
  const resolved = resolve(root, candidate);
  const rel = relative(resolve(root), resolved);
  return rel !== "" && !rel.startsWith("..") && !rel.startsWith("/");
}

export function parseTask(path: string): Task {
  const text = readFileSync(path, "utf8");
  const match = /^---\n([\s\S]*?)\n---\n([\s\S]*)/.exec(text);
  if (!match) throw new Error(`${path}: missing frontmatter`);
  const meta: Record<string, string> = {};
  for (const line of (match[1] as string).split(/\r?\n|\r/)) {
    const at = line.indexOf(":");
    if (at >= 0) meta[line.slice(0, at).trim()] = line.slice(at + 1).trim();
  }
  for (const key of ["id", "status", "depends_on", "owns"]) {
    if (!(key in meta)) throw new Error(`${path}: frontmatter lacks ${key}`);
  }
  const unknownKeys = Object.keys(meta).filter((k) => !ALLOWED_KEYS.has(k));
  const body = match[2] as string;
  const heading = /^# (.+)$/m.exec(body);
  const id = meta.id as string;
  const reviewRaw = meta.review ?? "agent";
  if (reviewRaw !== "agent" && reviewRaw !== "human") {
    throw new Error(`${path}: review must be agent or human, got ${reviewRaw}`);
  }
  const planRaw = meta.plan ?? "false";
  if (planRaw !== "true" && planRaw !== "false") {
    throw new Error(`${path}: plan must be true or false, got ${planRaw}`);
  }
  return {
    id,
    path,
    slug: parsePath(basename(path)).name,
    milestone: meta.milestone ?? "",
    status: meta.status as string,
    dependsOn: parseList(meta.depends_on as string),
    blockers: parseList(meta.decision_blockers ?? "[]"),
    owns: parseList(meta.owns as string),
    ...(meta.check ? { check: meta.check } : {}),
    acceptance: "acceptance" in meta ? parseList(meta.acceptance as string) : [],
    resources: "resources" in meta ? parseList(meta.resources as string) : [],
    plan: planRaw === "true",
    review: reviewRaw,
    title: heading ? (heading[1] as string) : id,
    body,
    unknownKeys,
  };
}

export function loadTasks(root: string, pattern = "docs/tasks/T-*.md"): TaskMap {
  const dir = join(root, dirname(pattern));
  const filePattern = basename(pattern);
  const prefix = filePattern.split("*")[0] ?? "T-";
  const suffix = filePattern.split("*")[1] ?? ".md";
  const tasks: TaskMap = new Map();
  const names = readdirSync(dir)
    .filter((name) => name.startsWith(prefix) && name.endsWith(suffix))
    .sort(byCodePoint);
  for (const name of names) {
    const path = join(dir, name);
    if (!statSync(path).isFile()) continue;
    const task = parseTask(path);
    if (tasks.has(task.id)) throw new Error(`duplicate task id ${task.id}`);
    tasks.set(task.id, task);
  }
  return tasks;
}

export function section(task: Task, heading: string): string {
  const escaped = heading.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = new RegExp(`^## ${escaped}\\n([\\s\\S]*?)(?=^## |(?![\\s\\S]))`, "m").exec(task.body);
  return match ? (match[1] as string).trim() : "";
}

export function ancestors(tasks: TaskMap): Map<string, Set<string>> {
  const cache = new Map<string, Set<string>>();
  const visit = (id: string, stack: ReadonlySet<string>): Set<string> => {
    const cached = cache.get(id);
    if (cached) return cached;
    const task = tasks.get(id);
    if (stack.has(id) || !task) return new Set();
    const found = new Set<string>();
    const next = new Set(stack).add(id);
    for (const dep of task.dependsOn) {
      found.add(dep);
      for (const item of visit(dep, next)) found.add(item);
    }
    cache.set(id, found);
    return found;
  };
  return new Map([...tasks.keys()].map((id) => [id, visit(id, new Set())]));
}

export function findCycle(tasks: TaskMap): string[] | null {
  const state = new Map<string, "open" | "done">();
  const visit = (id: string, path: string[]): string[] | null => {
    const seen = state.get(id);
    if (seen === "done") return null;
    if (seen === "open") return [...path.slice(path.indexOf(id)), id];
    state.set(id, "open");
    for (const dep of (tasks.get(id) as Task).dependsOn) {
      if (tasks.has(dep)) {
        const cycle = visit(dep, [...path, id]);
        if (cycle) return cycle;
      }
    }
    state.set(id, "done");
    return null;
  };
  for (const id of tasks.keys()) {
    const cycle = visit(id, []);
    if (cycle) return cycle;
  }
  return null;
}

export function pathsOverlap(a: string, b: string): boolean {
  return a === b || (a.endsWith("/") && b.startsWith(a)) || (b.endsWith("/") && a.startsWith(b));
}

export function ownsConflict(a: Task, b: Task): Array<[string, string]> {
  const clashes: Array<[string, string]> = [];
  for (const x of a.owns) for (const y of b.owns) if (pathsOverlap(x, y)) clashes.push([x, y]);
  return clashes;
}

export function validationProblems(task: Task, config?: Config): string[] {
  const problems: string[] = [];
  if (task.unknownKeys.length > 0) {
    problems.push(`${task.id}: unknown frontmatter keys: ${task.unknownKeys.join(", ")}`);
  }
  if (!task.owns.length) problems.push(`${task.id}: empty owns`);
  if ((task.status === "planned" || task.status === "blocked") && !(task.check || config?.check)) {
    problems.push(`${task.id}: scheduled task requires a check command`);
  }
  if (config && task.resources.length > 0) {
    for (const r of task.resources) {
      if (!(r in config.resources)) {
        problems.push(`${task.id}: unknown resource ${r}`);
      }
    }
  }
  return problems;
}

export function isReady(task: Task, tasks: TaskMap, config?: Config): boolean {
  return (
    task.status === "planned" &&
    validationProblems(task, config).length === 0 &&
    task.blockers.length === 0 &&
    task.dependsOn.every((dep) => tasks.get(dep)?.status === "done")
  );
}

export function downstreamDepth(tasks: TaskMap): Map<string, number> {
  const dependents = new Map<string, string[]>([...tasks.keys()].map((id) => [id, []]));
  for (const task of tasks.values()) {
    for (const dep of task.dependsOn) dependents.get(dep)?.push(task.id);
  }
  const cache = new Map<string, number>();
  const depth = (id: string): number => {
    const cached = cache.get(id);
    if (cached !== undefined) return cached;
    cache.set(id, 0);
    let longest = 0;
    for (const d of dependents.get(id) as string[]) {
      if ((tasks.get(d) as Task).status !== "done") longest = Math.max(longest, 1 + depth(d));
    }
    cache.set(id, longest);
    return longest;
  };
  return new Map([...tasks.keys()].map((id) => [id, depth(id)]));
}

export function ready(tasks: TaskMap, config?: Config): Task[] {
  const depth = downstreamDepth(tasks);
  return [...tasks.values()]
    .filter((task) => isReady(task, tasks, config))
    .sort((a, b) => (depth.get(b.id) as number) - (depth.get(a.id) as number) || byCodePoint(a.id, b.id));
}

export function claimable(
  tasks: TaskMap,
  count: number,
  exclude: ReadonlySet<string> = new Set(),
  config?: Config,
  only?: ReadonlySet<string>,
): Task[] {
  const chosen: Task[] = [];
  for (const task of ready(tasks, config)) {
    if (exclude.has(task.id)) continue;
    if (only && !only.has(task.id)) continue;
    if (chosen.every((other) => ownsConflict(task, other).length === 0)) chosen.push(task);
    if (chosen.length >= count) break;
  }
  return chosen;
}

export function renderSummary(tasks: TaskMap, config?: Config): string {
  const link = (t: Task): string => `[${t.id}](${basename(t.path)})`;
  const readyTasks = ready(tasks, config);
  const doneTasks = [...tasks.values()].filter((t) => t.status === "done");
  const readyLine =
    readyTasks.length > 0 ? `**Ready now:** ${readyTasks.map(link).join(", ")}.` : "**Ready now:** none.";
  const doneLine = doneTasks.length > 0 ? `**Done:** ${doneTasks.map(link).join(", ")}.` : "**Done:** none.";
  return [
    READY_START,
    "",
    readyLine,
    "",
    doneLine,
    "",
    `Generated from task frontmatter by \`${INDEX_COMMAND}\`; do not edit by hand.`,
    "",
    READY_END,
  ].join("\n");
}

function tableTitle(task: Task): string {
  const prefix = `${task.id} — `;
  const title = task.title.startsWith(prefix) ? task.title.slice(prefix.length) : task.title;
  return `${task.id} ${title}`;
}

export function renderIndexTable(tasks: TaskMap): string {
  const lines = ["| Task | Dependencies | Status / decision |", "| --- | --- | --- |"];
  for (const task of tasks.values()) {
    const deps = task.dependsOn.length > 0 ? task.dependsOn.join(", ") : "—";
    const status = task.blockers.length > 0 ? `${task.status} (${task.blockers.join(", ")})` : task.status;
    lines.push(`| [${tableTitle(task)}](${basename(task.path)}) | ${deps} | ${status} |`);
  }
  return lines.join("\n");
}

export function writeIndex(tasks: TaskMap, indexPath: string, checkOnly = false, config?: Config): boolean {
  const current = readFileSync(indexPath, "utf8");
  const summaryRe = new RegExp(
    `${READY_START.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}[\\s\\S]*?${READY_END.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`,
  );
  if (!summaryRe.test(current)) {
    throw new Error(`${indexPath}: missing ${READY_START} .. ${READY_END} block`);
  }
  let updated = current.replace(summaryRe, renderSummary(tasks, config));
  const tableRe = /(## Dependency index\n\n)(\| Task \|[\s\S]*?)(\n\n## |\s*$)/;
  if (!tableRe.test(updated)) {
    throw new Error(`${indexPath}: missing ## Dependency index table`);
  }
  updated = updated.replace(tableRe, `$1${renderIndexTable(tasks)}$3`);
  if (updated === current) return true;
  if (!checkOnly) writeFileSync(indexPath, updated);
  return false;
}

export function validateTasks(tasks: TaskMap, indexPath?: string, config?: Config): string[] {
  const errors: string[] = [];
  const sample = tasks.values().next().value;
  const root = sample ? resolve(dirname(sample.path), "../..") : null;
  for (const task of tasks.values()) {
    if (!(STATUSES as readonly string[]).includes(task.status)) {
      errors.push(`${task.id}: unknown status ${task.status}`);
    }
    errors.push(...validationProblems(task, config));
    if (root) {
      for (const owned of task.owns) {
        if (!staysInsideCheckout(root, owned)) {
          errors.push(`${task.id}: owned path escapes checkout: ${owned}`);
        }
      }
    }
    for (const dep of task.dependsOn) {
      if (!tasks.has(dep)) errors.push(`${task.id}: unknown dependency ${dep}`);
    }
    if (task.status === "blocked" && task.blockers.length === 0) {
      errors.push(`${task.id}: status is blocked but decision_blockers is empty`);
    }
    if (task.status === "done" && task.blockers.length > 0) {
      errors.push(`${task.id}: status is done but decision_blockers is non-empty`);
    }
  }
  const cycle = findCycle(tasks);
  if (cycle) errors.push(`dependency cycle: ${cycle.join(" -> ")}`);

  const anc = ancestors(tasks);
  const items = [...tasks.values()].filter((t) => t.status === "planned" || t.status === "blocked");
  for (let i = 0; i < items.length; i++) {
    const a = items[i] as Task;
    for (const b of items.slice(i + 1)) {
      if (anc.get(a.id)?.has(b.id) || anc.get(b.id)?.has(a.id)) continue;
      for (const [x, y] of ownsConflict(a, b)) {
        errors.push(`concurrent owns conflict: ${a.id} (${x}) and ${b.id} (${y}) have no dependency order`);
      }
    }
  }
  if (indexPath && !writeIndex(tasks, indexPath, true, config)) {
    errors.push(`${indexPath} is out of date; run \`${INDEX_COMMAND}\``);
  }
  return errors;
}
