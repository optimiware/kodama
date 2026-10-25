/**
 * Coordinates rate limits through shared persistent storage.
 *
 * The `RateLimiter` service consumes tokens for string keys using fixed-window
 * counters or token-bucket state. It can protect external APIs, enforce quotas,
 * or throttle workers across fibers and processes that share the same store.
 * This module includes helpers that fail when a limit is exceeded, return the
 * delay needed before continuing, or wrap an effect so it waits automatically.
 * It also defines the store service and in-memory or Redis-backed store layers.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Config from "../Config.ts";
import * as Context from "../Context.ts";
import * as Duration from "../Duration.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as Schema from "../Schema.ts";
import * as Redis from "./Redis.ts";
/**
 * Runtime type identifier for `RateLimiter` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId: TypeId;
/**
 * Type-level identifier used to brand `RateLimiter` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type TypeId = "~effect/persistence/RateLimiter";
/**
 * Service for consuming rate-limit tokens for a key using fixed-window or
 * token-bucket algorithms.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface RateLimiter {
    readonly [TypeId]: TypeId;
    readonly consume: (options: {
        readonly algorithm?: "fixed-window" | "token-bucket" | undefined;
        readonly onExceeded?: "delay" | "fail" | undefined;
        readonly window: Duration.Input;
        readonly limit: number;
        readonly key: string;
        readonly tokens?: number | undefined;
    }) => Effect.Effect<ConsumeResult, RateLimiterError>;
    readonly adaptiveConsume: (options: AdaptiveConsumeOptions) => Effect.Effect<AdaptiveConsumeResult, RateLimiterError>;
    readonly adaptiveFeedback: (options: AdaptiveFeedbackOptions) => Effect.Effect<void, RateLimiterError>;
}
/**
 * Service tag for persistent token-consumption services.
 *
 * **When to use**
 *
 * Use to access or provide rate-limit checks backed by fixed-window counters or
 * token-bucket state.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const RateLimiter: Context.Service<RateLimiter, RateLimiter>;
/**
 * Creates a `RateLimiter` from the current `RateLimiterStore`.
 *
 * **Details**
 *
 * The limiter supports fixed-window and token-bucket algorithms and either
 * fails or returns a delay when a limit is exceeded.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: Effect.Effect<RateLimiter, never, RateLimiterStore>;
/**
 * Provides `RateLimiter` using the current `RateLimiterStore`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<RateLimiter, never, RateLimiterStore>;
/**
 * Accesses a function that applies rate limiting to an effect.
 *
 * **Example** (Applying rate limits to effects)
 *
 * ```ts import.meta.vitest
 * import { Effect, Layer } from "effect"
 * import { RateLimiter } from "effect/persistence"
 *
 * const messages: Array<string> = []
 * const program = Effect.gen(function*() {
 *   // Access the `withLimiter` function from the RateLimiter module
 *   const withLimiter = yield* RateLimiter.makeWithRateLimiter
 *
 *   // Apply a rate limiter to an effect
 *   yield* Effect.sync(() => messages.push("Making a request with rate limiting")).pipe(
 *     withLimiter({
 *       key: "some-key",
 *       limit: 10,
 *       onExceeded: "delay",
 *       window: "5 seconds",
 *       algorithm: "fixed-window"
 *     })
 *   )
 * }).pipe(
 *   Effect.provide(RateLimiter.layer.pipe(Layer.provide(RateLimiter.layerStoreMemory)))
 * )
 *
 * await Effect.runPromise(program)
 * messages // => ["Making a request with rate limiting"]
 * ```
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const makeWithRateLimiter: Effect.Effect<((options: {
    readonly algorithm?: "fixed-window" | "token-bucket" | undefined;
    readonly onExceeded?: "delay" | "fail" | undefined;
    readonly window: Duration.Input;
    readonly limit: number;
    readonly key: string;
    readonly tokens?: number | undefined;
}) => <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E | RateLimiterError, R>), never, RateLimiter>;
/**
 * Sleeps when the rate limit is exceeded.
 *
 * **Example** (Sleeping until rate limit permits)
 *
 * ```ts import.meta.vitest
 * import { Effect, Layer } from "effect"
 * import { RateLimiter } from "effect/persistence"
 *
 * const program = Effect.gen(function*() {
 *   const limiter = yield* RateLimiter.RateLimiter
 *   const partiallyApplied = RateLimiter.sleep(limiter)
 *   const partial = yield* partiallyApplied({
 *     key: "partial",
 *     limit: 10,
 *     window: "5 seconds",
 *     algorithm: "fixed-window"
 *   })
 *   const direct = yield* RateLimiter.sleep(limiter, {
 *     key: "direct",
 *     limit: 10,
 *     window: "5 seconds",
 *     algorithm: "fixed-window"
 *   })
 *   return [partial.remaining, direct.remaining]
 * }).pipe(
 *   Effect.provide(RateLimiter.layer.pipe(Layer.provide(RateLimiter.layerStoreMemory)))
 * )
 *
 * await Effect.runPromise(program) // => [9, 9]
 * ```
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare function sleep(self: RateLimiter): (options: {
    readonly algorithm?: "fixed-window" | "token-bucket" | undefined;
    readonly window: Duration.Input;
    readonly limit: number;
    readonly key: string;
    readonly tokens?: number | undefined;
}) => Effect.Effect<ConsumeResult, RateLimiterError>;
/**
 * @stability unstable
 */
