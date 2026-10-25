export * as FileSystem from "./filesystem.js";
import { Schema } from "effect";
export declare const Event: {
    Changed: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"filesystem.changed">;
        readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>>, Schema.optionalKey<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>, never, never>;
        readonly data: Schema.Struct<{
            readonly file: Schema.String;
            readonly event: Schema.Literals<readonly ["add", "change", "unlink"]>;
        }>;
    }> & {
        type: "filesystem.changed";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly file: Schema.String;
            readonly event: Schema.Literals<readonly ["add", "change", "unlink"]>;
        }>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"filesystem.changed">;
        readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>>, Schema.optionalKey<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>, never, never>;
        readonly data: Schema.Struct<{
            readonly file: Schema.String;
            readonly event: Schema.Literals<readonly ["add", "change", "unlink"]>;
        }>;
    }> & {
        type: "filesystem.changed";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly file: Schema.String;
            readonly event: Schema.Literals<readonly ["add", "change", "unlink"]>;
        }>;
    }];
};
export interface Entry extends Schema.Schema.Type<typeof Entry> {
}
export declare const Entry: Schema.Struct<{
    readonly path: Schema.brand<Schema.String, "RelativePath">;
    readonly type: Schema.Literals<readonly ["file", "directory"]>;
}>;
export interface Submatch extends Schema.Schema.Type<typeof Submatch> {
}
export declare const Submatch: Schema.Struct<{
    readonly text: Schema.String;
    readonly start: Schema.Int;
    readonly end: Schema.Int;
}>;
export interface Match extends Schema.Schema.Type<typeof Match> {
}
export declare const Match: Schema.Struct<{
    readonly entry: Schema.Struct<{
        readonly path: Schema.brand<Schema.String, "RelativePath">;
        readonly type: Schema.Literals<readonly ["file", "directory"]>;
    }>;
    readonly line: Schema.Int;
    readonly offset: Schema.Int;
    readonly text: Schema.String;
    readonly submatches: Schema.$Array<Schema.Struct<{
        readonly text: Schema.String;
        readonly start: Schema.Int;
        readonly end: Schema.Int;
    }>>;
}>;
declare const FindInput_base: Schema.Class<FindInput, Schema.Struct<{
    readonly query: Schema.String;
    readonly type: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["file", "directory"]>>>, Schema.optionalKey<Schema.Literals<readonly ["file", "directory"]>>, never, never>;
    readonly limit: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>, {}>;
export declare class FindInput extends FindInput_base {
}
export interface Write extends Schema.Schema.Type<typeof Write> {
}
export declare const Write: Schema.Struct<{
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
}>;
