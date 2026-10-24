import type { TelegramUpdate } from "./client.ts";

export interface AccessPolicy {
  ownerId: number;
  chatId: number | null;
}

export type Authorized =
  | {
      ok: true;
      updateId: number;
      userId: number;
      chatId: number;
      kind: "text";
      text: string;
    }
  | {
      ok: true;
      updateId: number;
      userId: number;
      chatId: number;
      kind: "callback";
      data: string;
      callbackId: string;
    }
  | { ok: false; reason: "ignored" | "unauthorized" | "wrong-chat"; chatId?: number };

export function authorize(update: TelegramUpdate, access: AccessPolicy): Authorized {
  const message = update.message;
  const callback = update.callback_query;
  const chat = message?.chat ?? callback?.message?.chat;
  const user = message?.from ?? callback?.from;
  if (!chat || !user || chat.type !== "private") return { ok: false, reason: "ignored" };
  if (user.id !== access.ownerId) return { ok: false, reason: "unauthorized", chatId: chat.id };
  if (access.chatId !== null && chat.id !== access.chatId) {
    return { ok: false, reason: "wrong-chat", chatId: chat.id };
  }
  if (callback) {
    return {
      ok: true,
      updateId: update.update_id,
      userId: user.id,
      chatId: chat.id,
      kind: "callback",
      data: callback.data ?? "",
      callbackId: callback.id,
    };
  }
  return {
    ok: true,
    updateId: update.update_id,
    userId: user.id,
    chatId: chat.id,
    kind: "text",
    text: message?.text ?? "",
  };
}

export function parseCommand(text: string): { name: string; arg: string } | null {
  const match = /^\/([a-z0-9_]+)(?:@[A-Za-z0-9_]+)?(?:\s+([\s\S]*))?$/.exec(text.trim());
  if (!match?.[1]) return null;
  return { name: match[1].toLowerCase(), arg: (match[2] ?? "").trim() };
}
