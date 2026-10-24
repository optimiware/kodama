import { spawn } from "node:child_process";
import { closeSync, existsSync, mkdirSync, openSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Config } from "../config.ts";
import { appendEvent, unparkTask } from "../state.ts";
import { isPidAlive, lockHolder, readJSON, writeJSON } from "../system.ts";
import { once } from "./journal.ts";

export interface QueueLiveness {
  running: boolean;
  pid: number | null;
  halted: boolean;
}

const homeOf = (primary: string) => join(primary, ".work", ".harness");
const operatorHome = (primary: string) => join(homeOf(primary), "operator");

export function queueLiveness(primary: string): QueueLiveness {
  const home = homeOf(primary);
  const holder = lockHolder(join(home, "run.lock"));
  const recorded = readJSON<{ pid?: number }>(join(operatorHome(primary), "queue.json"), {});
  const recordedAlive = typeof recorded.pid === "number" && isPidAlive(recorded.pid);
  const pid = holder?.alive ? holder.pid : recordedAlive ? (recorded.pid as number) : null;
  return {
    running: Boolean(holder?.alive || recordedAlive),
    pid,
    halted: existsSync(join(home, "HALT")),
  };
}

export function requestHalt(primary: string): void {
  const home = homeOf(primary);
  mkdirSync(home, { recursive: true });
  writeFileSync(join(home, "HALT"), `${new Date().toISOString()}\n`);
}

export function clearHalt(primary: string): void {
  rmSync(join(homeOf(primary), "HALT"), { force: true });
}

export interface SpawnRequest {
  command: string[];
  cwd: string;
  logPath: string;
  env: NodeJS.ProcessEnv;
}

export interface ProcessLauncher {
  spawn(request: SpawnRequest): { pid: number };
}

export const nodeLauncher: ProcessLauncher = {
  spawn(request) {
    mkdirSync(join(request.logPath, ".."), { recursive: true });
    const fd = openSync(request.logPath, "a");
    try {
      const child = spawn(request.command[0] as string, request.command.slice(1), {
        cwd: request.cwd,
        env: request.env,
        detached: true,
        stdio: ["ignore", fd, fd],
      });
      child.unref();
      if (!child.pid) throw new Error("Failed to start process");
      return { pid: child.pid };
    } finally {
      closeSync(fd);
    }
  },
};

function cliCommand(args: string[]): string[] {
  return [process.execPath, fileURLToPath(new URL("../cli.ts", import.meta.url)), ...args];
}

export async function startQueue(
  primary: string,
  config: Config,
  workers = config.limits.workers,
  launcher: ProcessLauncher = nodeLauncher,
): Promise<string> {
  if (!Number.isInteger(workers) || workers < 1 || workers > 64) {
    throw new Error("workers must be an integer from 1 to 64");
  }
  const live = queueLiveness(primary);
  if (live.running) return `Queue already running${live.pid ? ` (pid ${live.pid})` : ""}`;
  if (live.halted) throw new Error("Queue is halted; resume it instead of starting over the halt file");
  const dir = operatorHome(primary);
  mkdirSync(dir, { recursive: true });
  const logPath = join(dir, "queue.log");
  writeFileSync(logPath, `\n--- start ${new Date().toISOString()} workers=${workers} ---\n`, { flag: "a" });
  const child = launcher.spawn({
    command: cliCommand(["run", "--workers", String(workers)]),
    cwd: primary,
    logPath,
    env: process.env,
  });
  writeJSON(join(dir, "queue.json"), { pid: child.pid, startedAt: new Date().toISOString(), workers });
  await appendEvent(primary, "queue_started", "-", { pid: child.pid, workers });
  return `Queue started (pid ${child.pid}, workers ${workers})`;
}

export async function haltQueue(primary: string): Promise<string> {
  requestHalt(primary);
  await appendEvent(primary, "queue_halt_requested", "-", {});
  const deadline = Date.now() + 250;
  while (Date.now() < deadline) {
    if (!queueLiveness(primary).running) return "Queue halted";
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  return "Halt requested. Workers stop when the current agent or check yields.";
}

export async function resumeQueue(
  primary: string,
  config: Config,
  workers?: number,
  launcher: ProcessLauncher = nodeLauncher,
): Promise<string> {
  if (queueLiveness(primary).running) {
    return "Queue is still running. Wait for halt to finish before resuming.";
  }
  clearHalt(primary);
  const started = await startQueue(primary, config, workers, launcher);
  await appendEvent(primary, "queue_resumed", "-", {});
  return started.replace("Queue started", "Queue resumed");
}

export async function unparkOnce(primary: string, taskId: string, operationId: string): Promise<string> {
  const outcome = await once(
    primary,
    operationId,
    async () => {
      await unparkTask(primary, taskId);
      return `Unparked ${taskId}`;
    },
    { retryFailed: true },
  );
  return outcome.repeated ? `${outcome.result} (already applied)` : outcome.result;
}

export async function retryMainCheck(
  primary: string,
  operationId: string,
  launcher: ProcessLauncher = nodeLauncher,
): Promise<string> {
  const holder = lockHolder(join(homeOf(primary), "publication.lock"));
  if (holder?.alive) return "A publication or main check is already running";
  const outcome = await once(primary, operationId, async () => {
    const dir = operatorHome(primary);
    mkdirSync(dir, { recursive: true });
    const child = launcher.spawn({
      command: cliCommand(["batch", "--retry"]),
      cwd: primary,
      logPath: join(dir, "main-check.log"),
      env: process.env,
    });
    await appendEvent(primary, "main_check_started", "-", { pid: child.pid });
    return `Main check started (pid ${child.pid})`;
  });
  return outcome.repeated ? `${outcome.result} (already applied)` : outcome.result;
}
