import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { z } from "zod";
import { readJSON, withLock, writeJSON } from "./system.ts";

export const PHASES = ["claimed", "planned", "implemented", "verified", "approved", "merged"] as const;
export type Phase = (typeof PHASES)[number];

export const taskStateSchema = z.object({
  phase: z.enum(PHASES).default("claimed"),
  baseCommit: z.string().min(1),
  candidateCommit: z.string().nullable().default(null),
  patchId: z.string().nullable().default(null),
  verifiedPatchId: z.string().nullable().default(null),
  approvedPatchId: z.string().nullable().default(null),
  qualificationContext: z.string().nullable().default(null),
  writerSessionID: z.string().nullable().default(null),
  reviewerSessionID: z.string().nullable().default(null),
  repairs: z.number().int().nonnegative().default(0),
  reviewRounds: z.number().int().nonnegative().default(0),
  infraRetries: z.number().int().nonnegative().default(0),
  repairReason: z.string().nullable().default(null),
  repairDetail: z.string().nullable().default(null),
  openFindings: z.string().nullable().default(null),
  activity: z.string().default("idle"),
  requeueGeneration: z.number().int().nonnegative().default(0),
});

export type TaskState = z.infer<typeof taskStateSchema>;

export class StateStore {
  readonly path: string;
  constructor(readonly worktree: string) {
    this.path = join(worktree, ".harness", "state.json");
  }

  exists(): boolean {
    return existsSync(this.path);
  }

  load(): TaskState {
    const raw = readJSON<unknown>(this.path);
    return taskStateSchema.parse(raw);
  }

  save(patch: Partial<TaskState>): TaskState {
    const current = this.exists()
      ? taskStateSchema.parse(readJSON<unknown>(this.path))
      : taskStateSchema.parse({ baseCommit: patch.baseCommit ?? "HEAD" });
    const updated = taskStateSchema.parse({ ...current, ...patch });
    writeJSON(this.path, updated);
    return updated;
  }
}

export interface MainCheckResult {
  ok: boolean;
  commit: string;
  finishedAt: string;
  message: string;
  logPath: string;
}

export interface MainState {
  lastGreenCommit: string | null;
  lastCountedCommit?: string | null;
  mergesSinceCheck: number;
  blocked: boolean;
  lastResult: MainCheckResult | null;
}

const mainStatePath = (primary: string) => join(primary, ".work", ".harness", "main.json");

export function loadMainState(primary: string): MainState {
  const path = mainStatePath(primary);
  if (existsSync(path)) {
    return readJSON<MainState>(path);
  }
  return {
    lastGreenCommit: null,
    mergesSinceCheck: 0,
    blocked: false,
    lastResult: null,
  };
}

export function saveMainState(primary: string, state: MainState): void {
  writeJSON(mainStatePath(primary), state);
}

const parkedPath = (primary: string) => join(primary, ".work", ".harness", "parked.json");
const generationsPath = (primary: string) => join(primary, ".work", ".harness", "generations.json");

export function loadParked(primary: string): Record<string, string> {
  return readJSON<Record<string, string>>(parkedPath(primary), {});
}

export function loadGenerations(primary: string): Record<string, number> {
  return readJSON<Record<string, number>>(generationsPath(primary), {});
}

export async function parkTask(primary: string, taskId: string, reason: string): Promise<void> {
  const home = join(primary, ".work", ".harness");
  await withLock(join(home, "parked.lock"), async () => {
    const parked = loadParked(primary);
    parked[taskId] = reason;
    writeJSON(parkedPath(primary), parked);
  });
  await appendEvent(primary, "parked", taskId, { reason });
}

export async function unparkTask(primary: string, taskId: string): Promise<void> {
  const home = join(primary, ".work", ".harness");
  await withLock(join(home, "parked.lock"), async () => {
    const parked = loadParked(primary);
    delete parked[taskId];
    writeJSON(parkedPath(primary), parked);
    const gens = loadGenerations(primary);
    gens[taskId] = (gens[taskId] ?? 0) + 1;
    writeJSON(generationsPath(primary), gens);
  });
  await appendEvent(primary, "unparked", taskId, {});
}

export async function appendEvent(
  primary: string,
  type: string,
  task: string,
  data: Record<string, unknown> = {},
): Promise<void> {
  const dir = join(primary, ".work", ".harness");
  mkdirSync(dir, { recursive: true });
  const record = JSON.stringify({ ts: new Date().toISOString(), type, task, ...data });
  writeFileSync(join(dir, "events.jsonl"), `${record}\n`, { flag: "a" });
}
