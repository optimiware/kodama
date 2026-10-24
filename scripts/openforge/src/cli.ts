#!/usr/bin/env bun
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { loadConfig } from "./config.ts";
import { loadDotenv } from "./dotenv.ts";
import { runTaskCheck } from "./gate.ts";
import { runTelegram } from "./operator/command.ts";
import { credentialsFromEnv } from "./runtime/credentials.ts";
import { type AgentRuntime, createRuntime, modelRef } from "./runtime/sdk.ts";
import { schedule, statusSnapshot } from "./scheduler.ts";
import { loadMainState, unparkTask } from "./state.ts";
import { primaryRoot, SemaphorePool } from "./system.ts";
import { runMainCheck, type StopPoint } from "./task-run.ts";
import { loadTasks, ready, validateTasks, writeIndex } from "./tasks.ts";

const USAGE = `OpenForge — lightweight autonomous SDLC harness
  bun scripts/openforge/src/cli.ts <command>

Commands:
  validate                                  Validate openforge.json, task graph, and index
  ready                                     List ready tasks in priority order
  index [--write]                           Check or regenerate the task index
  run [--workers N] [--only ID...] [--stop-after plan|implement|check|review|merge] [--dry-run]
  check-task TASK                           Run the configured check and acceptance gate for TASK
  status [--json]                           Show active worktrees, parked tasks, and main state
  batch [--run|--retry]                     Show or run the periodic main branch check
  main-check [--run|--retry]                Alias for batch
  unpark TASK                               Clear a parked task so it can be scheduled again
  doctor                                    Verify config, embedded SDK startup, and role models
  telegram                                  Run the private Telegram operator until interrupted
`;

const idleRuntime: AgentRuntime = {
  async run() {
    throw new Error("No runtime in dry-run");
  },
  async close() {},
};

