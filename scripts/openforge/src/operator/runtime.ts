import { mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Config } from "../config.ts";
import { inheritedEnv } from "../config.ts";
import { redactSecrets, secretValues } from "../dotenv.ts";
import { providerCredentials } from "../runtime/credentials.ts";
import { createHost, type FactoryHost } from "../runtime/host.ts";
import { modelRef } from "../runtime/sdk.ts";
import { type OperatorControls, operatorPermissions, operatorPlugin, type TurnContext } from "./plugin.ts";

type TalkHost = FactoryHost & {
  sessions: FactoryHost["sessions"] & {
    context(input: {
      sessionID: string;
    }): Promise<Array<{ type: string; content?: Array<{ type: string; text?: string }> }>>;
  };
};

export function assistantText(
  messages: readonly { type: string; content?: readonly { type: string; text?: string }[] }[],
): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (message?.type !== "assistant") continue;
    const text = (message.content ?? [])
      .filter((part) => part.type === "text" && part.text)
      .map((part) => part.text ?? "")
      .join("\n")
      .trim();
    if (text) return text;
  }
  return "";
}

export interface ConversationResult {
  text: string;
  sessionID: string;
  confirms: TurnContext["confirms"];
}

export interface ConversationalRuntime {
  reply(input: {
    text: string;
    sessionID?: string | null;
    operationPrefix: string;
    signal?: AbortSignal;
  }): Promise<ConversationResult>;
  cancel(): Promise<void>;
  close(): Promise<void>;
}

export class OperatorRuntime implements ConversationalRuntime {
  private activeSession: string | null = null;
  private activeAbort: AbortController | null = null;

  constructor(
    private readonly host: TalkHost,
    private readonly config: Config,
    private readonly primary: string,
    private readonly turn: TurnContext,
  ) {}

  async reply(input: {
    text: string;
    sessionID?: string | null;
    operationPrefix: string;
    signal?: AbortSignal;
  }): Promise<ConversationResult> {
    this.turn.operationPrefix = input.operationPrefix;
    this.turn.confirms = [];
    this.turn.notes = [];
    const sessionID = await this.resolveSession(input.sessionID);
    this.activeSession = sessionID;
    const abort = new AbortController();
    this.activeAbort = abort;
    const timeout = setTimeout(() => abort.abort(), this.config.operator.turnTimeoutSeconds * 1000);
    const onExternal = () => abort.abort();
    input.signal?.addEventListener("abort", onExternal, { once: true });
    try {
      await this.host.sessions.prompt({ sessionID, text: input.text });
      await Promise.race([
        this.host.sessions.wait({ sessionID }),
        new Promise<never>((_, reject) => {
          if (abort.signal.aborted) reject(new Error("aborted"));
          abort.signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        }),
      ]);
      const messages = await this.host.sessions.context({ sessionID });
      const spoken = assistantText(messages);
      const text = redactSecrets(spoken || this.turn.notes.join("\n") || "I have no reply.", secretValues());
      return { text, sessionID, confirms: [...this.turn.confirms] };
    } catch (error) {
      if (abort.signal.aborted) {
        try {
          await this.host.sessions.interrupt({ sessionID });
        } catch {
          // Best-effort interrupt.
        }
        throw new Error("Conversation turn cancelled");
      }
      throw error;
    } finally {
      clearTimeout(timeout);
      input.signal?.removeEventListener("abort", onExternal);
      this.activeAbort = null;
    }
  }

  async cancel(): Promise<void> {
    this.activeAbort?.abort();
    if (this.activeSession) {
      try {
        await this.host.sessions.interrupt({ sessionID: this.activeSession });
      } catch {
        // The session may already be idle.
      }
    }
  }

  async close(): Promise<void> {
    await this.host.close();
  }

  private async resolveSession(sessionID?: string | null): Promise<string> {
    const model = modelRef(this.config.operator.model);
    const permissions = operatorPermissions();
    if (sessionID) {
      try {
        await this.host.sessions.get({ sessionID });
        await this.host.sessions.switchAgent({ sessionID, agent: "orchestrator" });
        await this.host.sessions.switchModel({ sessionID, model });
        await this.host.sessions.update({ sessionID, permissions });
        return sessionID;
      } catch {
        // Create a fresh session if the saved one is gone.
      }
    }
    const created = await this.host.sessions.create({
      agent: "orchestrator",
      model,
      location: { directory: this.primary },
      title: "OpenForge operator",
      permissions,
    });
    await this.host.sessions.environment({
      sessionID: created.id,
      variables: inheritedEnv(this.config),
    });
    return created.id;
  }
}

export async function createOperatorRuntime(
  primary: string,
  config: Config,
  controls: OperatorControls,
): Promise<OperatorRuntime> {
  const home = join(primary, ".work", ".harness", "operator");
  mkdirSync(home, { recursive: true });
  const turn: TurnContext = { operationPrefix: "", confirms: [], notes: [] };
  const providerID = modelRef(config.operator.model).providerID;
  const providers = await providerCredentials(new Set([providerID]));
  const host = (await createHost({
    app: { name: "OpenForge Operator", version: "1" },
    database: { path: join(home, "opencode.db") },
    plugins: [operatorPlugin(controls, turn)],
    config: {
      content: JSON.stringify({
        providers,
        policies: [{ action: "integration.use", resource: "mcp:*", effect: "deny" }],
        snapshots: false,
      }),
    },
  })) as TalkHost;
  return new OperatorRuntime(host, config, primary, turn);
}
