import { join } from "node:path";
import type { Config } from "../config.ts";
import { prepareDecisionChange, prepareTaskChange } from "../control/changes.ts";
import {
  haltQueue,
  type ProcessLauncher,
  queueLiveness,
  resumeQueue,
  retryMainCheck,
  unparkOnce,
} from "../control/queue.ts";
import { redactSecrets, secretValues } from "../dotenv.ts";
import { loadMainState } from "../state.ts";
import { readJSON, writeJSON } from "../system.ts";
import { type AccessPolicy, authorize, parseCommand } from "../telegram/access.ts";
import {
  type SendRequest,
  splitText,
  TelegramError,
  type TelegramTransport,
  type TelegramUpdate,
} from "../telegram/client.ts";
import { detailText, HELP, handleAuthorized, type OwnerReply, snapshotText } from "./actions.ts";
import { formatEvent, readEvents } from "./monitor.ts";
import type { OperatorControls } from "./plugin.ts";
import type { ConversationalRuntime } from "./runtime.ts";

export interface OperatorState {
  offset: number;
  pending: TelegramUpdate[];
  processed: string[];
  bindingChatId: number | null;
  outbox: SendRequest[];
  eventsByte: number;
  sessionID: string | null;
  lastMainFinishedAt: string | null;
  queueRunning: boolean | null;
}

const statePath = (primary: string) => join(primary, ".work", ".harness", "operator", "state.json");

export function loadOperatorState(primary: string): OperatorState {
  return {
    offset: 0,
    pending: [],
    processed: [],
    bindingChatId: null,
    outbox: [],
    eventsByte: 0,
    sessionID: null,
    lastMainFinishedAt: null,
    queueRunning: null,
    ...readJSON<Partial<OperatorState>>(statePath(primary), {}),
  };
}

function saveOperatorState(primary: string, state: OperatorState): void {
  writeJSON(statePath(primary), state);
}

