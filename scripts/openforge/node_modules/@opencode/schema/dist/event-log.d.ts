export * as EventLog from "./event-log.js";
import { Schema } from "effect";
/**
 * Replay-to-live boundary marker for a durable log read. The reader now holds
 * every event committed at or below this watermark; `seq` is absent when the
 * captured watermark is empty. Emitted once for the captured watermark.
 */
export declare const Synced: Schema.Struct<{
    readonly type: Schema.Literal<"log.synced">;
    readonly aggregateID: Schema.String;
    readonly seq: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.Int, "Event.Seq">>>, Schema.optionalKey<Schema.brand<Schema.Int, "Event.Seq">>, never, never>;
}>;
export interface Synced extends Schema.Schema.Type<typeof Synced> {
}
