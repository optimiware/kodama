export * as Skill from "./skill.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "Skill.ID">;
export type ID = typeof ID.Type;
export declare const Name: Schema.brand<Schema.String, "Skill.Name">;
export type Name = typeof Name.Type;
export interface DirectorySource extends Schema.Schema.Type<typeof DirectorySource> {
}
export declare const DirectorySource: Schema.Struct<{
    readonly type: Schema.tag<"directory">;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
}>;
export interface UrlSource extends Schema.Schema.Type<typeof UrlSource> {
}
export declare const UrlSource: Schema.Struct<{
    readonly type: Schema.tag<"url">;
    readonly url: Schema.String;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const Info: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Skill.ID">;
    readonly name: Schema.brand<Schema.String, "Skill.Name">;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly autoinvoke: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
    readonly content: Schema.String;
}>;
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"skill.updated">;
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
        type: "skill.updated";
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
        readonly type: Schema.Literal<"skill.updated">;
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
        type: "skill.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    }];
};
export interface EmbeddedSource extends Schema.Schema.Type<typeof EmbeddedSource> {
}
export declare const EmbeddedSource: Schema.Struct<{
    readonly type: Schema.tag<"embedded">;
    readonly skill: Schema.suspend<Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Skill.ID">;
        readonly name: Schema.brand<Schema.String, "Skill.Name">;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly autoinvoke: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        readonly path: Schema.brand<Schema.String, "AbsolutePath">;
        readonly content: Schema.String;
    }>>;
}>;
export type Source = DirectorySource | UrlSource | EmbeddedSource;
export declare const Source: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.tag<"directory">;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
}>, Schema.Struct<{
    readonly type: Schema.tag<"url">;
    readonly url: Schema.String;
}>, Schema.Struct<{
    readonly type: Schema.tag<"embedded">;
    readonly skill: Schema.suspend<Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Skill.ID">;
        readonly name: Schema.brand<Schema.String, "Skill.Name">;
        readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly autoinvoke: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        readonly path: Schema.brand<Schema.String, "AbsolutePath">;
        readonly content: Schema.String;
    }>>;
}>]> & {
    equals: (a: Source, b: Source) => boolean;
    key: (source: Source) => string;
};
