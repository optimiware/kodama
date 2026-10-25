export * as Location from "./location.js";
import { Schema } from "effect";
export interface Ref extends Schema.Schema.Type<typeof Ref> {
}
export declare const Ref: Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
        ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
        create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
    }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
        ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
        create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
    }>, never, never>;
}>;
export declare const PublicRef: Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
}>;
export interface PublicRef extends Schema.Schema.Type<typeof PublicRef> {
}
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
        ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
        create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
    }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
        ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
        create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
    }>, never, never>;
    readonly project: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Project.ID"> & {
            global: string & import("effect/Brand").Brand<"Project.ID">;
        };
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
}>, {}>;
export declare class Info extends Info_base {
}
export declare const PublicInfo: Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly project: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Project.ID"> & {
            global: string & import("effect/Brand").Brand<"Project.ID">;
        };
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
        readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
}>;
export interface PublicInfo extends Schema.Schema.Type<typeof PublicInfo> {
}
export declare function response<S extends Schema.Top>(data: S): Schema.Struct<{
    readonly location: Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
    readonly data: S;
}>;
