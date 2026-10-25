export * as Reference from "./reference.js";
import { Schema } from "effect";
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"reference.updated">;
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
        type: "reference.updated";
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
        readonly type: Schema.Literal<"reference.updated">;
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
        type: "reference.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    }];
};
export interface LocalSource extends Schema.Schema.Type<typeof LocalSource> {
}
export declare const LocalSource: Schema.Struct<{
    readonly type: Schema.Literal<"local">;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>;
export interface GitSource extends Schema.Schema.Type<typeof GitSource> {
}
export declare const GitSource: Schema.Struct<{
    readonly type: Schema.Literal<"git">;
    readonly repository: Schema.String;
    readonly branch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>;
export declare const Source: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"local">;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"git">;
    readonly repository: Schema.String;
    readonly branch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>]>;
export type Source = typeof Source.Type;
export declare const Info: Schema.Struct<{
    readonly name: Schema.String;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly source: Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"local">;
        readonly path: Schema.brand<Schema.String, "AbsolutePath">;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>, Schema.Struct<{
        readonly type: Schema.Literal<"git">;
        readonly repository: Schema.String;
        readonly branch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    }>]>;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
