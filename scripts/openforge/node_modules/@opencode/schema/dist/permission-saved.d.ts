export * as PermissionSaved from "./permission-saved.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "PermissionSaved.ID"> & {
    create: () => string & import("effect/Brand").Brand<"PermissionSaved.ID">;
};
export type ID = typeof ID.Type;
export declare const Info: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "PermissionSaved.ID"> & {
        create: () => string & import("effect/Brand").Brand<"PermissionSaved.ID">;
    };
    readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
        global: string & import("effect/Brand").Brand<"Project.ID">;
    };
    readonly action: Schema.String;
    readonly resource: Schema.String;
    readonly time: Schema.Struct<{
        readonly created: Schema.decodeTo<Schema.DateTimeUtc, Schema.Finite, never, never>;
        readonly updated: Schema.decodeTo<Schema.DateTimeUtc, Schema.Finite, never, never>;
    }>;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
