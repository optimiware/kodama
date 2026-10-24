import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";
import type { Config } from "./config.ts";
import { reconcileLandingJournal } from "./git.ts";
import type { AgentRuntime } from "./runtime/sdk.ts";
import { loadMainState, loadParked, StateStore } from "./state.ts";
import { SemaphorePool, withLock } from "./system.ts";
import { runMainCheck, type StopPoint, TaskRunner } from "./task-run.ts";
import { claimable, isReady, loadTasks, ownsConflict, type Task, type TaskMap } from "./tasks.ts";

const REFRESH_MS = 15_000;

export interface ScheduleOptions {
  primary: string;
  taskRoot?: string;
  config: Config;
  runtime: AgentRuntime;
  workers?: number;
  only?: ReadonlySet<string>;
  stopAfter?: StopPoint;
  dryRun?: boolean;
  signal?: AbortSignal;
  log?: (taskId: string, msg: string) => void;
}

export interface ScheduleSummary {
  selected: string[];
  merged: string[];
  parked: Record<string, string>;
  paused: string[];
  mainCheckBlocked: boolean;
}

function resumableTasks(
  primary: string,
  tasks: TaskMap,
  parked: Record<string, string>,
  config: Config,
  only?: ReadonlySet<string>,
): Task[] {
  const workDir = join(primary, ".work");
  if (!existsSync(workDir)) return [];
  const found: Task[] = [];
  for (const task of tasks.values()) {
    if (!isReady(task, tasks, config) || parked[task.id]) continue;
    if (only && !only.has(task.id)) continue;
    const wt = join(workDir, task.slug);
    const store = new StateStore(wt);
    if (existsSync(wt) && store.exists() && store.load().phase !== "merged") {
      found.push(task);
    }
  }
  return found;
}

