export * as Worktree from "./worktree.js";
import { Schema } from "effect";
export declare const StrategyID: Schema.brand<Schema.Trim, "Worktree.StrategyID">;
export type StrategyID = typeof StrategyID.Type;
export declare const CreateInput: Schema.Struct<{
    readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
        global: string & import("effect/Brand").Brand<"Project.ID">;
    };
    readonly from: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "AbsolutePath">>>, Schema.optionalKey<Schema.brand<Schema.String, "AbsolutePath">>, never, never>;
    readonly branch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Trim>>, Schema.optionalKey<Schema.Trim>, never, never>;
    readonly directory: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "AbsolutePath">>>, Schema.optionalKey<Schema.brand<Schema.String, "AbsolutePath">>, never, never>;
    readonly name: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface CreateInput extends Schema.Schema.Type<typeof CreateInput> {
}
export declare const RemoveInput: Schema.Struct<{
    readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
        global: string & import("effect/Brand").Brand<"Project.ID">;
    };
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly force: Schema.Boolean;
}>;
export interface RemoveInput extends Schema.Schema.Type<typeof RemoveInput> {
}
export declare const Info: Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
}>;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const Directory: Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly strategy: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>;
export interface Directory extends Schema.Schema.Type<typeof Directory> {
}
export declare const ListEntry: Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly type: Schema.Literals<readonly ["root", "worktree"]>;
}>;
export interface ListEntry extends Schema.Schema.Type<typeof ListEntry> {
}
declare const OperationError_base: Schema.Class<OperationError, Schema.TaggedStruct<"Worktree.OperationError", {
    readonly message: Schema.String;
    readonly forceRequired: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>, import("effect/Cause").YieldableError>;
export declare class OperationError extends OperationError_base {
}
export declare const List: Schema.$Array<Schema.Struct<{
    readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    readonly strategy: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
}>>;
export type List = typeof List.Type;
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"worktree.updated">;
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
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
        }>;
    }> & {
        type: "worktree.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
        }>;
    };
    Resolved: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"worktree.resolved">;
        readonly durable: Schema.Struct<{
            readonly aggregateID: Schema.String;
            readonly seq: Schema.brand<Schema.Int, "Event.Seq">;
            readonly version: Schema.decodeTo<Schema.toType<Schema.brand<Schema.Int, "Event.Version">>, Schema.Literal<number>, never, never>;
        }>;
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
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly previous: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly adopted: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>>, Schema.optionalKey<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>, never, never>;
        }>;
    }> & {
        type: "worktree.resolved";
        durability: "durable";
        durable: {
            readonly version: number;
            readonly aggregate: string;
        };
        data: Schema.Struct<{
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly previous: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly adopted: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>>, Schema.optionalKey<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>, never, never>;
        }>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"worktree.updated">;
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
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
        }>;
    }> & {
        type: "worktree.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"worktree.resolved">;
        readonly durable: Schema.Struct<{
            readonly aggregateID: Schema.String;
            readonly seq: Schema.brand<Schema.Int, "Event.Seq">;
            readonly version: Schema.decodeTo<Schema.toType<Schema.brand<Schema.Int, "Event.Version">>, Schema.Literal<number>, never, never>;
        }>;
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
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly previous: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly adopted: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>>, Schema.optionalKey<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>, never, never>;
        }>;
    }> & {
        type: "worktree.resolved";
        durability: "durable";
        durable: {
            readonly version: number;
            readonly aggregate: string;
        };
        data: Schema.Struct<{
            readonly projectID: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly previous: Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            };
            readonly adopted: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>>, Schema.optionalKey<Schema.$Array<Schema.brand<Schema.String, "Project.ID"> & {
                global: string & import("effect/Brand").Brand<"Project.ID">;
            }>>, never, never>;
        }>;
    }];
};
export declare function adopt(session: {
    readonly projectID: string;
    readonly directory: string;
    readonly workspaceID?: string;
}, event: {
    readonly projectID: string;
    readonly directory: string;
    readonly previous: string;
    readonly adopted?: ReadonlyArray<string>;
}): {
    projectID: string;
    subpath: string | undefined;
} | undefined;
