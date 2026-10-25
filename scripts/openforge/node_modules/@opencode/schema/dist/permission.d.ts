export * as Permission from "./permission.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "Permission.ID"> & {
    create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
};
export type ID = typeof ID.Type;
export declare const Source: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"tool">;
    readonly messageID: Schema.String;
    readonly id: Schema.String;
}>]>;
export type Source = typeof Source.Type;
export declare const Request: Schema.Struct<{
    readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
        create: () => string & import("effect/Brand").Brand<"SessionID">;
        descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
    };
    readonly action: Schema.String;
    readonly resources: Schema.$Array<Schema.String>;
    readonly save: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly source: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"tool">;
        readonly messageID: Schema.String;
        readonly id: Schema.String;
    }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
        readonly type: Schema.Literal<"tool">;
        readonly messageID: Schema.String;
        readonly id: Schema.String;
    }>]>>, never, never>;
    readonly message: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly id: Schema.brand<Schema.String, "Permission.ID"> & {
        create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
    };
}>;
export interface Request extends Schema.Schema.Type<typeof Request> {
}
export declare const Reply: Schema.Literals<readonly ["once", "always", "reject"]>;
export type Reply = typeof Reply.Type;
export declare const Event: {
    Asked: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"permission.asked">;
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
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly action: Schema.String;
            readonly resources: Schema.$Array<Schema.String>;
            readonly save: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
            readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
            readonly source: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>, never, never>;
            readonly message: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly id: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
        }>;
    }> & {
        type: "permission.asked";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly action: Schema.String;
            readonly resources: Schema.$Array<Schema.String>;
            readonly save: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
            readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
            readonly source: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>, never, never>;
            readonly message: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly id: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
        }>;
    };
    Replied: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"permission.replied">;
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
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly requestID: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
            readonly reply: Schema.Literals<readonly ["once", "always", "reject"]>;
        }>;
    }> & {
        type: "permission.replied";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly requestID: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
            readonly reply: Schema.Literals<readonly ["once", "always", "reject"]>;
        }>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"permission.asked">;
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
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly action: Schema.String;
            readonly resources: Schema.$Array<Schema.String>;
            readonly save: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
            readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
            readonly source: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>, never, never>;
            readonly message: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly id: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
        }>;
    }> & {
        type: "permission.asked";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly action: Schema.String;
            readonly resources: Schema.$Array<Schema.String>;
            readonly save: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
            readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
            readonly source: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"tool">;
                readonly messageID: Schema.String;
                readonly id: Schema.String;
            }>]>>, never, never>;
            readonly message: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly id: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
        }>;
    }, Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"permission.replied">;
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
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly requestID: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
            readonly reply: Schema.Literals<readonly ["once", "always", "reject"]>;
        }>;
    }> & {
        type: "permission.replied";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{
            readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
                create: () => string & import("effect/Brand").Brand<"SessionID">;
                descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
            };
            readonly requestID: Schema.brand<Schema.String, "Permission.ID"> & {
                create: (id?: string) => string & import("effect/Brand").Brand<"Permission.ID">;
            };
            readonly reply: Schema.Literals<readonly ["once", "always", "reject"]>;
        }>;
    }];
};
export declare const Effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
export type Effect = typeof Effect.Type;
export interface Rule extends Schema.Schema.Type<typeof Rule> {
}
export declare const Rule: Schema.Struct<{
    readonly action: Schema.String;
    readonly resource: Schema.String;
    readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
}>;
export declare const Ruleset: Schema.$Array<Schema.Struct<{
    readonly action: Schema.String;
    readonly resource: Schema.String;
    readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
}>>;
export type Ruleset = typeof Ruleset.Type;
