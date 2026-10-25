import { Effect, Layer } from "effect";
export type Status = {
    readonly status: "completed";
};
export declare const layer: Layer.Layer<never, never, never>;
export declare function status(): Effect.Effect<{
    readonly status: "completed";
}, never, never>;
export declare function run(): Effect.Effect<{
    readonly status: "completed";
}, never, never>;
