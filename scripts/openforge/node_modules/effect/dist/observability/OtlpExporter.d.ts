import * as Context from "../Context.ts";
import * as Duration from "../Duration.ts";
import * as Effect from "../Effect.ts";
import * as Headers from "../http/Headers.ts";
import type { HttpBody } from "../http/HttpBody.ts";
import * as HttpClient from "../http/HttpClient.ts";
import * as Layer from "../Layer.ts";
import * as Scope from "../Scope.ts";
declare const Flusher_base: Context.ServiceClass<Flusher, "effect/observability/OtlpExporter/Flusher", {
    /**
     * Drains all registered exporters concurrently and cannot fail.
     *
     * **Details**
     *
     * There is no built-in timeout; use `Effect.timeoutOption` to bound the
     * operation. Exporters in their 60-second `disabledUntil` window are skipped.
     *
     * **Example** (Flushing exporters)
     *
     * ```ts import.meta.vitest
     * import { Effect } from "effect"
     * import { OtlpExporter } from "effect/observability"
     *
     * const program = Effect.gen(function*() {
     *   const flusher = yield* OtlpExporter.Flusher
     *   yield* flusher.flush
     *   return "flushed"
     * }).pipe(Effect.provide(OtlpExporter.layerFlusher))
     *
     * await Effect.runPromise(program) // => "flushed"
     * ```
     */
    readonly flush: Effect.Effect<void>;
    readonly register: (run: Effect.Effect<void>) => Effect.Effect<void, never, Scope.Scope>;
}>;
/**
 * Registry of exporter flush operations, used to manually drain buffered
 * telemetry before the surrounding scope closes.
 *
 * **Details**
 *
 * Every exporter created by `make` registers its export operation here, so a
 * single `flush` drains all signals sharing the registry. `flush` returns only
 * after the exports it initiated have settled, cannot fail, and respects each
 * exporter's temporary-disable window. Wrap it with `Effect.timeoutOption` to
 * bound its duration at the call site.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class Flusher extends Flusher_base {
}
/**
 * Provides a `Flusher` backed by a fresh registry.
 *
 * **Details**
 *
 * This is intentionally a single module-level constant rather than a factory:
 * layer memoization is keyed by layer instance, so every signal layer
 * referencing this same constant shares one registry per layer build, and one
 * `flush` drains traces, logs and metrics together. A factory returning a new
 * layer per call would silently create one registry per signal.
 *
 * Registration is scoped — an exporter is removed from the registry when its
 * own scope closes. Flushing with an empty registry is a no-op.
 *
 * Note that `flush` cannot await an export that was already in flight when it
 * was called (for example one started by the export interval); it only waits
 * for the exports it initiates.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layerFlusher: Layer.Layer<Flusher>;
/**
 * Creates a scoped OTLP batch exporter.
 *
 * **Details**
 *
 * The exporter buffers pushed data, periodically posts encoded batches to the
 * configured URL, retries transient failures, temporarily disables exporting
 * after unhandled failures, and flushes during scope finalization up to
 * `shutdownTimeout`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (options: {
    readonly url: string;
    readonly headers: Headers.Input | undefined;
    readonly label: string;
    readonly exportInterval: Duration.Input;
    readonly maxBatchSize: number | "disabled";
    readonly exportEmpty?: boolean | undefined;
    readonly body: (data: Array<any>) => readonly [body: HttpBody, onSuccess: Effect.Effect<void>];
    readonly shutdownTimeout: Duration.Input;
}) => Effect.Effect<{
    readonly push: (data: unknown) => void;
}, never, Flusher | HttpClient.HttpClient | Scope.Scope>;
export {};
//# sourceMappingURL=OtlpExporter.d.ts.map