export async function schedule(options: ScheduleOptions): Promise<ScheduleSummary> {
  const {
    primary,
    config,
    runtime,
    workers = config.limits.workers,
    only,
    stopAfter = "merge",
    dryRun = false,
    signal: externalSignal,
    log,
  } = options;
  const rootForTasks = dryRun ? (options.taskRoot ?? primary) : primary;
  const controller = new AbortController();
  const signal = externalSignal ? AbortSignal.any([externalSignal, controller.signal]) : controller.signal;
  const home = join(primary, ".work", ".harness");
  const haltFile = join(home, "HALT");
  const isHalted = () => Boolean(signal?.aborted || existsSync(haltFile));

  const tasks = loadTasks(rootForTasks, config.tasks);
  if (only) {
    for (const id of only) {
      if (!tasks.has(id)) throw new Error(`Unknown task in --only: ${id}`);
    }
  }

  if (dryRun) {
    const parked = loadParked(primary);
    const exclude = new Set(Object.keys(parked));
    const candidates = claimable(tasks, workers, exclude, config, only);
    return {
      selected: candidates.map((t) => t.id),
      merged: [],
      parked,
      paused: [],
      mainCheckBlocked: loadMainState(primary).blocked,
    };
  }

  return withLock(
    join(home, "run.lock"),
    async () => {
      await withLock(join(home, "publication.lock"), () => reconcileLandingJournal(primary, config), {
        signal,
        haltFile,
      });
      const pool = new SemaphorePool(config.resources);
      const activeRunners = new Map<string, { runner: TaskRunner; promise: Promise<void> }>();
      const selected = new Set<string>();
      const completed = new Set<string>();
      const merged: string[] = [];
      const paused = new Set<string>();
      let mainCheckPromise: Promise<void> | null = null;

      const isLandingGated = () => {
        const ms = loadMainState(primary);
        return (
          ms.blocked ||
          mainCheckPromise !== null ||
          (config.mainCheckEvery > 0 && ms.mergesSinceCheck >= config.mainCheckEvery)
        );
      };

      try {
        while (!isHalted()) {
          const currentTasks = loadTasks(primary, config.tasks);
          const parked = loadParked(primary);
          const mainState = loadMainState(primary);

          if (
            !mainState.blocked &&
            mainCheckPromise === null &&
            config.mainCheckEvery > 0 &&
            mainState.mergesSinceCheck >= config.mainCheckEvery
          ) {
            mainCheckPromise = runMainCheck(primary, config, signal)
              .then(() => {})
              .finally(() => {
                mainCheckPromise = null;
              });
          }

          const exclude = new Set<string>([
            ...Object.keys(parked),
            ...paused,
            ...completed,
            ...activeRunners.keys(),
          ]);

          const inProgress = resumableTasks(primary, currentTasks, parked, config, only).filter(
            (t) => !exclude.has(t.id),
          );
          const fresh = claimable(currentTasks, workers, exclude, config, only);
          const queue = [...inProgress, ...fresh.filter((t) => !inProgress.some((p) => p.id === t.id))];

          for (const candidate of queue) {
            if (activeRunners.size >= workers) break;
            const activeTasks = [...activeRunners.values()].map((a) => a.runner.task);
            if (activeTasks.some((other) => ownsConflict(candidate, other).length > 0)) continue;

            const runner = new TaskRunner(primary, candidate, config, runtime, pool, stopAfter, signal, log);
            if (runner.store.exists() && runner.state.phase === "approved" && isLandingGated()) {
              continue;
            }

            selected.add(candidate.id);
            const promise = (async () => {
              try {
                for (;;) {
                  if (isHalted()) break;
                  if (runner.store.exists() && runner.state.phase === "approved" && isLandingGated()) {
                    break;
                  }
                  const outcome = await runner.step();
                  if (outcome === "continue") continue;
                  if (outcome === "done") {
                    completed.add(candidate.id);
                    if (stopAfter === "merge") {
                      merged.push(candidate.id);
                    }
                  } else if (outcome === "paused") {
                    paused.add(candidate.id);
                  }
                  break;
                }
              } finally {
                activeRunners.delete(candidate.id);
              }
            })();
            activeRunners.set(candidate.id, { runner, promise });
          }

          const waitables: Promise<unknown>[] = [...activeRunners.values()].map((a) => a.promise);
          if (mainCheckPromise) waitables.push(mainCheckPromise);
          if (waitables.length === 0) break;
          // Wake periodically so tasks unparked or unblocked meanwhile are picked up
          // without waiting for an active runner to finish.
          let wake: ReturnType<typeof setTimeout> | undefined;
          waitables.push(new Promise<void>((resolve) => (wake = setTimeout(resolve, REFRESH_MS))));
          try {
            await Promise.race(waitables);
          } finally {
            clearTimeout(wake);
          }
        }

        if (mainCheckPromise) await Promise.allSettled([mainCheckPromise]);
        await Promise.allSettled([...activeRunners.values()].map((a) => a.promise));

        const finalMain = loadMainState(primary);
        if (
          !isHalted() &&
          !finalMain.blocked &&
          stopAfter === "merge" &&
          config.mainCheckOnDrain &&
          finalMain.mergesSinceCheck > 0
        ) {
          await runMainCheck(primary, config, signal);
        }

        return {
          selected: [...selected],
          merged,
          parked: loadParked(primary),
          paused: [...paused],
          mainCheckBlocked: loadMainState(primary).blocked,
        };
      } finally {
        // Retain run.lock until checks, agents and publication work have stopped,
        // including when a runner or the periodic check rejects unexpectedly.
        controller.abort();
        await Promise.allSettled([
          ...[...activeRunners.values()].map((a) => a.promise),
          ...(mainCheckPromise ? [mainCheckPromise] : []),
        ]);
      }
    },
    { wait: false },
  );
}

export function statusSnapshot(primary: string, config: Config, taskRoot = primary) {
  const tasks = loadTasks(taskRoot, config.tasks);
  const parked = loadParked(primary);
  const main = loadMainState(primary);
  const workDir = join(primary, ".work");
  const active: Array<{ id: string; slug: string; phase: string; activity: string; repairs: number }> = [];
  if (existsSync(workDir)) {
    for (const entry of readdirSync(workDir)) {
      if (entry.startsWith(".")) continue;
      const wt = join(workDir, entry);
      const store = new StateStore(wt);
      if (!store.exists()) continue;
      const st = store.load();
      const task = [...tasks.values()].find((t) => t.slug === entry);
      if (task?.status === "done" || st.phase === "merged") continue;
      active.push({
        id: task?.id ?? entry,
        slug: entry,
        phase: st.phase,
        activity: st.activity,
        repairs: st.repairs,
      });
    }
  }
  return {
    ready: claimable(tasks, 100, new Set(Object.keys(parked)), config).map((t) => t.id),
    active,
    parked,
    main,
  };
}
