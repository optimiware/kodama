export * as DurableEventManifest from "./durable-event-manifest.js";
import { Event } from "./event.js";
export declare const Durable: ReadonlyMap<string, Event.DurableDefinition<string, import("effect/Schema").Codec<unknown, unknown, never, never>>>;
