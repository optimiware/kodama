export * as Vcs from "./vcs.js";
import { Schema } from "effect";
export declare const Branch: Schema.Struct<{
    readonly current: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface Branch extends Schema.Schema.Type<typeof Branch> {
}
export declare const Info: Schema.Struct<{
    readonly provider: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly branch: Schema.Struct<{
        readonly current: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const BranchList: Schema.$Array<Schema.String>;
export type BranchList = typeof BranchList.Type;
export declare const Base: Schema.Struct<{
    readonly name: Schema.String;
    readonly ref: Schema.String;
    readonly source: Schema.Literals<readonly ["reflog", "default"]>;
}>;
export interface Base extends Schema.Schema.Type<typeof Base> {
}
export declare const Mode: Schema.Literals<readonly ["working", "branch", "committed"]>;
export type Mode = typeof Mode.Type;
export declare const FileStatus: Schema.Struct<{
    readonly file: Schema.String;
    readonly additions: Schema.Int;
    readonly deletions: Schema.Int;
    readonly status: Schema.Literals<readonly ["added", "deleted", "modified"]>;
}>;
export interface FileStatus extends Schema.Schema.Type<typeof FileStatus> {
}
