export * as LspEvent from "./lsp-event.js";
export declare const Updated: import("effect/Schema").Struct<{
    readonly id: import("effect/Schema").brand<import("effect/Schema").String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: import("effect/Schema").Finite;
    readonly metadata: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").$Record<import("effect/Schema").String, import("effect/Schema").Unknown>>>, import("effect/Schema").optionalKey<import("effect/Schema").$Record<import("effect/Schema").String, import("effect/Schema").Unknown>>, never, never>;
    readonly type: import("effect/Schema").Literal<"lsp.updated">;
    readonly location: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").Struct<{
        readonly directory: import("effect/Schema").brand<import("effect/Schema").String, "AbsolutePath">;
        readonly workspaceID: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, import("effect/Schema").optionalKey<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, import("effect/Schema").optionalKey<import("effect/Schema").Struct<{
        readonly directory: import("effect/Schema").brand<import("effect/Schema").String, "AbsolutePath">;
        readonly workspaceID: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, import("effect/Schema").optionalKey<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: import("effect/Schema").Struct<{}>;
}> & {
    type: "lsp.updated";
    durability: "ephemeral";
    durable: undefined;
    data: import("effect/Schema").Struct<{}>;
};
export declare const Definitions: readonly [import("effect/Schema").Struct<{
    readonly id: import("effect/Schema").brand<import("effect/Schema").String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: import("effect/Schema").Finite;
    readonly metadata: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").$Record<import("effect/Schema").String, import("effect/Schema").Unknown>>>, import("effect/Schema").optionalKey<import("effect/Schema").$Record<import("effect/Schema").String, import("effect/Schema").Unknown>>, never, never>;
    readonly type: import("effect/Schema").Literal<"lsp.updated">;
    readonly location: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").Struct<{
        readonly directory: import("effect/Schema").brand<import("effect/Schema").String, "AbsolutePath">;
        readonly workspaceID: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, import("effect/Schema").optionalKey<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>>, import("effect/Schema").optionalKey<import("effect/Schema").Struct<{
        readonly directory: import("effect/Schema").brand<import("effect/Schema").String, "AbsolutePath">;
        readonly workspaceID: import("effect/Schema").decodeTo<import("effect/Schema").optional<import("effect/Schema").toType<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>>, import("effect/Schema").optionalKey<import("effect/Schema").brand<import("effect/Schema").String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        }>, never, never>;
    }>>, never, never>;
    readonly data: import("effect/Schema").Struct<{}>;
}> & {
    type: "lsp.updated";
    durability: "ephemeral";
    durable: undefined;
    data: import("effect/Schema").Struct<{}>;
}];
