import type { Config } from "../config.ts";
import { prepareDecisionChange, prepareTaskChange } from "../control/changes.ts";
import { haltQueue, queueLiveness, resumeQueue, retryMainCheck, unparkOnce } from "../control/queue.ts";
import { redactSecrets, secretValues } from "../dotenv.ts";
import { TelegramClient } from "../telegram/client.ts";
import { detailText, snapshotText } from "./actions.ts";
import { formatEvent, readEvents } from "./monitor.ts";
import type { OperatorControls } from "./plugin.ts";
import { createOperatorRuntime } from "./runtime.ts";
import { runOperator } from "./service.ts";

function controls(primary: string, config: Config): OperatorControls {
  return {
    status: () => snapshotText(primary, config, queueLiveness(primary)),
    task: (id) => detailText(primary, config, id),
    events: (limit) => {
      const lines = readEvents(primary, 0)
        .events.slice(-limit)
        .map(formatEvent)
        .filter((line): line is string => Boolean(line));
      return lines.join("\n") || "No notable events.";
    },
    halt: () => haltQueue(primary),
    resume: (workers) => resumeQueue(primary, config, workers),
    unpark: (taskId, operationId) => unparkOnce(primary, taskId, operationId),
    retryMain: (operationId) => retryMainCheck(primary, operationId),
    prepareTask: async (input) => (await prepareTaskChange(primary, config, input)).summary,
    prepareDecision: async (input) => (await prepareDecisionChange(primary, config, input)).summary,
  };
}

export async function runTelegram(primary: string, config: Config, signal: AbortSignal): Promise<void> {
  const token = process.env[config.operator.tokenEnv];
  const ownerRaw = process.env[config.operator.ownerIdEnv];
  if (!token) {
    throw new Error(`Set ${config.operator.tokenEnv} in the environment or the primary checkout .env`);
  }
  if (!ownerRaw || !/^\d+$/.test(ownerRaw)) {
    throw new Error(`Set ${config.operator.ownerIdEnv} to your numeric Telegram user id`);
  }
  const chatRaw = process.env[config.operator.chatIdEnv];
  const chatId = chatRaw && /^-?\d+$/.test(chatRaw) ? Number(chatRaw) : null;
  const bound = controls(primary, config);
  let runtime: Awaited<ReturnType<typeof createOperatorRuntime>> | null = null;
  try {
    runtime = await createOperatorRuntime(primary, config, bound);
  } catch (error) {
    console.error(
      `operator model unavailable: ${redactSecrets(error instanceof Error ? error.message : String(error), secretValues())}`,
    );
  }
  try {
    await runOperator(
      {
        primary,
        config,
        ownerId: Number(ownerRaw),
        chatId,
        transport: new TelegramClient(token),
        runtime,
      },
      signal,
    );
  } finally {
    await runtime?.close();
  }
}
