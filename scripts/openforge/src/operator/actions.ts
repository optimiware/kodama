import type { Config } from "../config.ts";
import { publishChange } from "../control/changes.ts";
import { formatStatus, taskDetail } from "../control/format.ts";
import { statusSnapshot } from "../scheduler.ts";
import { loadMainState } from "../state.ts";
import type { Authorized } from "../telegram/access.ts";
import { parseCommand } from "../telegram/access.ts";
import type { OperatorControls } from "./plugin.ts";
import type { ConversationalRuntime } from "./runtime.ts";

export const HELP = `Commands:
/status — queue, ready, active, parked
/task T-001 — one task, its park reason, and the check-log tail
/halt — stop the queue after current work yields
/resume [workers] — clear halt and start the queue
/unpark T-001 — requeue a parked task
/retry-main — rerun the main check
/publish <id> — publish a prepared task or decision
/cancel — stop the current conversation turn
/help — this message

Other messages go to the operator. A prepared change is not on the base branch until you press Publish or send /publish.`;

export interface OwnerReply {
  text: string;
  buttons?: Array<{ label: string; data: string }>;
  callbackId?: string;
  sessionID?: string | null;
}

export interface ActionContext {
  primary: string;
  config: Config;
  controls: OperatorControls;
  runtime: ConversationalRuntime | null;
  sessionID: string | null;
  signal?: AbortSignal;
}

function workers(arg: string): number | undefined {
  if (!arg) return undefined;
  const value = Number(arg);
  if (!Number.isInteger(value) || value < 1 || value > 64)
    throw new Error("workers must be an integer from 1 to 64");
  return value;
}

export async function handleAuthorized(
  access: Authorized & { ok: true },
  ctx: ActionContext,
): Promise<OwnerReply> {
  const callbackId = access.kind === "callback" ? access.callbackId : undefined;
  if (access.kind === "callback") {
    return { ...(await handleCallback(access.data, access.updateId, ctx)), callbackId };
  }
  const command = parseCommand(access.text);
  if (command) return handleCommand(command.name, command.arg, access.updateId, ctx);
  if (!ctx.runtime) {
    return {
      text: "The model is unavailable. Use /status, /halt, /resume, /unpark, /retry-main, and /publish.",
    };
  }
  const operationPrefix = `tg${access.updateId}`;
  const prompt = [
    "Factory snapshot (authoritative; prefer it over earlier memory):",
    ctx.controls.status(),
    "",
    `Owner message:\n${access.text}`,
    "",
    `For any mutation or prepared change in this turn, operation_id must start with ${operationPrefix}.`,
    "Do not claim a task or decision is published. Prepared changes stay off the base branch until the owner confirms.",
    "Never reveal tokens, API keys, or the contents of credential files.",
  ].join("\n");
  const result = await ctx.runtime.reply({
    text: prompt,
    sessionID: ctx.sessionID,
    operationPrefix,
    signal: ctx.signal,
  });
  const extra = result.confirms.map((item) => item.summary).join("\n\n");
  const text = [result.text, extra].filter(Boolean).join("\n\n");
  return {
    text,
    sessionID: result.sessionID,
    buttons: result.confirms.map((item) => ({
      label: item.label.slice(0, 40),
      data: `p:${item.operationId}`,
    })),
  };
}

async function handleCommand(
  name: string,
  arg: string,
  updateId: number,
  ctx: ActionContext,
): Promise<OwnerReply> {
  switch (name) {
    case "start":
    case "help":
      return { text: HELP };
    case "status":
      return { text: ctx.controls.status() };
    case "task":
      if (!/^T-\d+$/.test(arg)) return { text: "Usage: /task T-001" };
      return { text: ctx.controls.task(arg) };
    case "halt":
      return { text: await ctx.controls.halt() };
    case "resume":
      return { text: await ctx.controls.resume(workers(arg)) };
    case "unpark":
      if (!/^T-\d+$/.test(arg)) return { text: "Usage: /unpark T-001" };
      return { text: await ctx.controls.unpark(arg, `tg${updateId}-unpark`) };
    case "retry-main":
    case "retry_main":
      return { text: await ctx.controls.retryMain(`tg${updateId}-main`) };
    case "publish":
      if (!arg) return { text: "Usage: /publish <operation-id>" };
      return { text: await publishChange(ctx.primary, ctx.config, arg) };
    case "cancel":
      await ctx.runtime?.cancel();
      return { text: "Cancelled the current conversation turn." };
    case "events":
      return { text: ctx.controls.events(15) };
    default:
      return { text: `Unknown command /${name}.\n\n${HELP}` };
  }
}

async function handleCallback(data: string, updateId: number, ctx: ActionContext): Promise<OwnerReply> {
  if (data === "s") return { text: ctx.controls.status() };
  if (data === "h") return { text: await ctx.controls.halt() };
  if (data === "r") return { text: await ctx.controls.resume() };
  if (data.startsWith("p:")) return { text: await publishChange(ctx.primary, ctx.config, data.slice(2)) };
  if (data.startsWith("u:")) {
    const taskId = data.slice(2);
    if (!/^T-\d+$/.test(taskId)) return { text: "Invalid task id" };
    return { text: await ctx.controls.unpark(taskId, `tg${updateId}-unpark`) };
  }
  return { text: "Unknown action" };
}

export function snapshotText(
  primary: string,
  config: Config,
  queue: { running: boolean; pid: number | null; halted: boolean },
) {
  return formatStatus(statusSnapshot(primary, config, primary), queue);
}

export function mainFinishedAt(primary: string): string | null {
  return loadMainState(primary).lastResult?.finishedAt ?? null;
}

export function detailText(primary: string, config: Config, taskId: string): string {
  return taskDetail(primary, config, taskId);
}
