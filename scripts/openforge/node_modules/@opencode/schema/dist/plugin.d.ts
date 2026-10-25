export * as Plugin from "./plugin.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "Plugin.ID">;
export type ID = typeof ID.Type;
export declare const Source: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"builtin">;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"package">;
    readonly target: Schema.String;
    readonly version: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly outdated: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
    readonly updating: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"local">;
    readonly path: Schema.String;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"sdk">;
}>]>;
export type Source = typeof Source.Type;
export declare const Features: Schema.Struct<{
    readonly server: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
    readonly tui: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
    readonly rpc: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
}>;
export type Features = typeof Features.Type;
export declare const State: Schema.Union<readonly [Schema.Struct<{
    readonly status: Schema.Literal<"active">;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"failed">;
    readonly error: Schema.String;
    readonly ref: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>]>;
export type State = typeof State.Type;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const Info: Schema.Struct<{
    readonly id: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Plugin.ID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Plugin.ID">>, never, never>;
    readonly source: Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"builtin">;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"package">;
        readonly target: Schema.String;
        readonly version: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly outdated: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
        readonly updating: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"local">;
        readonly path: Schema.String;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"sdk">;
    }>]>;
    readonly features: Schema.Struct<{
        readonly server: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
        readonly tui: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
        readonly rpc: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literal<true>>>, Schema.optionalKey<Schema.Literal<true>>, never, never>;
    }>;
    readonly state: Schema.Union<readonly [Schema.Struct<{
        readonly status: Schema.Literal<"active">;
    }>, Schema.Struct<{
        readonly status: Schema.Literal<"failed">;
        readonly error: Schema.String;
        readonly ref: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>]>;
}>;
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"plugin.updated">;
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
        readonly data: Schema.Struct<{}>;
    }> & {
        type: "plugin.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"plugin.updated">;
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
        readonly data: Schema.Struct<{}>;
    }> & {
        type: "plugin.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    }];
};
