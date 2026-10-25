export * as Event from "./event.js";
import { Schema } from "effect";
import { Location } from "./location.js";
export declare const ID: Schema.brand<Schema.String, "Event.ID"> & {
    create: () => string & import("effect/Brand").Brand<"Event.ID">;
};
export type ID = typeof ID.Type;
/**
 * Position in one aggregate's durable log. Values originate from the durable
 * event envelope and synced markers;
 * `after` cursors accept only values that came from those sources.
 */
export declare const Seq: Schema.brand<Schema.Int, "Event.Seq">;
export type Seq = typeof Seq.Type;
/** Durable schema version of one event type, from the event definition that committed it. */
export declare const Version: Schema.brand<Schema.Int, "Event.Version">;
export type Version = typeof Version.Type;
declare const DurableEnvelope: Schema.Struct<{
    readonly aggregateID: Schema.String;
    readonly seq: Schema.brand<Schema.Int, "Event.Seq">;
    readonly version: Schema.brand<Schema.Int, "Event.Version">;
}>;
export type DurableEnvelope = typeof DurableEnvelope.Type;
export type DurableDefinition<Type extends string = string, DataSchema extends Schema.Codec<unknown, unknown> = Schema.Codec<unknown, unknown>> = Schema.Top & {
    readonly type: Type;
    readonly durability: "durable";
    readonly durable: {
        readonly version: number;
        readonly aggregate: string;
    };
    readonly data: DataSchema;
};
export type EphemeralDefinition<Type extends string = string, DataSchema extends Schema.Codec<unknown, unknown> = Schema.Codec<unknown, unknown>> = Schema.Top & {
    readonly type: Type;
    readonly durability: "ephemeral";
    readonly durable?: never;
    readonly data: DataSchema;
};
export type Definition<Type extends string = string, DataSchema extends Schema.Codec<unknown, unknown> = Schema.Codec<unknown, unknown>> = DurableDefinition<Type, DataSchema> | EphemeralDefinition<Type, DataSchema>;
export type Data<D extends Definition> = Schema.Schema.Type<D["data"]>;
type PayloadBase<D extends Definition> = {
    readonly id: ID;
    readonly type: D["type"];
    readonly created: number;
    readonly data: Data<D>;
    readonly location?: Location.Ref;
    readonly metadata?: Record<string, unknown>;
};
export type Payload<D extends Definition = Definition> = D extends DurableDefinition ? PayloadBase<D> & {
    readonly durable: DurableEnvelope;
} : PayloadBase<D> & {
    readonly durable?: never;
};
type Input<Type extends string, Fields extends Readonly<Record<PropertyKey, Schema.Codec<unknown, unknown>>>> = {
    readonly type: Type;
    readonly identifier?: string;
    readonly durable?: {
        readonly version: number;
        readonly aggregate: string;
    };
    readonly schema: Fields;
};
export declare function durable<const Type extends string, const Fields extends Readonly<Record<PropertyKey, Schema.Codec<unknown, unknown>>>>(input: Input<Type, Fields> & {
    readonly durable: NonNullable<Input<Type, Fields>["durable"]>;
}): Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<Type>;
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
    readonly data: Schema.Struct<Fields>;
}> & {
    type: Type;
    durability: "durable";
    durable: {
        readonly version: number;
        readonly aggregate: string;
    };
    data: Schema.Struct<Fields>;
};
export declare function ephemeral<const Type extends string, const Fields extends Readonly<Record<PropertyKey, Schema.Codec<unknown, unknown>>>>(input: Omit<Input<Type, Fields>, "durable">): Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Event.ID"> & {
        create: () => string & import("effect/Brand").Brand<"Event.ID">;
    };
    readonly created: Schema.Finite;
    readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
    readonly type: Schema.Literal<Type>;
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
    readonly data: Schema.Struct<Fields>;
}> & {
    type: Type;
    durability: "ephemeral";
    durable: undefined;
    data: Schema.Struct<Fields>;
};
export declare function inventory<const Definitions extends ReadonlyArray<Definition>>(...definitions: Definitions): Readonly<Definitions>;
export declare function latest(definitions: ReadonlyArray<Definition>): ReadonlyMap<string, Definition<string, Schema.Codec<unknown, unknown, never, never>>>;
export declare function versionedType(type: string, version: number): string;
export declare function durableMap<const Definitions extends ReadonlyArray<Definition>>(definitions: Definitions): ReadonlyMap<string, DurableDefinition<string, Schema.Codec<unknown, unknown, never, never>>>;