export interface OperatorService {
  primary: string;
  config: Config;
  ownerId: number;
  chatId: number | null;
  transport: TelegramTransport;
  runtime: ConversationalRuntime | null;
  launcher?: ProcessLauncher;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

function controlsFor(service: OperatorService): OperatorControls {
  return {
    status: () => snapshotText(service.primary, service.config, queueLiveness(service.primary)),
    task: (id) => detailText(service.primary, service.config, id),
    events: (limit) => {
      const { events } = readEvents(service.primary, 0);
      const lines = events
        .slice(-limit)
        .map(formatEvent)
        .filter((line): line is string => Boolean(line));
      return lines.join("\n") || "No notable events.";
    },
    halt: () => haltQueue(service.primary),
    resume: (workers) => resumeQueue(service.primary, service.config, workers, service.launcher),
    unpark: (taskId, operationId) => unparkOnce(service.primary, taskId, operationId),
    retryMain: (operationId) => retryMainCheck(service.primary, operationId, service.launcher),
    prepareTask: async (input) => (await prepareTaskChange(service.primary, service.config, input)).summary,
    prepareDecision: async (input) =>
      (await prepareDecisionChange(service.primary, service.config, input)).summary,
  };
}

function accessFor(service: OperatorService, state: OperatorState): AccessPolicy {
  return { ownerId: service.ownerId, chatId: service.chatId ?? state.bindingChatId };
}

function enqueue(state: OperatorState, request: SendRequest): void {
  const parts = splitText(redactSecrets(request.text, secretValues()));
  parts.forEach((part, index) => {
    state.outbox.push({
      chatId: request.chatId,
      text: part,
      buttons: index === parts.length - 1 ? request.buttons : undefined,
      callbackId: index === 0 ? request.callbackId : undefined,
    });
  });
}

function remember(state: OperatorState, updateId: number): void {
  const id = String(updateId);
  if (!state.processed.includes(id)) state.processed.push(id);
  state.processed = state.processed.slice(-500);
  state.pending = state.pending.filter((update) => update.update_id !== updateId);
}

async function pollControlsDuringTurn(
  service: OperatorService,
  state: OperatorState,
  turnSignal: AbortSignal,
): Promise<boolean> {
  const sleep = service.sleep ?? defaultSleep;
  let cancelledByOwner = false;
  while (!turnSignal.aborted) {
    let updates: TelegramUpdate[] = [];
    try {
      updates = await service.transport.getUpdates(state.offset, 1, turnSignal);
    } catch {
      if (turnSignal.aborted) break;
      await sleep(100, turnSignal);
      continue;
    }
    if (updates.length === 0) {
      await sleep(50, turnSignal);
      continue;
    }
    state.pending.push(...updates);
    state.offset = Math.max(state.offset, ...updates.map((update) => update.update_id + 1));
    for (const incoming of updates) {
      if (state.processed.includes(String(incoming.update_id))) {
        remember(state, incoming.update_id);
        continue;
      }
      const auth = authorize(incoming, accessFor(service, state));
      if (!auth.ok) {
        if (auth.reason === "unauthorized" && auth.chatId) {
          enqueue(state, { chatId: auth.chatId, text: "Unauthorized." });
        }
        remember(state, incoming.update_id);
        continue;
      }
      const cmd = auth.kind === "text" ? parseCommand(auth.text)?.name : null;
      const isCancel = cmd === "cancel";
      const isHalt = cmd === "halt" || (auth.kind === "callback" && auth.data === "h");
      if (!isCancel && !isHalt) continue;
      if (isCancel) {
        cancelledByOwner = true;
        await service.runtime?.cancel();
        enqueue(state, { chatId: auth.chatId, text: "Cancelled the current conversation turn." });
      } else {
        enqueue(state, {
          chatId: auth.chatId,
          text: await haltQueue(service.primary),
          callbackId: auth.kind === "callback" ? auth.callbackId : undefined,
        });
      }
      remember(state, incoming.update_id);
    }
    saveOperatorState(service.primary, state);
    await flush(service, state);
    if (cancelledByOwner) break;
  }
  return cancelledByOwner;
}

export async function drainOnce(service: OperatorService, signal?: AbortSignal): Promise<void> {
  const state = loadOperatorState(service.primary);
  await flush(service, state, signal);
  if (state.pending.length === 0 && !signal?.aborted) {
    try {
      const updates = await service.transport.getUpdates(
        state.offset,
        service.config.operator.pollTimeoutSeconds,
        signal,
      );
      if (updates.length > 0) {
        state.pending.push(...updates);
        state.offset = Math.max(state.offset, ...updates.map((update) => update.update_id + 1));
        saveOperatorState(service.primary, state);
      }
    } catch (error) {
      if (signal?.aborted) return;
      throw error;
    }
  }
  const controls = controlsFor(service);
  while (state.pending.length > 0 && !signal?.aborted) {
    const update = state.pending[0] as TelegramUpdate;
    if (state.processed.includes(String(update.update_id))) {
      remember(state, update.update_id);
      saveOperatorState(service.primary, state);
      continue;
    }
    const authorized = authorize(update, accessFor(service, state));
    if (!authorized.ok) {
      if (authorized.reason === "unauthorized" && authorized.chatId) {
        enqueue(state, { chatId: authorized.chatId, text: "Unauthorized." });
      }
      remember(state, update.update_id);
      saveOperatorState(service.primary, state);
      await flush(service, state, signal);
      continue;
    }
    if (service.chatId === null && state.bindingChatId === null) {
      state.bindingChatId = authorized.chatId;
    }
    const isConversation =
      authorized.kind === "text" && parseCommand(authorized.text) === null && service.runtime !== null;
    const turnPoll = new AbortController();
    const pollSignal = signal ? AbortSignal.any([signal, turnPoll.signal]) : turnPoll.signal;
    const controlLoop = isConversation ? pollControlsDuringTurn(service, state, pollSignal) : null;
    let reply: OwnerReply | null = null;
    let turnError: unknown;
    try {
      reply = await handleAuthorized(authorized, {
        primary: service.primary,
        config: service.config,
        controls,
        runtime: service.runtime,
        sessionID: state.sessionID,
        signal,
      });
    } catch (error) {
      turnError = error;
    } finally {
      turnPoll.abort();
    }
    const cancelledByOwner = controlLoop ? await controlLoop : false;
    if (turnError && !cancelledByOwner) {
      reply = {
        text: redactSecrets(
          turnError instanceof Error ? turnError.message : String(turnError),
          secretValues(),
        ),
        callbackId: authorized.kind === "callback" ? authorized.callbackId : undefined,
      };
    }
    if (reply) {
      if (reply.sessionID) state.sessionID = reply.sessionID;
      enqueue(state, {
        chatId: authorized.chatId,
        text: reply.text,
        buttons: reply.buttons,
        callbackId: reply.callbackId,
      });
    }
    remember(state, update.update_id);
    saveOperatorState(service.primary, state);
    await flush(service, state, signal);
  }
  notifyFromJournal(service, state);
  saveOperatorState(service.primary, state);
  await flush(service, state, signal);
}

function notifyFromJournal(service: OperatorService, state: OperatorState): void {
  const chatId = service.chatId ?? state.bindingChatId;
  if (chatId === null) return;
  const { events, next } = readEvents(service.primary, state.eventsByte);
  state.eventsByte = next;
  const lines = events.map(formatEvent).filter((line): line is string => Boolean(line));
  if (lines.length > 0) {
    const buttons = lines.some((line) => line.startsWith("parked "))
      ? [{ label: "Status", data: "s" }]
      : undefined;
    enqueue(state, { chatId, text: lines.join("\n"), buttons });
  }
  const main = loadMainState(service.primary);
  const finished = main.lastResult?.finishedAt ?? null;
  if (state.lastMainFinishedAt && finished && finished !== state.lastMainFinishedAt) {
    enqueue(state, {
      chatId,
      text: `Main check ${main.lastResult?.ok ? "passed" : "failed"}: ${main.lastResult?.message ?? ""}`,
    });
  }
  state.lastMainFinishedAt = finished;
  const running = queueLiveness(service.primary).running;
  if (state.queueRunning !== null && state.queueRunning !== running) {
    enqueue(state, { chatId, text: running ? "Queue is running." : "Queue stopped." });
  }
  state.queueRunning = running;
}

async function flush(service: OperatorService, state: OperatorState, signal?: AbortSignal): Promise<void> {
  const remaining: SendRequest[] = [];
  for (let index = 0; index < state.outbox.length; index++) {
    const request = state.outbox[index] as SendRequest;
    try {
      await service.transport.send(request, signal);
    } catch (error) {
      if (!signal?.aborted) {
        console.error(
          `operator delivery: ${redactSecrets(error instanceof Error ? error.message : String(error), secretValues())}`,
        );
      }
      if (!signal?.aborted && error instanceof TelegramError && !error.retryable) {
        continue;
      }
      remaining.push({ ...request, callbackId: undefined }, ...state.outbox.slice(index + 1));
      break;
    }
  }
  state.outbox = remaining;
  saveOperatorState(service.primary, state);
}

export async function runOperator(service: OperatorService, signal: AbortSignal): Promise<void> {
  const sleep = service.sleep ?? defaultSleep;
  const state = loadOperatorState(service.primary);
  if (state.queueRunning === null) {
    const { next } = readEvents(service.primary, 0);
    state.eventsByte = next;
    state.queueRunning = queueLiveness(service.primary).running;
    state.lastMainFinishedAt = loadMainState(service.primary).lastResult?.finishedAt ?? null;
    const chatId = service.chatId ?? state.bindingChatId;
    if (chatId !== null) enqueue(state, { chatId, text: `OpenForge operator is online.\n\n${HELP}` });
    saveOperatorState(service.primary, state);
  }
  while (!signal.aborted) {
    try {
      await drainOnce(service, signal);
    } catch (error) {
      if (signal.aborted) return;
      const message = redactSecrets(error instanceof Error ? error.message : String(error), secretValues());
      console.error(`operator: ${message}`);
      await sleep(5_000, signal);
    }
  }
}

function defaultSleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (signal?.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}
