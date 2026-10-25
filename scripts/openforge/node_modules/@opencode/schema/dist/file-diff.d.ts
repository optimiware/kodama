export * as FileDiff from "./file-diff.js";
import { Schema } from "effect";
export declare const Info: Schema.Struct<{
    readonly file: Schema.String;
    readonly patch: Schema.String;
    readonly additions: Schema.Int;
    readonly deletions: Schema.Int;
    readonly status: Schema.Literals<readonly ["added", "deleted", "modified"]>;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
/** V1 snapshot and persisted session diff shape. */
export declare const LegacyInfo: Schema.Struct<{
    readonly file: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly patch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly additions: Schema.Finite;
    readonly deletions: Schema.Finite;
    readonly status: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["added", "deleted", "modified"]>>>, Schema.optionalKey<Schema.Literals<readonly ["added", "deleted", "modified"]>>, never, never>;
}>;
export interface LegacyInfo extends Schema.Schema.Type<typeof LegacyInfo> {
}
