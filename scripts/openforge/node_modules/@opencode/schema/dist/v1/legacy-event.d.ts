export * as LegacyEventV1 from "./legacy-event.js";
import { Schema } from "effect";
export declare const CommandExecuted: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"command.executed">;
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
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
        readonly arguments: Schema.String;
        readonly messageID: Schema.brand<Schema.String, "SessionV1.MessageID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"SessionV1.MessageID">;
        };
    }>;
}> & {
    type: "command.executed";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly name: Schema.String;
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
        readonly arguments: Schema.String;
        readonly messageID: Schema.brand<Schema.String, "SessionV1.MessageID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"SessionV1.MessageID">;
        };
    }>;
};
export declare const Definitions: readonly [Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<"command.executed">;
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
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
        readonly arguments: Schema.String;
        readonly messageID: Schema.brand<Schema.String, "SessionV1.MessageID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"SessionV1.MessageID">;
        };
    }>;
}> & {
    type: "command.executed";
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<{
        readonly name: Schema.String;
        readonly sessionID: Schema.brand<Schema.String, "SessionID"> & {
            create: () => string & import("effect/Brand").Brand<"SessionID">;
            descending: (id?: string) => string & import("effect/Brand").Brand<"SessionID">;
        };
        readonly arguments: Schema.String;
        readonly messageID: Schema.brand<Schema.String, "SessionV1.MessageID"> & {
            ascending: (id?: string) => string & import("effect/Brand").Brand<"SessionV1.MessageID">;
        };
    }>;
}];
