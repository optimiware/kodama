/**
 * Stores event-log entries and replication state.
 *
 * `EventJournal` records committed entries, exposes them for replay, publishes
 * local changes, and tracks the remote metadata needed to exchange entries with
 * other journals. Higher-level event-log schemas and handlers use this service
 * to rebuild projections, sync offline clients, import remote changes, and
 * coordinate writes per store. This module also defines journal errors, entry
 * and remote identifiers, schemas, and in-memory or IndexedDB-backed journal
 * layers.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.ts";
import type { Brand } from "../Brand.ts";
import * as Context from "../Context.ts";
import * as DateTime from "../DateTime.ts";
import * as Effect from "../Effect.ts";
import * as SchemaBinary from "../encoding/SchemaBinary.ts";
import * as Layer from "../Layer.ts";
import type { Option } from "../Option.ts";
import * as Order from "../Order.ts";
import * as PubSub from "../PubSub.ts";
import * as Schema from "../Schema.ts";
import type { Scope } from "../Scope.ts";
import type { StoreId } from "./EventLogMessage.ts";
declare const EventJournal_base: Context.ServiceClass<EventJournal, "effect/eventlog/EventJournal", {
    /**
     * Read all the entries in the journal.
     */
    readonly entries: Effect.Effect<ReadonlyArray<Entry>, EventJournalError>;
    /**
     * Write an event to the journal, performing an effect before committing the
     * event.
     */
    readonly write: <A, E, R>(options: {
        readonly event: string;
        readonly primaryKey: string;
        readonly payload: Uint8Array;
        readonly effect: (entry: Entry) => Effect.Effect<A, E, R>;
    }) => Effect.Effect<A, EventJournalError | E, R>;
    /**
     * Write events from a remote source to the journal.
     *
     * **Details**
     *
     * Effects run sequentially in compaction bracket order.
     */
    readonly writeFromRemote: (options: {
        readonly remoteId: RemoteId;
        readonly entries: ReadonlyArray<RemoteEntry>;
        readonly compact?: ((uncommitted: ReadonlyArray<RemoteEntry>) => Effect.Effect<ReadonlyArray<Entry>, EventJournalError>) | undefined;
        readonly effect: (options: {
            readonly entry: Entry;
            readonly conflicts: ReadonlyArray<Entry>;
        }) => Effect.Effect<void, EventJournalError>;
    }) => Effect.Effect<{
        readonly duplicateEntries: ReadonlyArray<Entry>;
    }, EventJournalError>;
    /**
     * Run an effect with the uncommitted entries for a remote source.
     *
     * The effect is not run when there are no uncommitted entries, in which case
     * `Option.none()` is returned. Otherwise, its result is wrapped in
     * `Option.some()`.
     */
    readonly withRemoteUncommited: <A, E, R>(remoteId: RemoteId, f: (entries: Arr.NonEmptyReadonlyArray<Entry>) => Effect.Effect<A, E, R>) => Effect.Effect<Option<A>, EventJournalError | E, R>;
    /**
     * Retrieve the first unused sequence number for a remote source.
     */
    readonly nextRemoteSequence: (remoteId: RemoteId) => Effect.Effect<number, EventJournalError>;
    /**
     * The entries added to the local journal.
     */
    readonly changes: Effect.Effect<PubSub.Subscription<Entry>, never, Scope>;
    /**
     * Remove all data
     */
    readonly destroy: Effect.Effect<void, EventJournalError>;
    /**
     * Run an effect with a lock on the journal.
     */
    readonly withLock: (storeId: StoreId) => <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
}>;
/**
 * Context service for storing and replaying event journal entries.
 *
 * **Details**
 *
 * The service writes local entries, imports entries from remote journals, exposes
 * a stream of local changes, and provides per-store locking.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class EventJournal extends EventJournal_base {
}
declare const TypeId: "effect/eventlog/EventJournal/EventJournalError";
declare const EventJournalError_base: new <A extends Record<string, any> = {}>(args: import("../Types.ts").VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "EventJournalError";
} & Readonly<A>;
/**
 * Error raised by event journal operations.
 *
 * **Details**
 *
 * The error records the journal method that failed and the underlying cause.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class EventJournalError extends EventJournalError_base<{
    readonly method: string;
    readonly cause: unknown;
}> {
    /**
     * Marks this value as an event journal error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [TypeId]: "effect/eventlog/EventJournal/EventJournalError";
}
/**
 * Brand identifier used for `RemoteId` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type RemoteIdTypeId = "effect/eventlog/EventJournal/RemoteId";
/**
 * Runtime brand identifier used for `RemoteId` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const RemoteIdTypeId: RemoteIdTypeId;
/**
 * Branded byte identifier for a remote event journal source.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type RemoteId = Uint8Array & Brand<RemoteIdTypeId>;
/**
 * Schema for branded remote event journal identifiers.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const RemoteId: Schema.brand<Schema.Uint8Array, "effect/eventlog/EventJournal/RemoteId">;
/**
 * Generates a new random `RemoteId`.
 *
 * **When to use**
 *
 * Use when generating a fresh event-log remote id internally and the UUID bytes
 * are trusted to satisfy the brand.
 *
 * **Gotchas**
 *
 * This is unsafe because the generated UUID bytes are cast to the brand without
 * schema validation.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export declare const makeRemoteIdUnsafe: () => RemoteId;
/**
 * Runtime brand identifier used for `EntryId` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const EntryIdTypeId: EntryIdTypeId;
/**
 * Brand identifier used for `EntryId` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type EntryIdTypeId = "effect/eventlog/EventJournal/EntryId";
/**
 * Branded byte identifier for an event journal entry.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type EntryId = Uint8Array<ArrayBuffer> & Brand<EntryIdTypeId>;
/**
 * Schema for branded event journal entry identifiers.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const EntryId: Schema.brand<Schema.instanceOf<Uint8Array<ArrayBuffer>, Uint8Array<ArrayBuffer>>, "effect/eventlog/EventJournal/EntryId">;
/**
 * Provides an Ordering instance for entry identifiers based on their raw UUID bytes.
 *
 * @stability unstable
 * @category ordering
 * @since 4.0.0
 */
