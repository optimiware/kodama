export * as ConfigCompaction from "./compaction.js";
import { Schema } from "effect";
declare const Keep_base: Schema.Class<Keep, Schema.Struct<{
    readonly tokens: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>, {}>;
export declare class Keep extends Keep_base {
}
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly auto: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly keep: Schema.decodeTo<Schema.optional<Schema.toType<typeof Keep>>, Schema.optionalKey<typeof Keep>, never, never>;
    readonly buffer: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
