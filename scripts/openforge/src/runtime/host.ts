import { mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { OpenCode } from "@opencode/client";
import { SdkPlugins } from "@opencode/core/plugin/sdk";
import { Session } from "@opencode/core/session";
import { SessionRestart } from "@opencode/core/session/execution/restart";
import { fromPromise } from "@opencode/plugin/promise/adapter";
import type { OpenCode as SDK } from "@opencode/sdk";
import { createEmbeddedRoutes } from "@opencode/server/routes";
import { Context, Effect, Exit, Layer, Scope } from "effect";
import { HttpEffect, HttpRouter, HttpServer } from "effect/http";
import { FileLock } from "../system.ts";

export type RuntimeHost = Pick<SDK.Interface, "close"> & {
  sessions: Pick<
    SDK.Interface["sessions"],
    | "create"
    | "get"
    | "update"
    | "switchAgent"
    | "switchModel"
    | "environment"
    | "prompt"
    | "wait"
    | "interrupt"
  >;
};

export type FactoryHost = RuntimeHost & {
  server: Pick<SDK.Interface["server"], "info">;
  resumeSelected(sessionID: string): Promise<void>;
  [Symbol.asyncDispose](): Promise<void>;
};

export async function createHost(options: SDK.CreateOptions): Promise<FactoryHost> {
  const database = options.database?.path;
  if (!database || database === ":memory:") {
    throw new Error("Factory host requires an explicitly owned database");
  }
  mkdirSync(dirname(database), { recursive: true });
  const lock = FileLock.tryAcquire(`${database}.owner.lock`);
  if (!lock) throw new Error("Another OpenForge host owns this execution database");
  const scope = Effect.runSync(Scope.make());
  const close = () => Effect.runPromise(Scope.close(scope, Exit.void)).finally(() => lock.release());
  try {
    if (options.instances) throw new Error("Factory host does not accept externally managed SDK instances");
    const context = await Effect.runPromise(
      Effect.provideService(
        Layer.build(
          createEmbeddedRoutes(
            {
              app: { name: options.app?.name ?? "OpenForge", version: options.app?.version ?? "1" },
              database: { path: database },
              config: options.config,
              fs: options.fs,
              models: options.models,
            },
            [
              SessionRestart.node.replace(
                Layer.succeed(SessionRestart.Service, { resumeSuspendedSessions: Effect.void }),
              ),
            ],
          ).pipe(Layer.provide(HttpServer.layerServices)),
        ),
        Scope.Scope,
        scope,
      ),
    );
    const plugins = Context.get(context, SdkPlugins.Service);
    for (const plugin of options.plugins ?? []) {
      await Effect.runPromise(
        Effect.provideService(plugins.register(fromPromise(plugin)), Scope.Scope, scope),
      );
    }
    const handler = (
      HttpEffect.toWebHandlerWith(context) as unknown as (
        self: Effect.Effect<unknown, unknown, unknown>,
      ) => (request: Request) => Promise<Response>
    )(Context.get(context, HttpRouter.HttpRouter).asHttpEffect());
    const client = OpenCode.make({
      baseUrl: "http://opencode.local",
      fetch: Object.assign(
        (input: RequestInfo | URL, init?: RequestInit) => handler(new Request(input, init)),
        { preconnect: () => undefined },
      ),
    });
    const sessions = Context.get(context, Session.Service);
    return {
      ...client,
      sessions: client.session,
      close,
      [Symbol.asyncDispose]: close,
      resumeSelected: (sessionID) => Effect.runPromise(sessions.resume(Session.ID.make(sessionID))),
    };
  } catch (error) {
    await close();
    throw error;
  }
}