export declare function sleep(self: RateLimiter, options: {
    readonly algorithm?: "fixed-window" | "token-bucket" | undefined;
    readonly window: Duration.Input;
    readonly limit: number;
    readonly key: string;
    readonly tokens?: number | undefined;
}): Effect.Effect<ConsumeResult, RateLimiterError>;
/**
 * Runtime type identifier for `RateLimiterError`.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const ErrorTypeId: ErrorTypeId;
/**
 * Type-level identifier used to brand `RateLimiterError` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type ErrorTypeId = "~effect/persistence/RateLimiter/RateLimiterError";
declare const RateLimitExceeded_base: Schema.Class<RateLimitExceeded, Schema.Struct<{
    readonly _tag: Schema.tag<"RateLimitExceeded">;
    readonly retryAfter: Schema.DurationFromMillis;
    readonly key: Schema.String;
    readonly limit: Schema.Finite;
    readonly remaining: Schema.Finite;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error reason for a rate-limit check that exceeded the configured limit.
 *
 * **Details**
 *
 * Includes the affected key, limit, remaining token count, and retry delay.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class RateLimitExceeded extends RateLimitExceeded_base {
    /**
     * Public message used when the rate limiter rejects a request.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): string;
}
declare const RateLimitStoreError_base: Schema.Class<RateLimitStoreError, Schema.Struct<{
    readonly _tag: Schema.tag<"RateLimitStoreError">;
    readonly message: Schema.String;
    readonly cause: Schema.optional<Schema.Defect>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error reason for failures in the backing `RateLimiterStore`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class RateLimitStoreError extends RateLimitStoreError_base {
}
/**
 * Union of reasons carried by `RateLimiterError`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export type RateLimiterErrorReason = RateLimitExceeded | RateLimitStoreError;
/**
 * Schema for all reasons that can be carried by `RateLimiterError`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare const RateLimiterErrorReason: Schema.Union<[
    typeof RateLimitExceeded,
    typeof RateLimitStoreError
]>;
declare const RateLimiterError_base: Schema.Class<RateLimiterError, Schema.Struct<{
    readonly _tag: Schema.tag<"RateLimiterError">;
    readonly reason: Schema.Union<[typeof RateLimitExceeded, typeof RateLimitStoreError]>;
}>, import("../Cause.ts").YieldableError>;
/**
 * Error raised by rate limiter operations, wrapping a concrete failure
 * `reason`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class RateLimiterError extends RateLimiterError_base {
    constructor(props: {
        readonly reason: RateLimiterErrorReason;
    });
    /**
     * Marks this value as a rate limiter error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [ErrorTypeId]: ErrorTypeId;
    get message(): string;
}
/**
 * Metadata returned after consuming tokens from a rate limiter.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface ConsumeResult {
    /**
     * The amount of delay to wait before making the next request, when the rate
     * limiter is using the "delay" `onExceeded` strategy. It will be
     * Duration.zero if the request is allowed immediately.
     */
    readonly delay: Duration.Duration;
    /**
     * The maximum number of requests allowed in the current window.
     */
    readonly limit: number;
    /**
     * The number of remaining requests in the current window.
     */
    readonly remaining: number;
    /**
     * The time until the rate limit fully resets.
     *
     * **Details**
     *
     * For token buckets, accounts for elapsed refill time and reserved debt,
     * assuming no further consumption.
     */
    readonly resetAfter: Duration.Duration;
}
/**
 * Phase of adaptive rate limiting driven by server feedback.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type AdaptivePhase = "inactive" | "cooldown" | "learning" | "learned";
/**
 * Options for consuming tokens from the adaptive rate limiter store.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface AdaptiveConsumeOptions {
    /**
     * The rate-limit key.
     */
    readonly key: string;
    /**
     * The number of tokens to consume.
     */
    readonly tokens: number;
    /**
     * The fallback limit configured for the regular rate limiter.
     */
    readonly fallbackLimit: number;
    /**
     * The fallback window configured for the regular rate limiter.
     */
    readonly fallbackWindow: Duration.Duration;
}
/**
 * Metadata returned after consuming tokens from the adaptive rate limiter store.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface AdaptiveConsumeResult {
    /**
     * The amount of delay to wait before making the request.
     */
    readonly delay: Duration.Duration;
    /**
     * The adaptive state epoch used to correlate later response feedback.
     */
    readonly epoch: number;
    /**
     * The adaptive phase observed by this consume operation.
     */
    readonly phase: AdaptivePhase;
}
/**
 * Options for reporting response feedback to the adaptive rate limiter store.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface AdaptiveFeedbackOptions {
    /**
     * The rate-limit key.
     */
    readonly key: string;
    /**
     * The adaptive state epoch returned by `adaptiveConsume`.
     */
    readonly epoch: number;
    /**
     * The number of tokens consumed by the request.
     */
    readonly tokens: number;
    /**
     * The HTTP response status code.
     */
    readonly status: number;
    /**
     * The parsed `Retry-After` delay, when present.
     */
    readonly retryAfter: Duration.Duration | undefined;
}
declare const RateLimiterStore_base: Context.ServiceClass<RateLimiterStore, "effect/persistence/RateLimiter/RateLimiterStore", {
    /**
     * Returns the token count *after* taking the specified `tokens` and time to
     * live for the `key`.
     *
     * If `limit` is provided, the number of taken tokens will be capped at the
     * limit.
     *
     * In the case the limit is exceeded, the returned count will be greater
     * than the limit, but the TTL will not be updated.
     */
    readonly fixedWindow: (options: {
        readonly key: string;
        readonly tokens: number;
        readonly refillRate: Duration.Duration;
        readonly limit: number | undefined;
    }) => Effect.Effect<readonly [count: number, ttl: number], RateLimiterError>;
    /**
     * Refills the bucket for `key`, attempts to consume `tokens`, and returns
     * `[remaining, elapsedMillis]` from that single atomic operation.
     *
     * `remaining` is the token count after subtracting `tokens`. Fractional counts
     * must retain their numeric precision. A negative count is only persisted
     * when `allowOverflow` is true.
     *
     * `elapsedMillis` is the time since the current refill interval started, in
     * milliseconds (fractions preserved), always at least `0` and less than
     * `Duration.toMillis(refillRate)`. It is `0` when the bucket is at capacity
     * after refilling and before consuming. Otherwise the boundary advances by
     * whole refill intervals and the interval never restarts.
     */
    readonly tokenBucket: (options: {
        readonly key: string;
        readonly tokens: number;
        readonly limit: number;
        readonly refillRate: Duration.Duration;
        readonly allowOverflow: boolean;
    }) => Effect.Effect<readonly [remaining: number, elapsedMillis: number], RateLimiterError>;
    /**
     * Consumes tokens from the adaptive rate-limit state for the `key`.
     *
     * When the store has no adaptive state for the `key`, implementations
     * should return a zero delay with the inactive phase.
     */
    readonly adaptiveConsume: (options: AdaptiveConsumeOptions) => Effect.Effect<AdaptiveConsumeResult, RateLimiterError>;
    /**
     * Records response feedback for the adaptive rate-limit state.
     */
    readonly adaptiveFeedback: (options: AdaptiveFeedbackOptions) => Effect.Effect<void, RateLimiterError>;
}>;
/**
 * Defines the low-level backing store for rate-limit state.
 *
 * **When to use**
 *
 * Use to provide the shared counter storage and adaptive feedback state used by
 * persistent rate-limit checks.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class RateLimiterStore extends RateLimiterStore_base {
}
/**
 * Provides a process-local in-memory `RateLimiterStore`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerStoreMemory: Layer.Layer<RateLimiterStore>;
/**
 * Creates a Redis-backed `RateLimiterStore` using Lua scripts and the
 * configured key prefix.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeStoreRedis: (options?: {
    readonly prefix?: string | undefined;
} | undefined) => Effect.Effect<{
    /**
     * Returns the token count *after* taking the specified `tokens` and time to
     * live for the `key`.
     *
     * If `limit` is provided, the number of taken tokens will be capped at the
     * limit.
     *
     * In the case the limit is exceeded, the returned count will be greater
     * than the limit, but the TTL will not be updated.
     */
    readonly fixedWindow: (options: {
        readonly key: string;
        readonly tokens: number;
        readonly refillRate: Duration.Duration;
        readonly limit: number | undefined;
    }) => Effect.Effect<readonly [count: number, ttl: number], RateLimiterError>;
    /**
     * Refills the bucket for `key`, attempts to consume `tokens`, and returns
     * `[remaining, elapsedMillis]` from that single atomic operation.
     *
     * `remaining` is the token count after subtracting `tokens`. Fractional counts
     * must retain their numeric precision. A negative count is only persisted
     * when `allowOverflow` is true.
     *
     * `elapsedMillis` is the time since the current refill interval started, in
     * milliseconds (fractions preserved), always at least `0` and less than
     * `Duration.toMillis(refillRate)`. It is `0` when the bucket is at capacity
     * after refilling and before consuming. Otherwise the boundary advances by
     * whole refill intervals and the interval never restarts.
     */
    readonly tokenBucket: (options: {
        readonly key: string;
        readonly tokens: number;
        readonly limit: number;
        readonly refillRate: Duration.Duration;
        readonly allowOverflow: boolean;
    }) => Effect.Effect<readonly [remaining: number, elapsedMillis: number], RateLimiterError>;
    /**
     * Consumes tokens from the adaptive rate-limit state for the `key`.
     *
     * When the store has no adaptive state for the `key`, implementations
     * should return a zero delay with the inactive phase.
     */
    readonly adaptiveConsume: (options: AdaptiveConsumeOptions) => Effect.Effect<AdaptiveConsumeResult, RateLimiterError>;
    /**
     * Records response feedback for the adaptive rate-limit state.
     */
    readonly adaptiveFeedback: (options: AdaptiveFeedbackOptions) => Effect.Effect<void, RateLimiterError>;
}, never, Redis.Redis>;
/**
 * Provides a Redis-backed `RateLimiterStore` using `makeStoreRedis`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerStoreRedis: (options?: {
    readonly prefix?: string | undefined;
}) => Layer.Layer<RateLimiterStore, never, Redis.Redis>;
/**
 * Provides a Redis-backed `RateLimiterStore` from wrapped configuration
 * options.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerStoreRedisConfig: (options: Config.Wrap<{
    readonly prefix?: string | undefined;
}>) => Layer.Layer<RateLimiterStore, Config.ConfigError, Redis.Redis>;
export {};
//# sourceMappingURL=RateLimiter.d.ts.map