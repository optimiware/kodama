/**
 * Stores schema-encoded queue work in persistent storage.
 *
 * A `PersistedQueue<A>` keeps JSON-encoded values in a named queue and lets
 * workers take one value at a time inside a scoped processing window. It is
 * useful for durable handoffs, background jobs, outbox-style integrations, and
 * work that should retry across fibers, process restarts, or multiple workers.
 * This module includes a queue factory, store service, id-based de-duplication,
 * retry handling, and in-memory, Redis, and SQL-backed store layers.
 *
 * Delivery is at-least-once: a crash between handler success and the
 * acknowledgement redelivers the element, so handlers must be idempotent.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cause from "../Cause.ts";
import * as Context from "../Context.ts";
import * as Duration from "../Duration.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as Schedule from "../Schedule.ts";
import * as Schema from "../Schema.ts";
import * as Scope from "../Scope.ts";
import * as SqlClient from "../sql/SqlClient.ts";
import type { SqlError } from "../sql/SqlError.ts";
import * as Redis from "./Redis.ts";
/**
 * Runtime type identifier for `PersistedQueue` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId: TypeId;
/**
 * Type-level identifier used to brand `PersistedQueue` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type TypeId = "~effect/persistence/PersistedQueue";
/**
 * Persistent queue of schema-encoded values.
 *
 * **Details**
 *
 * `offer` enqueues values by id, and `take` processes one value at a time,
 * marking it complete on success or retrying it with the queue's retry
 * schedule until the maximum attempts is reached, after which it is marked as
 * failed.
 *
 * Delivery is at-least-once: a crash between handler success and the
 * acknowledgement redelivers the element, so handlers must be idempotent.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface PersistedQueue<in out A, out R = never> {
    readonly [TypeId]: TypeId;
    /**
     * Adds an element to the queue and returns the id of the enqueued element.
     *
     * **Details**
     *
     * If an element with the same id already exists in the queue, it will not be
     * added again. De-duplication survives completion until the id is removed by
     * `layerCleanup`.
     */
    readonly offer: (value: A, options?: {
        readonly id: string | undefined;
    }) => Effect.Effect<string, PersistedQueueError | Schema.SchemaError, R>;
    /**
     * Takes an element from the queue, waiting until one is available when the
     * queue is empty.
     *
     * **Details**
     *
     * If the returned effect succeeds, the element is marked as processed;
     * otherwise it will be retried with the queue's retry schedule until the
     * maximum attempts is reached, after which it is marked as failed.
     *
     * An attempt is counted when the element is claimed, so `attempts` in the
     * handler metadata is 1-based ("this is attempt 3"). A handler crash that
     * takes down the process still consumes an attempt.
     *
     * Elements that fail to decode with the queue's schema are marked as failed
     * immediately and the next element is taken instead, so schema decode errors
     * never surface from `take`.
     */
    readonly take: <XA, XE, XR>(f: (value: A, metadata: {
        readonly id: string;
        readonly attempts: number;
    }) => Effect.Effect<XA, XE, XR>) => Effect.Effect<XA, XE | PersistedQueueError, R | XR>;
}
declare const PersistedQueueFactory_base: Context.ServiceClass<PersistedQueueFactory, "effect/persistence/PersistedQueue/PersistedQueueFactory", {
    readonly make: <S extends Schema.Constraint>(options: {
        readonly name: string;
        readonly schema: S;
        readonly maxAttempts?: number | undefined;
        readonly retrySchedule?: Schedule.Schedule<any, number> | undefined;
    }) => Effect.Effect<PersistedQueue<S["Type"], S["EncodingServices"] | S["DecodingServices"]>>;
}>;
/**
 * Service for constructing named `PersistedQueue` instances from schemas.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class PersistedQueueFactory extends PersistedQueueFactory_base {
}
/**
 * Accesses `PersistedQueueFactory` to create a named persisted queue for a
 * schema.
 *
 * **Details**
 *
 * `maxAttempts` defaults to 10. `retrySchedule` controls the delay before a
 * failed element becomes visible again, and defaults to an exponential delay
 * starting at 1 second and capped at 5 minutes.
 *
 * The schedule's state is the element's persisted attempt count. On each
 * failure the schedule is replayed up to the current attempt, so delays keep
 * progressing even when consecutive retries run in different processes. The
 * schedule input is the attempt number.
 *
 * Replay simulates elapsed time from the sum of the computed delays.
 * Attempt-driven schedules are therefore exact, while wall-clock-anchored
 * schedules observe this idealized time. In particular,
 * `Schedule.upTo({ duration })` caps the summed delays rather than real time
 * since the original failure.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const make: <S extends Schema.Constraint>(options: {
    readonly name: string;
    readonly schema: S;
    readonly maxAttempts?: number | undefined;
    readonly retrySchedule?: Schedule.Schedule<any, number> | undefined;
}) => Effect.Effect<PersistedQueue<S["Type"], S["EncodingServices"] | S["DecodingServices"]>, never, PersistedQueueFactory>;
/**
 * Creates a `PersistedQueueFactory` from the current `PersistedQueueStore`.
 *
 * **Details**
 *
 * Values are encoded and decoded with the supplied schema, automatically
 * assigned an id when needed, and acknowledged or retried according to the
 * `take` handler's exit.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeFactory: Effect.Effect<{
    readonly make: <S extends Schema.Constraint>(options: {
        readonly name: string;
        readonly schema: S;
        readonly maxAttempts?: number | undefined;
        readonly retrySchedule?: Schedule.Schedule<any, number> | undefined;
    }) => Effect.Effect<PersistedQueue<S["Type"], S["EncodingServices"] | S["DecodingServices"]>>;
}, never, PersistedQueueStore>;
/**
 * Provides `PersistedQueueFactory` using the current `PersistedQueueStore`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<PersistedQueueFactory, never, PersistedQueueStore>;
/**
 * Runs `PersistedQueueStore.cleanup` on a schedule.
 *
 * **Details**
 *
 * Completed elements are retained for `timeToLive` (default 30 days) so
 * offer de-duplication keeps working across replays, then removed. Failed
 * elements are the dead-letter record and are kept forever unless
 * `failedTimeToLive` is set.
 *
 * Run this layer in one instance of a deployment rather than on every worker;
 * racing instances are harmless since deletes are idempotent, but the work is
 * redundant.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerCleanup: (options?: {
    readonly interval?: Duration.Input | undefined;
    readonly timeToLive?: Duration.Input | undefined;
    readonly failedTimeToLive?: Duration.Input | undefined;
}) => Layer.Layer<never, never, PersistedQueueStore>;
/**
 * Runtime type identifier for `PersistedQueueError`.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const ErrorTypeId: ErrorTypeId;
/**
 * Type-level identifier used to brand `PersistedQueueError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type ErrorTypeId = "~effect/persistence/PersistedQueue/PersistedQueueError";
declare const PersistedQueueError_base: Schema.Class<PersistedQueueError, Schema.Struct<{
    readonly _tag: Schema.tag<"PersistedQueueError">;
    readonly message: Schema.String;
    readonly cause: Schema.optional<Schema.Defect>;
}>, Cause.YieldableError>;
/**
 * Error raised by persisted queue store operations.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class PersistedQueueError extends PersistedQueueError_base {
    /**
     * Marks this value as a persisted queue error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ErrorTypeId]: ErrorTypeId;
}
declare const PersistedQueueStore_base: Context.ServiceClass<PersistedQueueStore, "effect/persistence/PersistedQueue/PersistedQueueStore", {
    readonly offer: (options: {
        readonly name: string;
        readonly id: string;
        readonly element: unknown;
        readonly isCustomId: boolean;
    }) => Effect.Effect<void, PersistedQueueError>;
    readonly take: (options: {
        readonly name: string;
        readonly maxAttempts: number;
        readonly retryDelay: (attempts: number) => Effect.Effect<Duration.Duration>;
    }) => Effect.Effect<{
        readonly id: string;
        readonly attempts: number;
        readonly element: unknown;
    }, PersistedQueueError, Scope.Scope>;
    /**
     * Removes completed elements older than `timeToLive`, together with their
     * de-duplication records. Failed elements are removed only when
     * `failedTimeToLive` is provided.
     */
    readonly cleanup: (options: {
        readonly timeToLive: Duration.Duration;
        readonly failedTimeToLive: Duration.Duration | undefined;
    }) => Effect.Effect<void, PersistedQueueError>;
}>;
/**
 * Defines the low-level backing store service used by `PersistedQueue`.
 *
 * **When to use**
 *
 * Use to provide the persistence backend that stores queued elements, scoped
 * takes, retry attempts, and acknowledgements.
 *
 * **Details**
 *
 * The store persists offered elements and returns taken elements in a scope so
 * the finalizer can complete or retry them based on the processing exit.
 *
 * Claiming an element counts an attempt, so the `attempts` returned by `take`
 * is 1-based. When the take scope closes with a success the element is marked
 * completed; a failure retries it according to `retryDelay` or marks it failed
 * once `maxAttempts` is exhausted; an interruption releases it without
 * counting the attempt.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class PersistedQueueStore extends PersistedQueueStore_base {
}
/**
 * Provides an in-memory `PersistedQueueStore`.
 *
 * **Details**
 *
 * The store is process-local and volatile; failed takes are requeued with the
 * queue's retry schedule until the configured maximum attempts is reached,
 * after which the element is marked as failed.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerStoreMemory: Layer.Layer<PersistedQueueStore>;
/**
 * Creates a Redis-backed `PersistedQueueStore`.
 *
 * **Details**
 *
 * The store uses Redis lists, hashes, and sorted sets with worker locks,
 * periodically refreshes locks while items are being processed, delays retried
 * items with the queue's retry schedule, and moves exhausted items to a failed
 * queue.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeStoreRedis: (options?: {
    readonly prefix?: string | undefined;
    readonly pollInterval?: Duration.Input | undefined;
    readonly lockRefreshInterval?: Duration.Input | undefined;
    readonly lockExpiration?: Duration.Input | undefined;
} | undefined) => Effect.Effect<{
    readonly offer: (options: {
        readonly name: string;
        readonly id: string;
        readonly element: unknown;
        readonly isCustomId: boolean;
    }) => Effect.Effect<void, PersistedQueueError>;
    readonly take: (options: {
        readonly name: string;
        readonly maxAttempts: number;
        readonly retryDelay: (attempts: number) => Effect.Effect<Duration.Duration>;
    }) => Effect.Effect<{
        readonly id: string;
        readonly attempts: number;
        readonly element: unknown;
    }, PersistedQueueError, Scope.Scope>;
    /**
     * Removes completed elements older than `timeToLive`, together with their
     * de-duplication records. Failed elements are removed only when
     * `failedTimeToLive` is provided.
     */
    readonly cleanup: (options: {
        readonly timeToLive: Duration.Duration;
        readonly failedTimeToLive: Duration.Duration | undefined;
    }) => Effect.Effect<void, PersistedQueueError>;
}, never, Redis.Redis | Scope.Scope>;
/**
 * Provides a Redis-backed `PersistedQueueStore` using `makeStoreRedis`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerStoreRedis: (options?: {
    readonly prefix?: string | undefined;
    readonly pollInterval?: Duration.Input | undefined;
    readonly lockRefreshInterval?: Duration.Input | undefined;
    readonly lockExpiration?: Duration.Input | undefined;
} | undefined) => Layer.Layer<PersistedQueueStore, never, Redis.Redis>;
/**
 * Creates a SQL-backed `PersistedQueueStore`.
 *
 * **Details**
 *
 * The store creates the queue table and indexes, acquires rows with
 * per-worker locks, refreshes active locks while scoped takes are running, and
 * retries or completes rows according to the processing exit.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeStoreSql: (options?: {
    readonly tableName?: string | undefined;
    readonly pollInterval?: Duration.Input | undefined;
    readonly lockRefreshInterval?: Duration.Input | undefined;
    readonly lockExpiration?: Duration.Input | undefined;
} | undefined) => Effect.Effect<PersistedQueueStore["Service"], SqlError, SqlClient.SqlClient | Scope.Scope>;
/**
 * Provides a SQL-backed `PersistedQueueStore` using `makeStoreSql`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerStoreSql: (options?: {
    readonly tableName?: string | undefined;
    readonly pollInterval?: Duration.Input | undefined;
    readonly lockRefreshInterval?: Duration.Input | undefined;
    readonly lockExpiration?: Duration.Input | undefined;
} | undefined) => Layer.Layer<PersistedQueueStore, SqlError, SqlClient.SqlClient>;
export {};
//# sourceMappingURL=PersistedQueue.d.ts.map