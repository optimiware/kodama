/**
 * Process-local invalidation for connecting writes to dependent reads.
 *
 * This module does not cache values itself. It lets callers register handlers
 * for keys, invalidate those keys, wrap successful mutations so they invalidate
 * keys, and expose effects as queues or streams that rerun when matching keys
 * change. The service can also batch invalidations so handlers run after the
 * batch completes.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as Queue from "../Queue.ts";
import type { ReadonlyRecord } from "../Record.ts";
import * as Scope from "../Scope.ts";
import * as Stream from "../Stream.ts";
/**
 * Brand type for `Reactivity`.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type TypeId = "~effect/reactivity/Reactivity";
/**
 * Brand for `Reactivity` implementations.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId: TypeId;
/**
 * Registers handlers and reruns queries when their keys are invalidated.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Reactivity {
    readonly [TypeId]: TypeId;
    readonly invalidateUnsafe: (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>) => void;
    readonly registerUnsafe: (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>, handler: () => void) => () => void;
    readonly invalidate: (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>) => Effect.Effect<void>;
    readonly mutation: <A, E, R>(keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>, effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
    readonly query: <A, E, R>(keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>, effect: Effect.Effect<A, E, R>) => Effect.Effect<Queue.Dequeue<A, E>, never, R | Scope.Scope>;
    readonly stream: <A, E, R>(keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>, effect: Effect.Effect<A, E, R>) => Stream.Stream<A, E, Exclude<R, Scope.Scope>>;
    readonly withBatch: <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R>;
}
/**
 * Service key for reactive invalidation.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const Reactivity: Context.Service<Reactivity, Reactivity>;
/**
 * Creates an in-memory `Reactivity` service.
 *
 * **Details**
 *
 * The service tracks handlers by hashed keys and runs the registered handlers when
 * matching keys are invalidated.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: Effect.Effect<Reactivity, never, never>;
/**
 * Wraps an effect so the supplied keys are invalidated after the effect succeeds.
 *
 * **Gotchas**
 *
 * If the effect fails, the keys are not invalidated.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const mutation: {
    /**
     * Wraps an effect so the supplied keys are invalidated after the effect succeeds.
     *
     * **Gotchas**
     *
     * If the effect fails, the keys are not invalidated.
     *
     * @stability unstable
     * @category accessors
     * @since 4.0.0
     */
    (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>): <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<A, E, R | Reactivity>;
    /**
     * Wraps an effect so the supplied keys are invalidated after the effect succeeds.
     *
     * **Gotchas**
     *
     * If the effect fails, the keys are not invalidated.
     *
     * @stability unstable
     * @category accessors
     * @since 4.0.0
     */
    <A, E, R>(effect: Effect.Effect<A, E, R>, keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>): Effect.Effect<A, E, R | Reactivity>;
};
/**
 * Runs an effect as a query tied to the supplied invalidation keys.
 *
 * **Details**
 *
 * The returned queue receives the initial result and each later result after the
 * keys are invalidated. The registration is removed when the current scope closes.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const query: {
    /**
     * Runs an effect as a query tied to the supplied invalidation keys.
     *
     * **Details**
     *
     * The returned queue receives the initial result and each later result after the
     * keys are invalidated. The registration is removed when the current scope closes.
     *
     * @stability unstable
     * @category accessors
     * @since 4.0.0
     */
    (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>): <A, E, R>(effect: Effect.Effect<A, E, R>) => Effect.Effect<Queue.Dequeue<A, E>, never, R | Scope.Scope | Reactivity>;
    /**
     * Runs an effect as a query tied to the supplied invalidation keys.
     *
     * **Details**
     *
     * The returned queue receives the initial result and each later result after the
     * keys are invalidated. The registration is removed when the current scope closes.
     *
     * @stability unstable
     * @category accessors
     * @since 4.0.0
     */
    <A, E, R>(effect: Effect.Effect<A, E, R>, keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>): Effect.Effect<Queue.Dequeue<A, E>, never, R | Scope.Scope | Reactivity>;
};
/**
 * Runs an effect as a stream of query results tied to the supplied invalidation
 * keys.
 *
 * **Details**
 *
 * The effect runs initially and reruns whenever the keys are invalidated.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const stream: {
    /**
     * Runs an effect as a stream of query results tied to the supplied invalidation
     * keys.
     *
     * **Details**
     *
     * The effect runs initially and reruns whenever the keys are invalidated.
     *
     * @stability unstable
     * @category accessors
     * @since 4.0.0
     */
    (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>): <A, E, R>(effect: Effect.Effect<A, E, R>) => Stream.Stream<A, E, Exclude<R, Scope.Scope> | Reactivity>;
    /**
     * Runs an effect as a stream of query results tied to the supplied invalidation
     * keys.
     *
     * **Details**
     *
     * The effect runs initially and reruns whenever the keys are invalidated.
     *
     * @stability unstable
     * @category accessors
     * @since 4.0.0
     */
    <A, E, R>(effect: Effect.Effect<A, E, R>, keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>): Stream.Stream<A, E, Exclude<R, Scope.Scope> | Reactivity>;
};
/**
 * Invalidates the supplied keys through the `Reactivity` service.
 *
 * **Details**
 *
 * Registered queries for matching keys are rerun immediately, or collected until
 * the enclosing reactivity batch completes.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const invalidate: (keys: ReadonlyArray<unknown> | ReadonlyRecord<string, ReadonlyArray<unknown>>) => Effect.Effect<void, never, Reactivity>;
/**
 * The default layer that provides an in-memory `Reactivity` service.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<Reactivity>;
//# sourceMappingURL=Reactivity.d.ts.map