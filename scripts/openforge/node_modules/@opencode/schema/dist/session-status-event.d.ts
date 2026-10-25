export * as SessionStatusEvent from "./session-status-event.js";
import { Schema } from "effect";
export declare const Info: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"idle">;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"retry">;
    readonly attempt: Schema.Int;
    readonly message: Schema.String;
    readonly action: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly reason: Schema.String;
        readonly provider: Schema.String;
        readonly title: Schema.String;
        readonly message: Schema.String;
        readonly label: Schema.String;
        readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly reason: Schema.String;
        readonly provider: Schema.String;
        readonly title: Schema.String;
        readonly message: Schema.String;
        readonly label: Schema.String;
        readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
    readonly next: Schema.Int;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"busy">;
}>]>;
export type Info = Schema.Schema.Type<typeof Info>;
export declare const Status: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"session.status">;
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
        readonly status: Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"idle">;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"retry">;
            readonly attempt: Schema.Int;
            readonly message: Schema.String;
            readonly action: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly next: Schema.Int;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"busy">;
        }>]>;
    }>;
}> & {
    type: "session.status";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
        readonly status: Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"idle">;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"retry">;
            readonly attempt: Schema.Int;
            readonly message: Schema.String;
            readonly action: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly next: Schema.Int;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"busy">;
        }>]>;
    }>;
};
export declare const Idle: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"session.idle">;
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
    }>;
}> & {
    type: "session.idle";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
    }>;
};
export declare const Definitions: readonly [Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"session.status">;
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
        readonly status: Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"idle">;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"retry">;
            readonly attempt: Schema.Int;
            readonly message: Schema.String;
            readonly action: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly next: Schema.Int;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"busy">;
        }>]>;
    }>;
}> & {
    type: "session.status";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
        readonly status: Schema.Union<readonly [Schema.Struct<{
            readonly type: Schema.Literal<"idle">;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"retry">;
            readonly attempt: Schema.Int;
            readonly message: Schema.String;
            readonly action: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, Schema.optionalKey<Schema.Struct<{
                readonly reason: Schema.String;
                readonly provider: Schema.String;
                readonly title: Schema.String;
                readonly message: Schema.String;
                readonly label: Schema.String;
                readonly link: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>, never, never>;
            readonly next: Schema.Int;
        }>, Schema.Struct<{
            readonly type: Schema.Literal<"busy">;
        }>]>;
    }>;
}, Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"session.idle">;
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
    }>;
}> & {
    type: "session.idle";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
    }>;
}];
