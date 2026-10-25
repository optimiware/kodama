/**
 * Stores Effect Cluster messages and replies behind a pluggable backend.
 *
 * `MessageStorage` is the boundary between cluster runner logic and the storage
 * system that keeps mailbox state recoverable. It saves requests, control
 * envelopes, and replies; finds unprocessed messages for assigned shards;
 * tracks duplicate requests; and manages reply handlers waiting for responses.
 * This module also includes the encoded storage-driver contract and no-op or
 * in-memory implementations for local use and tests.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.ts";
import * as Context from "../Context.ts";
import * as Data from "../Data.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as Option from "../Option.ts";
import type * as Rpc from "../rpc/Rpc.ts";
import { EntityNotAssignedToRunner, MalformedMessage, type PersistenceError } from "./ClusterError.ts";
import type { EntityAddress } from "./EntityAddress.ts";
import * as Envelope from "./Envelope.ts";
import * as Message from "./Message.ts";
import * as Reply from "./Reply.ts";
import * as ShardId from "./ShardId.ts";
import type { ShardingConfig } from "./ShardingConfig.ts";
import * as Snowflake from "./Snowflake.ts";
declare const MessageStorage_base: Context.ServiceClass<MessageStorage, "effect/cluster/MessageStorage", {
    /**
     * Save the provided message and its associated metadata.
     */
    readonly saveRequest: <R extends Rpc.Any>(envelope: Message.OutgoingRequest<R>) => Effect.Effect<SaveResult<R>, PersistenceError | MalformedMessage>;
    /**
     * Save the provided message and its associated metadata.
     */
    readonly saveEnvelope: (envelope: Message.OutgoingEnvelope) => Effect.Effect<void, PersistenceError | MalformedMessage>;
    /**
     * Save the provided `Reply` and its associated metadata.
     */
    readonly saveReply: <R extends Rpc.Any>(reply: Reply.ReplyWithContext<R>) => Effect.Effect<void, PersistenceError | MalformedMessage>;
    /**
     * Clear replies for the given request id. When options.expectedReplyId is
     * provided, clear only if it is still the latest reply at the storage boundary.
     * Without it, clear unconditionally.
     * Custom storage implementations must honor this condition to protect
     * workflows from stale concurrent resumes.
     */
    readonly clearReplies: (requestId: Snowflake.Snowflake, options?: {
        readonly expectedReplyId?: Snowflake.Snowflake | undefined;
    }) => Effect.Effect<void, PersistenceError>;
    /**
     * Retrieves the replies for the specified requests.
     *
     * **Details**
     *
     * This returns:
     *
     * - Un-acknowledged chunk replies
     * - `WithExit` replies
     */
    readonly repliesFor: <R extends Rpc.Any>(requests: Iterable<Message.OutgoingRequest<R>>) => Effect.Effect<Array<Reply.Reply<R>>, PersistenceError | MalformedMessage>;
    /**
     * Retrieves the encoded replies for the specified request ids.
     */
    readonly repliesForUnfiltered: (requestIds: Iterable<Snowflake.Snowflake>) => Effect.Effect<Array<Reply.Encoded>, PersistenceError | MalformedMessage>;
    /**
     * Retrieves the request id for the specified primary key.
     */
    readonly requestIdForPrimaryKey: (options: {
        readonly address: EntityAddress;
        readonly tag: string;
        readonly id: string;
    }) => Effect.Effect<Option.Option<Snowflake.Snowflake>, PersistenceError>;
    /**
     * For locally sent messages, register a handler to process the replies.
     */
    readonly registerReplyHandler: <R extends Rpc.Any>(message: Message.OutgoingRequest<R> | Message.IncomingRequest<R>) => Effect.Effect<void, EntityNotAssignedToRunner>;
    /**
     * Unregister the reply handler for the specified message.
     */
    readonly unregisterReplyHandler: (requestId: Snowflake.Snowflake) => Effect.Effect<void>;
    /**
     * Unregister the reply handlers for the specified ShardId.
     *
     * By default the waiters are resumed with `EntityNotAssignedToRunner`, so
     * they can re-route the wait. With `interrupt: true` the waiters are
     * interrupted with a cluster-abandon annotation instead, for when the runner
     * is shutting down and the reply can never be observed here.
     */
    readonly unregisterShardReplyHandlers: (shardId: ShardId.ShardId, options?: {
        readonly interrupt?: boolean | undefined;
    }) => Effect.Effect<void>;
    /**
     * Retrieves the unprocessed messages for the specified shards.
     *
     * **Details**
     *
     * A message is unprocessed when:
     *
     * - Requests that have no `WithExit` replies or no unacknowledged chunk replies
     * - The latest `AckChunk` envelope
     * - All `Interrupt` envelopes for unprocessed requests
     *
     * The `limit` option bounds the number of messages returned in a single
     * read, and the `addresses` option restricts the read to the provided
     * entity addresses. Only the returned messages are claimed; other messages
     * stay eligible for later reads.
     */
    readonly unprocessedMessages: (shardIds: Iterable<ShardId.ShardId>, options?: {
        readonly limit?: number | undefined;
        readonly addresses?: ReadonlyArray<EntityAddress> | undefined;
    } | undefined) => Effect.Effect<Array<Message.Incoming<any>>, PersistenceError>;
    /**
     * Retrieves the unprocessed messages by id.
     */
    readonly unprocessedMessagesById: <R extends Rpc.Any>(messageIds: Iterable<Snowflake.Snowflake>) => Effect.Effect<Array<Message.Incoming<R>>, PersistenceError>;
    /**
     * Reset the mailbox state for the provided shards.
     */
    readonly resetShards: (shardIds: Iterable<ShardId.ShardId>) => Effect.Effect<void, PersistenceError>;
    /**
     * Reset the mailbox state for the provided address.
     */
    readonly resetAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
    /** Release request claims without changing replies or processed state. */
    readonly resetRequests: (requestIds: ReadonlyArray<Snowflake.Snowflake>) => Effect.Effect<void, PersistenceError>;
    /**
     * Reset the mailbox state for the provided addresses.
     */
    readonly resetAddresses: (addresses: ReadonlyArray<EntityAddress>) => Effect.Effect<void, PersistenceError>;
    /**
     * Clear all messages and replies for the provided address.
     */
    readonly clearAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
    /**
     * Used to wrap requests with transactions.
     */
    readonly withTransaction: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
}>;
/**
 * Service for cluster mailbox persistence and reply delivery.
 *
 * **Details**
 *
 * It stores outgoing requests, control envelopes, and replies; reads unprocessed
 * messages; manages reply handlers; and provides transaction wrapping for storage
 * operations.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class MessageStorage extends MessageStorage_base {
}
/**
 * Result of saving a request or envelope into message storage.
 *
 * **Details**
 *
 * A duplicate result carries the original request ID and the last reply already
 * received for the duplicated request.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type SaveResult<R extends Rpc.Any> = SaveResult.Success | SaveResult.Duplicate<R>;
/**
 * Constructors and matchers for decoded save results.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const SaveResult: {
    readonly $is: <Tag extends "Duplicate" | "Success">(tag: Tag) => {
        <T extends Data.TaggedEnum.Kind<SaveResult.Constructor, any, any, any, any>>(u: T): u is T & {
            readonly _tag: Tag;
        };
        (u: unknown): u is Extract<SaveResult.Duplicate<never>, {
            readonly _tag: Tag;
        }> | Extract<SaveResult.Success, {
            readonly _tag: Tag;
        }>;
    };
    readonly $match: {
        <A, B, C, D, Cases extends { readonly [Tag in "Duplicate" | "Success"]: (args: Extract<Data.TaggedEnum.Kind<SaveResult.Constructor, A, B, C, D>, {
            readonly _tag: Tag;
        }>) => any; }>(cases: Cases): (self: SaveResult<A extends Rpc.Any ? A : never>) => import("../Unify.ts").Unify<ReturnType<Cases["Duplicate" | "Success"]>>;
        <A, B, C, D, Cases extends { readonly [Tag in "Duplicate" | "Success"]: (args: Extract<Data.TaggedEnum.Kind<SaveResult.Constructor, A, B, C, D>, {
            readonly _tag: Tag;
        }>) => any; }>(self: SaveResult<A extends Rpc.Any ? A : never>, cases: Cases): import("../Unify.ts").Unify<ReturnType<Cases["Duplicate" | "Success"]>>;
    };
    readonly Duplicate: <A>(args: {
        readonly originalId: Snowflake.Snowflake;
        readonly lastReceivedReply: Option.Option<Reply.Reply<A extends Rpc.Any ? A : never>>;
    }) => SaveResult.Duplicate<A extends Rpc.Any ? A : never>;
    readonly Success: <A>(args: void) => SaveResult.Success;
};
/**
 * Constructors and matchers for encoded save results returned by storage
 * drivers.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const SaveResultEncoded: {
    readonly $is: <Tag extends "Duplicate" | "Success">(tag: Tag) => (u: unknown) => u is Extract<SaveResult.DuplicateEncoded, {
        readonly _tag: Tag;
    }> | Extract<SaveResult.Success, {
        readonly _tag: Tag;
    }>;
    readonly $match: {
        <Cases extends { readonly [Tag in "Duplicate" | "Success"]: (args: Extract<SaveResult.Encoded, {
            readonly _tag: Tag;
        }>) => any; }>(cases: Cases): (value: SaveResult.Encoded) => import("../Unify.ts").Unify<ReturnType<Cases["Duplicate" | "Success"]>>;
        <Cases extends { readonly [Tag in "Duplicate" | "Success"]: (args: Extract<SaveResult.Encoded, {
            readonly _tag: Tag;
        }>) => any; }>(value: SaveResult.Encoded, cases: Cases): import("../Unify.ts").Unify<ReturnType<Cases["Duplicate" | "Success"]>>;
    };
    readonly Duplicate: Data.TaggedEnum.ConstructorFrom<SaveResult.DuplicateEncoded, "_tag">;
    readonly Success: Data.TaggedEnum.ConstructorFrom<SaveResult.Success, "_tag">;
};
/**
 * Variants and helper types for `SaveResult`.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace SaveResult {
    /**
     * Encoded storage-driver form of `SaveResult`.
     *
     * **Details**
     *
     * Duplicate results contain an encoded last received reply instead of a decoded
     * reply.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Encoded = SaveResult.Success | SaveResult.DuplicateEncoded;
    /**
     * Variant indicating that the message was saved as a new storage entry.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface Success {
        readonly _tag: "Success";
    }
    /**
     * Variant indicating that the request duplicates an existing stored request.
     *
     * **Details**
     *
     * It carries the original request ID and the latest decoded reply, when one is
     * available.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface Duplicate<R extends Rpc.Any> {
        readonly _tag: "Duplicate";
        readonly originalId: Snowflake.Snowflake;
        readonly lastReceivedReply: Option.Option<Reply.Reply<R>>;
    }
    /**
     * Encoded duplicate-save variant returned by lower-level storage drivers.
     *
     * **Details**
     *
     * It carries the original request ID and the latest encoded reply, when one is
     * available.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    interface DuplicateEncoded {
        readonly _tag: "Duplicate";
        readonly originalId: Snowflake.Snowflake;
        readonly lastReceivedReply: Option.Option<Reply.Encoded>;
    }
    /**
     * Generic tagged enum constructor type for `SaveResult`.
     *
     * @stability unstable
     * @category utility types
     * @since 4.0.0
     */
    interface Constructor extends Data.TaggedEnum.WithGenerics<1> {
        readonly taggedEnum: SaveResult<this["A"] extends Rpc.Any ? this["A"] : never>;
    }
}
/**
 * Low-level storage-driver contract for encoded envelopes and replies.
 *
 * **Details**
 *
 * Implementations persist encoded messages, track primary keys and delayed
 * delivery, read unprocessed messages, and provide transaction wrapping.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export type Encoded = {
    /**
     * Save the provided message and its associated metadata.
     */
    readonly saveEnvelope: (options: {
        readonly envelope: Envelope.Encoded;
        readonly primaryKey: string | null;
        readonly deliverAt: number | null;
    }) => Effect.Effect<SaveResult.Encoded, PersistenceError>;
    /**
     * Save the provided `Reply` and its associated metadata.
     */
    readonly saveReply: (reply: Reply.Encoded) => Effect.Effect<void, PersistenceError>;
    /**
     * Remove the replies for the specified request. If options.expectedReplyId
     * is provided, compare it with the latest reply atomically before clearing.
     * Without it, clear unconditionally.
     * Custom storage implementations must honor this condition to protect
     * workflows from stale concurrent resumes.
     */
    readonly clearReplies: (requestId: Snowflake.Snowflake, options?: {
        readonly expectedReplyId?: Snowflake.Snowflake | undefined;
    }) => Effect.Effect<void, PersistenceError>;
    /**
     * Retrieves the request id for the specified primary key.
     */
    readonly requestIdForPrimaryKey: (primaryKey: string) => Effect.Effect<Option.Option<Snowflake.Snowflake>, PersistenceError>;
    /**
     * Retrieves the replies for the specified requests.
     *
     * **Details**
     *
     * This returns:
     *
     * - Un-acknowledged chunk replies
     * - `WithExit` replies
     */
    readonly repliesFor: (requestIds: Arr.NonEmptyArray<string>) => Effect.Effect<Array<Reply.Encoded>, PersistenceError>;
    /**
     * Retrieves the replies for the specified request ids.
     */
    readonly repliesForUnfiltered: (requestIds: Arr.NonEmptyArray<string>) => Effect.Effect<Array<Reply.Encoded>, PersistenceError>;
    /**
     * Retrieves the unprocessed messages for the given shards.
     *
     * **Details**
     *
     * A message is unprocessed when:
     *
     * - Requests that have no `WithExit` replies or no unacknowledged chunk replies
     * - The latest `AckChunk` envelope
     * - All `Interrupt` envelopes for unprocessed requests
     *
     * The `limit` option bounds the number of rows returned by a single read,
     * and the `addresses` option restricts the read to the provided entity
     * addresses. Only the returned rows are claimed; other rows stay eligible
     * for later reads.
     */
    readonly unprocessedMessages: (shardIds: Arr.NonEmptyArray<string>, now: number, options?: {
        readonly limit?: number | undefined;
        readonly addresses?: ReadonlyArray<EntityAddress> | undefined;
    } | undefined) => Effect.Effect<Array<{
        readonly envelope: Envelope.Encoded;
        readonly lastSentReply: Option.Option<Reply.Encoded>;
    }>, PersistenceError>;
    /**
     * Retrieves the unprocessed messages by id.
     */
    readonly unprocessedMessagesById: (messageIds: Arr.NonEmptyArray<Snowflake.Snowflake>, now: number) => Effect.Effect<Array<{
        readonly envelope: Envelope.Encoded;
        readonly lastSentReply: Option.Option<Reply.Encoded>;
    }>, PersistenceError>;
    /** Release request claims without changing replies or processed state. */
    readonly resetRequests: (requestIds: ReadonlyArray<Snowflake.Snowflake>) => Effect.Effect<void, PersistenceError>;
    /**
     * Reset the mailbox state for the provided addresses.
     */
    readonly resetAddresses: (addresses: ReadonlyArray<EntityAddress>) => Effect.Effect<void, PersistenceError>;
    /**
     * Clear all messages and replies for the provided address.
     */
    readonly clearAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
    /**
     * Reset the mailbox state for the provided shards.
     */
    readonly resetShards: (shardIds: Arr.NonEmptyArray<string>) => Effect.Effect<void, PersistenceError>;
    /**
     * Used to wrap requests with transactions.
     */
    readonly withTransaction: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
};
/**
 * Cursor options for reading encoded unprocessed messages across shard sets.
 *
 * **Details**
 *
 * The fields distinguish existing shards from newly assigned shards and carry the
 * driver-specific pagination cursor.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type EncodedUnprocessedOptions<A> = {
    readonly existingShards: Array<number>;
    readonly newShards: Array<number>;
    readonly cursor: Option.Option<A>;
};
/**
 * Cursor options for reading encoded replies across request sets.
 *
 * **Details**
 *
 * The fields distinguish existing requests from new requests and carry the
 * driver-specific pagination cursor.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type EncodedRepliesOptions<A> = {
    readonly existingRequests: Array<string>;
    readonly newRequests: Array<string>;
    readonly cursor: Option.Option<A>;
};
/**
 * Wraps a concrete message storage implementation with reply-handler management.
 *
 * **Details**
 *
 * The returned service can register waiting reply handlers, notify them when
 * replies are saved, and fail them when a request or shard is unregistered.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (storage: Omit<MessageStorage["Service"], "saveReply" | "registerReplyHandler" | "unregisterReplyHandler" | "unregisterShardReplyHandlers"> & {
    /**
     * Save the provided `Reply`, returning the reply as it was persisted.
     *
     * Implementations that persist a different representation, such as a
     * defect reply when the original cannot be encoded, return that fallback
     * so reply handlers deliver exactly what storage recorded.
     */
    readonly saveReply: <R extends Rpc.Any>(reply: Reply.ReplyWithContext<R>) => Effect.Effect<Reply.ReplyWithContext<R>, PersistenceError | MalformedMessage>;
}) => Effect.Effect<MessageStorage["Service"]>;
/**
 * Builds a `MessageStorage` service from an encoded storage driver.
 *
 * **Details**
 *
 * The adapter handles envelope and reply encoding and decoding, primary-key
 * generation, delayed delivery checks, duplicate decoding, and malformed-message
 * defect replies.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeEncoded: (encoded: Encoded) => Effect.Effect<MessageStorage["Service"], never, Snowflake.Generator>;
/**
 * No-op `MessageStorage` service that does not persist messages or replies.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const noop: MessageStorage["Service"];
/**
 * In-memory storage entry for a request envelope.
 *
 * **Details**
 *
 * It stores the encoded envelope, last acknowledged chunk, accumulated replies,
 * and optional delivery time.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type MemoryEntry = {
    readonly envelope: Envelope.Encoded;
    lastReceivedChunk: Reply.ChunkEncoded | undefined;
    replies: Array<Reply.Encoded>;
    deliverAt: number | null;
};
/**
 * Provides a context reference used in tests to simulate a transaction.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const MemoryTransaction: Context.Reference<boolean>;
declare const MemoryDriver_base: Context.ServiceClass<MemoryDriver, "effect/cluster/MessageStorage/MemoryDriver", {
    readonly storage: {
        /**
         * Save the provided message and its associated metadata.
         */
        readonly saveRequest: <R extends Rpc.Any>(envelope: Message.OutgoingRequest<R>) => Effect.Effect<SaveResult<R>, PersistenceError | MalformedMessage>;
        /**
         * Save the provided message and its associated metadata.
         */
        readonly saveEnvelope: (envelope: Message.OutgoingEnvelope) => Effect.Effect<void, PersistenceError | MalformedMessage>;
        /**
         * Save the provided `Reply` and its associated metadata.
         */
        readonly saveReply: <R extends Rpc.Any>(reply: Reply.ReplyWithContext<R>) => Effect.Effect<void, PersistenceError | MalformedMessage>;
        /**
         * Clear replies for the given request id. When options.expectedReplyId is
         * provided, clear only if it is still the latest reply at the storage boundary.
         * Without it, clear unconditionally.
         * Custom storage implementations must honor this condition to protect
         * workflows from stale concurrent resumes.
         */
        readonly clearReplies: (requestId: Snowflake.Snowflake, options?: {
            readonly expectedReplyId?: Snowflake.Snowflake | undefined;
        }) => Effect.Effect<void, PersistenceError>;
        /**
         * Retrieves the replies for the specified requests.
         *
         * **Details**
         *
         * This returns:
         *
         * - Un-acknowledged chunk replies
         * - `WithExit` replies
         */
        readonly repliesFor: <R extends Rpc.Any>(requests: Iterable<Message.OutgoingRequest<R>>) => Effect.Effect<Array<Reply.Reply<R>>, PersistenceError | MalformedMessage>;
        /**
         * Retrieves the encoded replies for the specified request ids.
         */
        readonly repliesForUnfiltered: (requestIds: Iterable<Snowflake.Snowflake>) => Effect.Effect<Array<Reply.Encoded>, PersistenceError | MalformedMessage>;
        /**
         * Retrieves the request id for the specified primary key.
         */
        readonly requestIdForPrimaryKey: (options: {
            readonly address: EntityAddress;
            readonly tag: string;
            readonly id: string;
        }) => Effect.Effect<Option.Option<Snowflake.Snowflake>, PersistenceError>;
        /**
         * For locally sent messages, register a handler to process the replies.
         */
        readonly registerReplyHandler: <R extends Rpc.Any>(message: Message.OutgoingRequest<R> | Message.IncomingRequest<R>) => Effect.Effect<void, EntityNotAssignedToRunner>;
        /**
         * Unregister the reply handler for the specified message.
         */
        readonly unregisterReplyHandler: (requestId: Snowflake.Snowflake) => Effect.Effect<void>;
        /**
         * Unregister the reply handlers for the specified ShardId.
         *
         * By default the waiters are resumed with `EntityNotAssignedToRunner`, so
         * they can re-route the wait. With `interrupt: true` the waiters are
         * interrupted with a cluster-abandon annotation instead, for when the runner
         * is shutting down and the reply can never be observed here.
         */
        readonly unregisterShardReplyHandlers: (shardId: ShardId.ShardId, options?: {
            readonly interrupt?: boolean | undefined;
        }) => Effect.Effect<void>;
        /**
         * Retrieves the unprocessed messages for the specified shards.
         *
         * **Details**
         *
         * A message is unprocessed when:
         *
         * - Requests that have no `WithExit` replies or no unacknowledged chunk replies
         * - The latest `AckChunk` envelope
         * - All `Interrupt` envelopes for unprocessed requests
         *
         * The `limit` option bounds the number of messages returned in a single
         * read, and the `addresses` option restricts the read to the provided
         * entity addresses. Only the returned messages are claimed; other messages
         * stay eligible for later reads.
         */
        readonly unprocessedMessages: (shardIds: Iterable<ShardId.ShardId>, options?: {
            readonly limit?: number | undefined;
            readonly addresses?: ReadonlyArray<EntityAddress> | undefined;
        } | undefined) => Effect.Effect<Array<Message.Incoming<any>>, PersistenceError>;
        /**
         * Retrieves the unprocessed messages by id.
         */
        readonly unprocessedMessagesById: <R extends Rpc.Any>(messageIds: Iterable<Snowflake.Snowflake>) => Effect.Effect<Array<Message.Incoming<R>>, PersistenceError>;
        /**
         * Reset the mailbox state for the provided shards.
         */
        readonly resetShards: (shardIds: Iterable<ShardId.ShardId>) => Effect.Effect<void, PersistenceError>;
        /**
         * Reset the mailbox state for the provided address.
         */
        readonly resetAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
        /** Release request claims without changing replies or processed state. */
        readonly resetRequests: (requestIds: ReadonlyArray<Snowflake.Snowflake>) => Effect.Effect<void, PersistenceError>;
        /**
         * Reset the mailbox state for the provided addresses.
         */
        readonly resetAddresses: (addresses: ReadonlyArray<EntityAddress>) => Effect.Effect<void, PersistenceError>;
        /**
         * Clear all messages and replies for the provided address.
         */
        readonly clearAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
        /**
         * Used to wrap requests with transactions.
         */
        readonly withTransaction: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
    };
    readonly encoded: Encoded;
    readonly requests: Map<string, MemoryEntry>;
    readonly requestsByPrimaryKey: Map<string, MemoryEntry>;
    readonly unprocessed: Set<Envelope.Encoded>;
    readonly replyIds: Set<string>;
    readonly journal: Envelope.Encoded[];
    readonly cursors: WeakMap<{}, number>;
}> & {
    readonly make: Effect.Effect<{
        readonly storage: {
            /**
             * Save the provided message and its associated metadata.
             */
            readonly saveRequest: <R extends Rpc.Any>(envelope: Message.OutgoingRequest<R>) => Effect.Effect<SaveResult<R>, PersistenceError | MalformedMessage>;
            /**
             * Save the provided message and its associated metadata.
             */
            readonly saveEnvelope: (envelope: Message.OutgoingEnvelope) => Effect.Effect<void, PersistenceError | MalformedMessage>;
            /**
             * Save the provided `Reply` and its associated metadata.
             */
            readonly saveReply: <R extends Rpc.Any>(reply: Reply.ReplyWithContext<R>) => Effect.Effect<void, PersistenceError | MalformedMessage>;
            /**
             * Clear replies for the given request id. When options.expectedReplyId is
             * provided, clear only if it is still the latest reply at the storage boundary.
             * Without it, clear unconditionally.
             * Custom storage implementations must honor this condition to protect
             * workflows from stale concurrent resumes.
             */
            readonly clearReplies: (requestId: Snowflake.Snowflake, options?: {
                readonly expectedReplyId?: Snowflake.Snowflake | undefined;
            }) => Effect.Effect<void, PersistenceError>;
            /**
             * Retrieves the replies for the specified requests.
             *
             * **Details**
             *
             * This returns:
             *
             * - Un-acknowledged chunk replies
             * - `WithExit` replies
             */
            readonly repliesFor: <R extends Rpc.Any>(requests: Iterable<Message.OutgoingRequest<R>>) => Effect.Effect<Array<Reply.Reply<R>>, PersistenceError | MalformedMessage>;
            /**
             * Retrieves the encoded replies for the specified request ids.
             */
            readonly repliesForUnfiltered: (requestIds: Iterable<Snowflake.Snowflake>) => Effect.Effect<Array<Reply.Encoded>, PersistenceError | MalformedMessage>;
            /**
             * Retrieves the request id for the specified primary key.
             */
            readonly requestIdForPrimaryKey: (options: {
                readonly address: EntityAddress;
                readonly tag: string;
                readonly id: string;
            }) => Effect.Effect<Option.Option<Snowflake.Snowflake>, PersistenceError>;
            /**
             * For locally sent messages, register a handler to process the replies.
             */
            readonly registerReplyHandler: <R extends Rpc.Any>(message: Message.OutgoingRequest<R> | Message.IncomingRequest<R>) => Effect.Effect<void, EntityNotAssignedToRunner>;
            /**
             * Unregister the reply handler for the specified message.
             */
            readonly unregisterReplyHandler: (requestId: Snowflake.Snowflake) => Effect.Effect<void>;
            /**
             * Unregister the reply handlers for the specified ShardId.
             *
             * By default the waiters are resumed with `EntityNotAssignedToRunner`, so
             * they can re-route the wait. With `interrupt: true` the waiters are
             * interrupted with a cluster-abandon annotation instead, for when the runner
             * is shutting down and the reply can never be observed here.
             */
            readonly unregisterShardReplyHandlers: (shardId: ShardId.ShardId, options?: {
                readonly interrupt?: boolean | undefined;
            }) => Effect.Effect<void>;
            /**
             * Retrieves the unprocessed messages for the specified shards.
             *
             * **Details**
             *
             * A message is unprocessed when:
             *
             * - Requests that have no `WithExit` replies or no unacknowledged chunk replies
             * - The latest `AckChunk` envelope
             * - All `Interrupt` envelopes for unprocessed requests
             *
             * The `limit` option bounds the number of messages returned in a single
             * read, and the `addresses` option restricts the read to the provided
             * entity addresses. Only the returned messages are claimed; other messages
             * stay eligible for later reads.
             */
            readonly unprocessedMessages: (shardIds: Iterable<ShardId.ShardId>, options?: {
                readonly limit?: number | undefined;
                readonly addresses?: ReadonlyArray<EntityAddress> | undefined;
            } | undefined) => Effect.Effect<Array<Message.Incoming<any>>, PersistenceError>;
            /**
             * Retrieves the unprocessed messages by id.
             */
            readonly unprocessedMessagesById: <R extends Rpc.Any>(messageIds: Iterable<Snowflake.Snowflake>) => Effect.Effect<Array<Message.Incoming<R>>, PersistenceError>;
            /**
             * Reset the mailbox state for the provided shards.
             */
            readonly resetShards: (shardIds: Iterable<ShardId.ShardId>) => Effect.Effect<void, PersistenceError>;
            /**
             * Reset the mailbox state for the provided address.
             */
            readonly resetAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
            /** Release request claims without changing replies or processed state. */
            readonly resetRequests: (requestIds: ReadonlyArray<Snowflake.Snowflake>) => Effect.Effect<void, PersistenceError>;
            /**
             * Reset the mailbox state for the provided addresses.
             */
            readonly resetAddresses: (addresses: ReadonlyArray<EntityAddress>) => Effect.Effect<void, PersistenceError>;
            /**
             * Clear all messages and replies for the provided address.
             */
            readonly clearAddress: (address: EntityAddress) => Effect.Effect<void, PersistenceError>;
            /**
             * Used to wrap requests with transactions.
             */
            readonly withTransaction: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
        };
        readonly encoded: Encoded;
        readonly requests: Map<string, MemoryEntry>;
        readonly requestsByPrimaryKey: Map<string, MemoryEntry>;
        readonly unprocessed: Set<Envelope.Encoded>;
        readonly replyIds: Set<string>;
        readonly journal: Envelope.Encoded[];
        readonly cursors: WeakMap<{}, number>;
    }, never, Snowflake.Generator>;
};
/**
 * Service that provides an in-memory message storage driver with inspectable backing state.
 *
 * **Details**
 *
 * It provides a `MessageStorage` service, the encoded driver implementation, and
 * maps used to track requests, primary keys, unprocessed envelopes, reply IDs,
 * and the journal.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class MemoryDriver extends MemoryDriver_base {
    /**
     * Layer that provides the in-memory message storage driver.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static readonly layer: Layer.Layer<MemoryDriver>;
}
/**
 * Layer that provides the no-op `MessageStorage` service.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerNoop: Layer.Layer<MessageStorage>;
/**
 * Layer that provides in-memory message storage and its backing `MemoryDriver`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerMemory: Layer.Layer<MessageStorage | MemoryDriver, never, ShardingConfig>;
export {};
//# sourceMappingURL=MessageStorage.d.ts.map