export async function main(argv = process.argv.slice(2), cwd = process.cwd()): Promise<number> {
  const command = argv[0];
  if (!command || command === "help" || command === "--help") {
    console.log(USAGE);
    return 0;
  }

  const { values, positionals } = parseArgs({
    args: argv.slice(1),
    allowPositionals: true,
    options: {
      workers: { type: "string" },
      only: { type: "string", multiple: true },
      "stop-after": { type: "string" },
      "dry-run": { type: "boolean" },
      json: { type: "boolean" },
      write: { type: "boolean" },
      run: { type: "boolean" },
      retry: { type: "boolean" },
    },
  });

  const root = await primaryRoot(cwd);
  const loaded = loadDotenv(join(root, ".env"));
  if (loaded.length > 0) console.error(`dotenv: loaded ${loaded.length} variable(s)`);
  const config = loadConfig(cwd);
  const tasks = loadTasks(cwd, config.tasks);
  const indexPath = join(cwd, config.index);

  if (command === "validate") {
    const errors = validateTasks(tasks, indexPath, config);
    if (errors.length > 0) {
      for (const err of errors) console.error(err);
      return 1;
    }
    console.log(`PASS task graph, ownership and index (${tasks.size} tasks)`);
    return 0;
  }

  if (command === "ready") {
    for (const task of ready(tasks, config)) {
      console.log(`${task.id}  ${task.title}`);
    }
    return 0;
  }

  if (command === "index") {
    const ok = writeIndex(tasks, indexPath, !values.write, config);
    if (!ok && !values.write) {
      console.error(`${config.index} is out of date; run \`bun scripts/openforge/src/cli.ts index --write\``);
      return 1;
    }
    return 0;
  }

  if (command === "status") {
    const snap = statusSnapshot(root, config, cwd);
    if (values.json) {
      console.log(JSON.stringify(snap, null, 2));
    } else {
      console.log(`Ready (${snap.ready.length}): ${snap.ready.join(", ") || "none"}`);
      console.log(`Active (${snap.active.length}):`);
      for (const a of snap.active) {
        console.log(`  ${a.id} [${a.phase}] (${a.activity}, repairs=${a.repairs})`);
      }
      const parkedIds = Object.keys(snap.parked);
      console.log(`Parked (${parkedIds.length}):`);
      for (const [id, reason] of Object.entries(snap.parked)) {
        console.log(`  ${id}: ${reason}`);
      }
      console.log(
        `Main check: blocked=${snap.main.blocked}, lastGreen=${snap.main.lastGreenCommit ?? "none"}`,
      );
    }
    return 0;
  }

  if (command === "unpark") {
    const taskId = positionals[0];
    if (!taskId) throw new Error("unpark requires a TASK ID");
    await unparkTask(root, taskId);
    console.log(`Unparked ${taskId}`);
    return 0;
  }

  if (command === "batch" || command === "main-check") {
    if (values.run || values.retry) {
      const res = await runMainCheck(root, config);
      console.log(`${res.message}\nLog: ${res.logPath}`);
      return res.ok ? 0 : 1;
    }
    const state = loadMainState(root);
    console.log(JSON.stringify(state, null, 2));
    return state.blocked ? 1 : 0;
  }

  if (command === "check-task") {
    const taskId = positionals[0];
    if (!taskId) throw new Error("check-task requires a TASK ID");
    const task = tasks.get(taskId);
    if (!task) throw new Error(`Unknown task: ${taskId}`);
    const pool = new SemaphorePool(config.resources);
    const home = join(root, ".work", ".harness");
    const outcome = await runTaskCheck(cwd, home, task, config, pool);
    if (outcome.status === "passed") {
      console.log(`PASS ${task.id} (${outcome.commit.slice(0, 10)})`);
      return 0;
    }
    console.error(`FAIL ${task.id} [${outcome.status}]: ${outcome.reason}`);
    if ("detail" in outcome && outcome.detail) console.error(outcome.detail);
    return outcome.status === "infra_pause" ? 75 : 1;
  }

  if (command === "doctor") {
    const errors = validateTasks(tasks, indexPath, config);
    if (errors.length > 0) {
      for (const err of errors) console.error(err);
      return 1;
    }
    const tmp = mkdtempSync(join(tmpdir(), "openforge-doctor-"));
    try {
      const runtime = await createRuntime(root, config, { databasePath: join(tmp, "doctor.db") });
      try {
        for (const [role, entry] of Object.entries(config.roles)) {
          modelRef(entry.model);
          void role;
        }
        const providers = new Set(
          Object.values(config.roles).map((role) => role.model.slice(0, role.model.indexOf("/"))),
        );
        const fromEnv = Object.keys(credentialsFromEnv(providers));
        console.log(
          `PASS OpenForge doctor (config, task graph, and embedded SDK host; env keys: ${fromEnv.join(", ") || "none"})`,
        );
        return 0;
      } finally {
        await runtime.close();
      }
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  }

  if (command === "telegram") {
    const controller = new AbortController();
    const stop = () => controller.abort();
    process.on("SIGINT", stop);
    process.on("SIGTERM", stop);
    try {
      await runTelegram(root, config, controller.signal);
      return 0;
    } finally {
      process.off("SIGINT", stop);
      process.off("SIGTERM", stop);
    }
  }

  if (command === "run") {
    const workers = values.workers ? Number(values.workers) : config.limits.workers;
    if (!Number.isInteger(workers) || workers < 1) {
      throw new Error("--workers must be a positive integer");
    }
    const stopAfter = (values["stop-after"] ?? "merge") as StopPoint;
    if (!["plan", "implement", "check", "review", "merge"].includes(stopAfter)) {
      throw new Error(`Invalid --stop-after: ${stopAfter}`);
    }
    const onlyIds = values.only?.flatMap((v) => v.split(",")).filter(Boolean);
    const only = onlyIds && onlyIds.length > 0 ? new Set(onlyIds) : undefined;
    const runtime = values["dry-run"] ? idleRuntime : await createRuntime(root, config);
    try {
      const summary = await schedule({
        primary: root,
        taskRoot: cwd,
        config,
        runtime,
        workers,
        only,
        stopAfter,
        dryRun: Boolean(values["dry-run"]),
      });
      if (values["dry-run"]) {
        console.log(
          `Dry-run selected (${summary.selected.length}): ${summary.selected.join(", ") || "none"}`,
        );
      }
      return summary.mainCheckBlocked ? 1 : 0;
    } finally {
      await runtime.close();
    }
  }

  throw new Error(`Unknown command: ${command}`);
}

if (import.meta.main) {
  main().then(
    (code) => process.exit(code),
    (err) => {
      console.error(err instanceof Error ? err.message : String(err));
      process.exit(1);
    },
  );
}
