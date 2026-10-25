import { type RouteBody, type TriggerCompactOperation } from "../../route/client.js";
/** Collect a trigger response before acknowledging transport completion. No generation output escapes. */
export declare const make: <Body>(body: RouteBody<Body>) => TriggerCompactOperation;
export * as ResponsesCheckpoint from "./responses-checkpoint.js";
