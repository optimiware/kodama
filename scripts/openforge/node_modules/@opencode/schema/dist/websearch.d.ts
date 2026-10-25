export * as WebSearch from "./websearch.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "WebSearch.ID">;
export type ID = typeof ID.Type;
export interface Provider extends Schema.Schema.Type<typeof Provider> {
}
export declare const Provider: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "WebSearch.ID">;
    readonly name: Schema.String;
}>;
export interface Input extends Schema.Schema.Type<typeof Input> {
}
export declare const Input: Schema.Struct<{
    readonly query: Schema.String;
    readonly providerID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "WebSearch.ID">>>, Schema.optionalKey<Schema.brand<Schema.String, "WebSearch.ID">>, never, never>;
}>;
export type ProviderInput = Pick<Input, "query">;
export interface Result extends Schema.Schema.Type<typeof Result> {
}
export declare const Result: Schema.Struct<{
    readonly url: Schema.String;
    readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly content: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly time: Schema.Struct<{
        readonly published: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    }>;
}>;
declare const Response_base: Schema.Class<Response, Schema.Struct<{
    readonly providerID: Schema.brand<Schema.String, "WebSearch.ID">;
    readonly results: Schema.$Array<Schema.Struct<{
        readonly url: Schema.String;
        readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly content: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly time: Schema.Struct<{
            readonly published: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
        }>;
    }>>;
}>, {}>;
export declare class Response extends Response_base {
}
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"websearch.updated">;
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
        type: "websearch.updated";
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
        readonly type: Schema.Literal<"websearch.updated">;
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
        type: "websearch.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    }];
};
