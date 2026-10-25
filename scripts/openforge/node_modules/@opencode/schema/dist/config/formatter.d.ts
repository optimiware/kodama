export * as ConfigFormatter from "./formatter.js";
import { Schema } from "effect";
declare const Entry_base: Schema.Class<Entry, Schema.Struct<{
    readonly disabled: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly command: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly environment: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.String>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.String>>, never, never>;
    readonly extensions: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
}>, {}>;
export declare class Entry extends Entry_base {
}
export declare const Info: Schema.Union<readonly [Schema.Boolean, Schema.$Record<Schema.String, typeof Entry>]>;
