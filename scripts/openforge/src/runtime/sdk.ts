import { existsSync, mkdirSync } from "node:fs";
import { join, resolve } from "node:path";
import type { Config, Role } from "../config.ts";
import { REPORT_TOOLS } from "../prompts.ts";
import { Paused } from "../system.ts";
import { providerCredentials } from "./credentials.ts";
import { createHost, type FactoryHost, type RuntimeHost } from "./host.ts";
import { type ActiveInvocation, type RoleResult, reportingPlugin } from "./plugin.ts";

export class AgentError extends Error {
  constructor(
    message: string,
    readonly retryable = false,
  ) {
    super(message);
    this.name = "AgentError";
  }
}

export interface AgentRequest {
  task: string;
  role: Role;
  prompt: string;
  worktree: string;
  owns: readonly string[];
  protectedPaths?: readonly string[];
  sessionID?: string | null;
  environment?: Record<string, string>;
  timeoutSeconds: number;
  signal?: AbortSignal;
  onSession?: (sessionID: string) => void;
}

export interface AgentRunOutput<T = RoleResult> {
  result: T;
  sessionID: string;
}

export interface AgentRuntime {
  run(request: AgentRequest): Promise<AgentRunOutput>;
  close(): Promise<void>;
}

type Permissions = NonNullable<NonNullable<Parameters<RuntimeHost["sessions"]["create"]>[0]>["permissions"]>;

export function sessionPermissions(request: AgentRequest): Permissions {
  const rules: Array<Permissions[number]> = [{ action: "*", resource: "*", effect: "deny" }];
  for (const action of ["read", "glob", "grep", REPORT_TOOLS[request.role]]) {
    rules.push({ action, resource: "*", effect: "allow" });
  }
  if (request.role === "reviewer") {
    for (const command of [
      "git diff *",
      "git log *",
      "git show *",
      "git status *",
      "git ls-files *",
      "ls *",
    ]) {
      rules.push({ action: "shell", resource: command, effect: "allow" });
    }
  }
  if (request.role === "implementer" || request.role === "fixer") {
    rules.push({ action: "shell", resource: "*", effect: "allow" });
    for (const path of [...request.owns, ...(request.protectedPaths ?? []), ".harness/"]) {
      rules.push({ action: "edit", resource: path.endsWith("/") ? `${path}*` : path, effect: "allow" });
    }
    for (const command of [
      "git push*",
      "git remote*",
      "git worktree*",
      "git checkout *",
      "git switch*",
      "git rebase*",
      "git merge*",
      "git branch -D*",
      "git branch -d*",
      "git -C *",
      "git -c *",
      "git --*",
      "git update-ref*",
      "git symbolic-ref*",
      "git config*",
      "git reflog*",
      "git gc*",
      "git prune*",
      "git fetch*",
      "git pull*",
      "git clone*",
      "git submodule*",
      "git replace*",
      "git filter*",
      "rm -rf*",
      "sudo*",
      "pkill*",
      "killall*",
    ]) {
      rules.push({ action: "shell", resource: command, effect: "deny" });
    }
  }
  return rules;
}

export function modelRef(reference: string) {
  const slash = reference.indexOf("/");
  if (slash < 1) throw new Error(`Invalid model reference: ${reference}`);
  const [id, variant] = reference.slice(slash + 1).split("#");
  if (!id) throw new Error(`Invalid model reference: ${reference}`);
  return { providerID: reference.slice(0, slash), id, ...(variant ? { variant } : {}) };
}

export class SDKRuntime<H extends RuntimeHost = RuntimeHost> implements AgentRuntime {
  constructor(
    readonly host: H,
    readonly active: Map<string, ActiveInvocation>,
    readonly models: Record<Role, string>,
  ) {}

