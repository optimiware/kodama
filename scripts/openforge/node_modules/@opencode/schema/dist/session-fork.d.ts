export * as SessionFork from "./session-fork.js";
import { Schema } from "effect";
export declare const Boundary: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"before">;
    readonly messageID: Schema.brand<Schema.String, "Session.Message.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Session.Message.ID">;
        fromEvent: (eventID: import("./event.js").ID) => string & import("effect/Brand").Brand<"Session.Message.ID">;
    };
}>, Schema.Struct<{
    readonly type: Schema.Literal<"through">;
    readonly messageID: Schema.brand<Schema.String, "Session.Message.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Session.Message.ID">;
        fromEvent: (eventID: import("./event.js").ID) => string & import("effect/Brand").Brand<"Session.Message.ID">;
    };
}>]>;
export type Boundary = typeof Boundary.Type;
