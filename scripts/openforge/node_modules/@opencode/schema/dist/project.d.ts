export * as Project from "./project.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "Project.ID"> & {
    global: string & import("effect/Brand").Brand<"Project.ID">;
};
export type ID = typeof ID.Type;
export declare const Vcs: Schema.String;
export declare const Current: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Project.ID"> & {
        global: string & import("effect/Brand").Brand<"Project.ID">;
    };
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
}>;
export interface Current extends Schema.Schema.Type<typeof Current> {
}
export declare const Icon: Schema.Struct<{
    readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface Icon extends Schema.Schema.Type<typeof Icon> {
}
export declare const Commands: Schema.Struct<{
    readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface Commands extends Schema.Schema.Type<typeof Commands> {
}
export declare const Time: Schema.Struct<{
    readonly created: Schema.Int;
    readonly updated: Schema.Int;
    readonly active: Schema.Int;
    readonly archived: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
}>;
export interface Time extends Schema.Schema.Type<typeof Time> {
}
export declare const Info: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Project.ID"> & {
        global: string & import("effect/Brand").Brand<"Project.ID">;
    };
    readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
    readonly vcs: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly icon: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
    readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
    readonly time: Schema.Struct<{
        readonly created: Schema.Int;
        readonly updated: Schema.Int;
        readonly active: Schema.Int;
        readonly archived: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    }>;
    readonly sandboxes: Schema.$Array<Schema.String>;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const UpdateInput: Schema.Struct<{
    readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
        global: string & import("effect/Brand").Brand<"Project.ID">;
    };
    readonly canonical: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "AbsolutePath">>>, Schema.optionalKey<Schema.brand<Schema.String, "AbsolutePath">>, never, never>;
    readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly icon: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
    readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
}>;
export interface UpdateInput extends Schema.Schema.Type<typeof UpdateInput> {
}
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"project.updated">;
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
            readonly id: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
            readonly vcs: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly icon: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly time: Schema.Struct<{
                readonly created: Schema.Int;
                readonly updated: Schema.Int;
                readonly active: Schema.Int;
                readonly archived: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
            readonly sandboxes: Schema.$Array<Schema.String>;
        }>;
    }> & {
        type: "project.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
            readonly vcs: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly icon: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly time: Schema.Struct<{
                readonly created: Schema.Int;
                readonly updated: Schema.Int;
                readonly active: Schema.Int;
                readonly archived: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
            readonly sandboxes: Schema.$Array<Schema.String>;
        }>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"project.updated">;
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
            readonly id: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
            readonly vcs: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly icon: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly time: Schema.Struct<{
                readonly created: Schema.Int;
                readonly updated: Schema.Int;
                readonly active: Schema.Int;
                readonly archived: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
            readonly sandboxes: Schema.$Array<Schema.String>;
        }>;
    }> & {
        type: "project.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly canonical: Schema.brand<Schema.String, "AbsolutePath">;
            readonly vcs: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly icon: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly override: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
                readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly start: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly time: Schema.Struct<{
                readonly created: Schema.Int;
                readonly updated: Schema.Int;
                readonly active: Schema.Int;
                readonly archived: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            }>;
            readonly sandboxes: Schema.$Array<Schema.String>;
        }>;
    }];
};
