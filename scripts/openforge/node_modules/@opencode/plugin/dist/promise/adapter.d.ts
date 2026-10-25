import type { Scope } from "effect";
import type { Plugin } from "./plugin.js";
/**
 * Adapts a Promise plugin into an Effect plugin so the existing Effect-only
 * Core plugin runtime can run it unchanged.
 *
 * Hook registrations created during the async `setup` attach to the plugin's
 * scope, so unloading the plugin disposes them. The captured fiber context
 * preserves boot-time batching, so Promise-plugin transforms still coalesce
 * into one reload per domain.
 */
export declare function fromPromise(plugin: Plugin): import("../effect/plugin.js").Plugin<Scope.Scope>;