export declare const EntryIdOrder: Order.Order<EntryId>;
/**
 * Generates a UUID v7 `EntryId`, optionally using the supplied millisecond
 * timestamp.
 *
 * **When to use**
 *
 * Use when generating an event-log entry id internally and the UUID v7 bytes
 * are trusted to satisfy the brand.
 *
 * **Gotchas**
 *
 * This is unsafe because the generated UUID bytes are cast to the brand without
 * schema validation.
 *
 * @stability unstable
 * @category unsafe
 * @since 4.0.0
 */
export declare const makeEntryIdUnsafe: (options?: {
    msecs?: number;
}) => EntryId;
/**
 * Extracts the millisecond timestamp encoded in a UUID v7 `EntryId`.
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export declare const entryIdMillis: (entryId: EntryId) => number;
declare const Entry_base: Schema.Class<Entry, Schema.Struct<{
    readonly id: Schema.brand<Schema.instanceOf<Uint8Array<ArrayBuffer>, Uint8Array<ArrayBuffer>>, "effect/eventlog/EventJournal/EntryId">;
    readonly event: Schema.String;
    readonly primaryKey: Schema.String;
    readonly payload: Schema.Uint8Array;
}>, {}>;
/**
 * Schema for a committed event journal entry.
 *
 * **Details**
 *
 * An entry records its ID, event tag, primary key, and SchemaBinary-encoded
 * payload, with helpers for array SchemaBinary encoding and creation timestamps.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class Entry extends Entry_base {
    /**
     * SchemaBinary codec for arrays of committed event journal entries.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static arraySchemaBinary: Schema.$Array<SchemaBinary.toCodec<typeof Entry>>;
    /**
     * Encodes arrays of committed entries with the SchemaBinary entry codec.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static encodeArray: (input: unknown, options?: import("../SchemaAST.ts").ParseOptions) => Effect.Effect<readonly Uint8Array<ArrayBuffer>[], Schema.SchemaError, never>;
    /**
     * Decodes arrays of committed entries with the SchemaBinary entry codec.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static decodeArray: (input: unknown, options?: import("../SchemaAST.ts").ParseOptions) => Effect.Effect<readonly Entry[], Schema.SchemaError, never>;
    /**
     * Ordering for committed entries by their event journal entry id.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static Order: Order.Order<Entry>;
    /**
     * String representation of the entry id.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get idString(): string;
    /**
     * Creation timestamp encoded in the UUID v7 entry id, in epoch milliseconds.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get createdAtMillis(): number;
    /**
     * Creation timestamp encoded in the UUID v7 entry id, as a UTC date-time.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get createdAt(): DateTime.Utc;
}
declare const RemoteEntry_base: Schema.Class<RemoteEntry, Schema.Struct<{
    readonly remoteSequence: Schema.Natural;
    readonly entry: typeof Entry;
}>, {}>;
/**
 * Schema for an event journal entry received from a remote source.
 *
 * **Details**
 *
 * It pairs the remote sequence number with the journal entry payload.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare class RemoteEntry extends RemoteEntry_base {
}
/**
 * Creates an in-memory `EventJournal` service.
 *
 * **Gotchas**
 *
 * Entries, remote tracking state, and locks live only in the current process and
 * are lost when the service is discarded.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeMemory: Effect.Effect<EventJournal["Service"]>;
/**
 * Layer that provides an in-memory `EventJournal`.
 *
 * **Gotchas**
 *
 * All journal data is stored in process memory and is not persisted across layer
 * lifetimes.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerMemory: Layer.Layer<EventJournal>;
/**
 * Creates an `EventJournal` backed by IndexedDB.
 *
 * **Details**
 *
 * The journal stores entries and remote replication metadata in the configured
 * browser database, publishes local changes, and requires `Scope` so the database
 * connection can be closed when the scope ends.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeIndexedDb: (options?: {
    readonly database?: string;
}) => Effect.Effect<EventJournal["Service"], EventJournalError, Scope>;
/**
 * Provides `EventJournal` using the IndexedDB-backed implementation created by
 * `makeIndexedDb`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerIndexedDb: (options?: {
    readonly database?: string;
}) => Layer.Layer<EventJournal, EventJournalError>;
export {};
//# sourceMappingURL=EventJournal.d.ts.map