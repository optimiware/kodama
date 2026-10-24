export interface TelegramUpdate {
  update_id: number;
  message?: {
    message_id: number;
    text?: string;
    chat: { id: number; type: string };
    from?: { id: number; is_bot?: boolean };
  };
  callback_query?: {
    id: string;
    data?: string;
    from: { id: number };
    message?: { message_id: number; chat: { id: number; type: string } };
  };
}

export interface SendRequest {
  chatId: number;
  text: string;
  buttons?: Array<{ label: string; data: string }>;
  callbackId?: string;
}

export interface TelegramTransport {
  getUpdates(offset: number, timeoutSeconds: number, signal?: AbortSignal): Promise<TelegramUpdate[]>;
  send(request: SendRequest, signal?: AbortSignal): Promise<{ messageId: number }>;
}

interface TelegramResponse {
  ok: boolean;
  result?: unknown;
  description?: string;
  error_code?: number;
  parameters?: { retry_after?: number };
}

export class TelegramError extends Error {
  constructor(
    message: string,
    readonly retryable: boolean,
    readonly errorCode?: number,
  ) {
    super(message);
    this.name = "TelegramError";
  }
}

export class TelegramClient implements TelegramTransport {
  constructor(
    private readonly token: string,
    private readonly fetchImpl: typeof fetch = fetch,
  ) {}

  redact(text: string): string {
    return text.split(this.token).join("<redacted>");
  }

  private async call(method: string, body: Record<string, unknown>, signal?: AbortSignal): Promise<unknown> {
    let attempt = 0;
    for (;;) {
      let response: Response;
      try {
        response = await this.fetchImpl(`https://api.telegram.org/bot${this.token}/${method}`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify(body),
          signal,
        });
      } catch (error) {
        if (signal?.aborted) throw error;
        throw new TelegramError(this.redact(error instanceof Error ? error.message : String(error)), true);
      }
      const json = (await response.json()) as TelegramResponse;
      if (json.ok) return json.result;
      if (json.error_code === 429 && attempt < 3) {
        attempt++;
        const wait = (json.parameters?.retry_after ?? 1) * 1000;
        await new Promise((resolve, reject) => {
          const timer = setTimeout(resolve, wait);
          signal?.addEventListener(
            "abort",
            () => {
              clearTimeout(timer);
              reject(new Error("aborted"));
            },
            { once: true },
          );
        });
        continue;
      }
      const code = json.error_code ?? response.status;
      const retryable = code === 429 || code >= 500;
      throw new TelegramError(
        this.redact(`Telegram ${method} failed (${code}): ${json.description ?? "unknown"}`),
        retryable,
        code,
      );
    }
  }

  async getUpdates(offset: number, timeoutSeconds: number, signal?: AbortSignal): Promise<TelegramUpdate[]> {
    const result = await this.call(
      "getUpdates",
      {
        offset,
        timeout: timeoutSeconds,
        allowed_updates: ["message", "callback_query"],
      },
      signal,
    );
    return Array.isArray(result) ? (result as TelegramUpdate[]) : [];
  }

  async send(request: SendRequest, signal?: AbortSignal): Promise<{ messageId: number }> {
    if (request.callbackId) {
      try {
        await this.call("answerCallbackQuery", { callback_query_id: request.callbackId }, signal);
      } catch (error) {
        if (signal?.aborted) throw error;
      }
    }
    const result = (await this.call(
      "sendMessage",
      {
        chat_id: request.chatId,
        text: request.text,
        ...(request.buttons && request.buttons.length > 0
          ? {
              reply_markup: {
                inline_keyboard: [
                  request.buttons.map((button) => ({ text: button.label, callback_data: button.data })),
                ],
              },
            }
          : {}),
      },
      signal,
    )) as { message_id: number };
    return { messageId: result.message_id };
  }
}

export function splitText(text: string, limit = 4000): string[] {
  const clean = text.trim() || "(empty)";
  if (clean.length <= limit) return [clean];
  const parts: string[] = [];
  let rest = clean;
  while (rest.length > limit) {
    const window = rest.slice(0, limit);
    const breakAt = window.lastIndexOf("\n");
    const cut = breakAt > limit / 2 ? breakAt : limit;
    parts.push(rest.slice(0, cut));
    rest = rest.slice(cut).trimStart();
  }
  if (rest) parts.push(rest);
  return parts;
}
