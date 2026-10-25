export * as ConfigToolOutput from "./tool-output.js";
import { Schema } from "effect";
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly max_lines: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly max_bytes: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
