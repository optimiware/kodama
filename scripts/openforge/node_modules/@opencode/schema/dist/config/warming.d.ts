export * as ConfigWarming from "./warming.js";
import { Schema } from "effect";
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly prompt: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly interval: Schema.decodeTo<Schema.optional<Schema.toType<Schema.DurationFromString>>, Schema.optionalKey<Schema.DurationFromString>, never, never>;
    readonly duration: Schema.decodeTo<Schema.optional<Schema.toType<Schema.DurationFromString>>, Schema.optionalKey<Schema.DurationFromString>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
export declare const Warming: Schema.Union<readonly [Schema.Boolean, typeof Info]>;
