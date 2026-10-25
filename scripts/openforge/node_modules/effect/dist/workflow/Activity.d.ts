/**
 * Defines named effects whose results can be stored by a workflow engine.
 *
 * An `Activity` is an `Effect` with a stable name and schemas for its success
 * and error values. `make` wraps an effect so the `WorkflowEngine` can execute
 * it, store its result, or replay that result during a workflow run. This module
 * also includes helpers for retry attempts, idempotency keys, and durable races.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type { NonEmptyReadonlyArray } from "../Array.ts";
import * as Cause from "../Cause.ts";
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Schedule from "../Schedule.ts";
import * as Schema from "../Schema.ts";
import type { Scope } from "../Scope.ts";
import type * as Types from "../Types.ts";
import type { WorkflowEngine, WorkflowInstance } from "./WorkflowEngine.ts";
declare const TypeId = "~effect/workflow/Activity";
/**
 * Durable workflow activity that behaves as an `Effect` and records its name,
 * result schemas, annotations, and encoded execution form for the workflow
 * engine.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Activity<Success extends Schema.Constraint = Schema.Void, Error extends Schema.Constraint = Schema.Never, R = never> extends Effect.Effect<Success["Type"], Error["Type"], Success["DecodingServices"] | Error["DecodingServices"] | R | WorkflowEngine | WorkflowInstance> {
    readonly [TypeId]: typeof TypeId;
    readonly name: string;
    readonly successSchema: Success;
    readonly errorSchema: Error;
    readonly exitSchema: Schema.Exit<Success, Error, Schema.Defect>;
    readonly exitSchemaPartial: Schema.Exit<Success, Error, Schema.Unknown>;
    readonly annotations: Context.Context<never>;
    annotate<I, S>(key: Context.Key<I, S>, value: S): Activity<Success, Error, R>;
    annotateMerge<I>(annotations: Context.Context<I>): Activity<Success, Error, R>;
    readonly execute: Effect.Effect<Success["Type"], Error["Type"], Success["DecodingServices"] | Success["EncodingServices"] | Error["DecodingServices"] | Error["EncodingServices"] | R | Scope | WorkflowEngine | WorkflowInstance>;
    readonly executeEncoded: Effect.Effect<unknown, unknown, Success["DecodingServices"] | Success["EncodingServices"] | Error["DecodingServices"] | Error["EncodingServices"] | R | Scope | WorkflowEngine | WorkflowInstance>;
}
/**
 * Type-erased activity shape for APIs that only need the activity identity,
 * name, annotations, and encoded execution.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Any {
    readonly [TypeId]: typeof TypeId;
    readonly name: string;
    readonly executeEncoded: Effect.Effect<any, any, any>;
    readonly annotations: Context.Context<never>;
}
/**
 * Type-erased activity shape that also exposes success and error schemas for
 * derived workflow APIs.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface AnyWithProps {
    readonly [TypeId]: typeof TypeId;
    readonly name: string;
    readonly successSchema: Schema.Top;
    readonly errorSchema: Schema.Top;
    readonly executeEncoded: Effect.Effect<any, any, any>;
}
/**
 * Creates a workflow activity from an effect, using the provided schemas to
 * encode successes and failures for durable execution.
 *
 * **Gotchas**
 *
 * Only completed activity results are memoized. If the activity suspends while
 * awaiting child workflows or a durable clock, its body runs again when the
 * parent workflow replays. Side effects before the suspension can repeat;
 * make those side effects idempotent.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <R, Success extends Schema.Constraint = Schema.Void, Error extends Schema.Constraint = Schema.Never>(options: {
    readonly name: string;
    readonly success?: Success | undefined;
    readonly error?: Error | undefined;
    readonly execute: Effect.Effect<Success["Type"], Error["Type"], R>;
    readonly interruptRetryPolicy?: Schedule.Schedule<any, Cause.Cause<unknown>> | undefined;
    readonly annotations?: Context.Context<never> | undefined;
}) => Activity<Success, Error, Exclude<R, WorkflowInstance | WorkflowEngine | Scope>>;
/**
 * Retries an effect with `Effect.retry` while updating `CurrentAttempt` for
 * each attempt.
 *
 * @stability unstable
 * @category error handling
 * @since 4.0.0
 */
export declare const retry: {
    /**
     * Retries an effect with `Effect.retry` while updating `CurrentAttempt` for
     * each attempt.
     *
     * @stability unstable
     * @category error handling
     * @since 4.0.0
     */
    <E, O extends Types.NoExcessProperties<Omit<Effect.Retry.Options<E>, "schedule">, O>>(options: O): <A, R>(self: Effect.Effect<A, E, R>) => Effect.Retry.Return<R, E, A, O>;
    /**
     * Retries an effect with `Effect.retry` while updating `CurrentAttempt` for
     * each attempt.
     *
     * @stability unstable
     * @category error handling
     * @since 4.0.0
     */
    <A, E, R, O extends Types.NoExcessProperties<Omit<Effect.Retry.Options<E>, "schedule">, O>>(self: Effect.Effect<A, E, R>, options: O): Effect.Retry.Return<R, E, A, O>;
};
/**
 * Context reference containing the current activity retry attempt, defaulting
 * to `1`.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const CurrentAttempt: Context.Reference<number>;
/**
 * Computes a deterministic activity idempotency key from the current workflow
 * execution ID, the supplied name, and optionally the current attempt.
 *
 * @stability unstable
 * @category idempotency
 * @since 4.0.0
 */
export declare const idempotencyKey: (name: string, options?: {
    readonly includeAttempt?: boolean | undefined;
} | undefined) => Effect.Effect<string, never, WorkflowInstance>;
/**
 * Runs a non-empty collection of activities as a durable race and returns the
 * first completed success or failure using unioned success and error schemas.
 *
 * @stability unstable
 * @category racing
 * @since 4.0.0
 */
export declare const raceAll: <const Activities extends NonEmptyReadonlyArray<Any>>(name: string, activities: Activities) => Effect.Effect<Activities[number] extends Activity<infer _A, infer _E, infer _R> ? _A["Type"] : never, Activities[number] extends Activity<infer _A, infer _E, infer _R> ? _E["Type"] : never, (Activities[number] extends Activity<infer Success, infer Error, infer R> ? Success["DecodingServices"] | Error["DecodingServices"] | R : never) | WorkflowEngine | WorkflowInstance>;
export {};
//# sourceMappingURL=Activity.d.ts.map