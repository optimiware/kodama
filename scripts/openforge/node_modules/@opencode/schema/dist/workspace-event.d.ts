export * as WorkspaceEvent from "./workspace-event.js";
import { Schema } from "effect";
export declare const ConnectionStatus: Schema.Struct<{
    readonly workspaceID: Schema.brand<Schema.String, "Workspace.ID"> & {
        ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
        create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
    };
    readonly status: Schema.Literals<readonly ["connected", "connecting", "disconnected", "error"]>;
}>;
export interface ConnectionStatus extends Schema.Schema.Type<typeof ConnectionStatus> {
}
export declare const Ready: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"workspace.ready">;
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
        readonly name: Schema.String;
    }>;
}> & {
    type: "workspace.ready";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly name: Schema.String;
    }>;
};
export declare const Failed: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"workspace.failed">;
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
        readonly message: Schema.String;
    }>;
}> & {
    type: "workspace.failed";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly message: Schema.String;
    }>;
};
export declare const Status: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"workspace.status">;
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
        readonly workspaceID: Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        };
        readonly status: Schema.Literals<readonly ["connected", "connecting", "disconnected", "error"]>;
    }>;
}> & {
    type: "workspace.status";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly workspaceID: Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        };
        readonly status: Schema.Literals<readonly ["connected", "connecting", "disconnected", "error"]>;
    }>;
};
export declare const Definitions: readonly [Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"workspace.ready">;
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
        readonly name: Schema.String;
    }>;
}> & {
    type: "workspace.ready";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly name: Schema.String;
    }>;
}, Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"workspace.failed">;
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
        readonly message: Schema.String;
    }>;
}> & {
    type: "workspace.failed";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly message: Schema.String;
    }>;
}, Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"workspace.status">;
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
        readonly workspaceID: Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        };
        readonly status: Schema.Literals<readonly ["connected", "connecting", "disconnected", "error"]>;
    }>;
}> & {
    type: "workspace.status";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly workspaceID: Schema.brand<Schema.String, "Workspace.ID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
            create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
        };
        readonly status: Schema.Literals<readonly ["connected", "connecting", "disconnected", "error"]>;
    }>;
}];
