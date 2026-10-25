export * as ConfigReference from "./reference.js";
import { Schema } from "effect";
declare const Git_base: Schema.Class<Git, Schema.Struct<{
    readonly repository: Schema.String;
    readonly branch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, {}>;
export declare class Git extends Git_base {
}
declare const Local_base: Schema.Class<Local, Schema.Struct<{
    readonly path: Schema.String;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, {}>;
export declare class Local extends Local_base {
}
export declare const Entry: Schema.Union<readonly [Schema.String, typeof Git, typeof Local]>;
export type Entry = typeof Entry.Type;
export declare const Info: Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, typeof Git, typeof Local]>>;
export type Info = typeof Info.Type;
