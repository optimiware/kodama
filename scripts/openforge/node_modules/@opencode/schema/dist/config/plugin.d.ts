export * as ConfigPlugin from "./plugin.js";
import { Schema } from "effect";
declare const Entry_base: Schema.Class<Entry, Schema.Struct<{
    readonly package: Schema.String;
    readonly options: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
}>, {}>;
export declare class Entry extends Entry_base {
}
export declare const Plugin: Schema.Union<readonly [Schema.String, typeof Entry]>;
export type Plugin = typeof Plugin.Type;
export declare const Plugins: Schema.$Array<Schema.Union<readonly [Schema.String, typeof Entry]>>;