  private async resolveSession(request: AgentRequest): Promise<string> {
    const permissions = sessionPermissions(request);
    const model = modelRef(this.models[request.role]);
    const directory = resolve(request.worktree);
    if (request.sessionID) {
      try {
        await this.host.sessions.get({ sessionID: request.sessionID });
        await this.host.sessions.switchAgent({ sessionID: request.sessionID, agent: request.role });
        await this.host.sessions.switchModel({ sessionID: request.sessionID, model });
        await this.host.sessions.update({ sessionID: request.sessionID, permissions });
        return request.sessionID;
      } catch {
        // Fall through and create a fresh session if the previous one is absent.
      }
    }
    const created = await this.host.sessions.create({
      agent: request.role,
      model,
      location: { directory },
      title: `${request.task} ${request.role}`,
      permissions,
    });
    return created.id;
  }

  async run(request: AgentRequest): Promise<AgentRunOutput> {
    const haltFile = request.environment?.FORGE_HALT_FILE;
    const isHalted = () => Boolean(request.signal?.aborted || (haltFile && existsSync(haltFile)));
    if (isHalted()) throw new Paused("Queue halted");

    const sessionID = await this.resolveSession(request);
    request.onSession?.(sessionID);
    const invocation: ActiveInvocation = {
      role: request.role,
      worktree: resolve(request.worktree),
    };
    this.active.set(sessionID, invocation);

    let timedOut = false;
    const controller = new AbortController();
    const timer =
      request.timeoutSeconds > 0
        ? setTimeout(() => {
            timedOut = true;
            controller.abort();
          }, request.timeoutSeconds * 1000)
        : null;
    const poll = setInterval(() => {
      if (isHalted()) controller.abort();
    }, 100);
    const onExternalAbort = () => controller.abort();
    request.signal?.addEventListener("abort", onExternalAbort, { once: true });

    try {
      if (request.environment) {
        await this.host.sessions.environment({
          sessionID,
          variables: request.environment,
        });
      }
      await this.host.sessions.prompt({
        sessionID,
        text: request.prompt,
      });

      await Promise.race([
        this.host.sessions.wait({ sessionID }),
        new Promise<never>((_, reject) => {
          if (controller.signal.aborted) {
            reject(new Error("aborted"));
            return;
          }
          controller.signal.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        }),
      ]);

      if (invocation.result === undefined) {
        throw new AgentError(`${request.role} finished without calling ${REPORT_TOOLS[request.role]}`, true);
      }
      return { result: invocation.result, sessionID };
    } catch (error) {
      if (controller.signal.aborted || isHalted() || timedOut) {
        try {
          await this.host.sessions.interrupt({ sessionID });
        } catch {
          // Best-effort interrupt.
        }
      }
      if (isHalted()) throw new Paused("Queue halted");
      if (timedOut) {
        throw new AgentError(`${request.role} timed out after ${request.timeoutSeconds}s`, true);
      }
      if (error instanceof AgentError || error instanceof Paused) throw error;
      throw new AgentError(error instanceof Error ? error.message : String(error), true);
    } finally {
      if (timer) clearTimeout(timer);
      clearInterval(poll);
      request.signal?.removeEventListener("abort", onExternalAbort);
      this.active.delete(sessionID);
    }
  }

  async close(): Promise<void> {
    await this.host.close();
  }
}

export async function createRuntime(
  primary: string,
  config: Config,
  options: { databasePath?: string } = {},
): Promise<SDKRuntime<FactoryHost>> {
  if (config.sdkVersion !== "2.1.2") {
    throw new Error(`Configured sdkVersion ${config.sdkVersion} differs from installed 2.1.2`);
  }
  const home = join(primary, ".work", ".harness");
  mkdirSync(home, { recursive: true });
  const active = new Map<string, ActiveInvocation>();
  const providers = await providerCredentials(
    new Set(Object.values(config.roles).map((role) => modelRef(role.model).providerID)),
  );
  const host = await createHost({
    app: { name: "OpenForge", version: "1" },
    database: { path: options.databasePath ?? join(home, "opencode.db") },
    plugins: [reportingPlugin(active)],
    config: {
      content: JSON.stringify({
        providers,
        policies: [{ action: "integration.use", resource: "mcp:*", effect: "deny" }],
        snapshots: false,
      }),
    },
  });
  return new SDKRuntime(host, active, {
    planner: config.roles.planner.model,
    implementer: config.roles.implementer.model,
    fixer: config.roles.fixer.model,
    reviewer: config.roles.reviewer.model,
  });
}
