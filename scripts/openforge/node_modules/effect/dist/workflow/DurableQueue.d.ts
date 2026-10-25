/**
 * Durable workflow queues delegate work to persisted background workers and
 * resume the waiting workflow with the worker result.
 *
 * A workflow calls `process` to encode a payload, offer it to a named
 * `PersistedQueue`, attach a `DurableDeferred` token, and suspend. A worker
 * created with `makeWorker` or `worker` takes the item, runs the handler, and
 * records the handler's `Exit` through that token so the original workflow can
 * continue with the typed success or error.
 *
 * Delivery is at-least-once: a crash between handler success and the
 * acknowledgement redelivers the item, so handlers must be idempotent. When an
 * item exhausts its persisted queue attempts it is dead-lettered: the
 * `DurableDeferred` never resolves and the workflow stays parked until the
 * item is requeued out of band, while the id-based de-duplication prevents
 * replays from resurrecting the failed item. Deployments should run
 * `PersistedQueue.layerCleanup` in one instance to prune old completed items.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
import * as PersistedQueue from "../persistence/PersistedQueue.ts";
import * as Schedule from "../Schedule.ts";
import * as Schema from "../Schema.ts";
import * as DurableDeferred from "./DurableDeferred.ts";
import type { WorkflowEngine, WorkflowInstance } from "./WorkflowEngine.ts";
/**
 * Type-level identifier used to recognize `DurableQueue` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export type TypeId = "~effect/workflow/DurableQueue";
/**
 * Runtime identifier attached to `DurableQueue` values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId: TypeId;
/**
 * Durable workflow queue definition containing a payload schema, idempotency
 * key, and deferred used to await worker results.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface DurableQueue<Payload extends Schema.Top, Success extends Schema.Top = Schema.Void, Error extends Schema.Top = Schema.Never> {
    readonly [TypeId]: TypeId;
    readonly name: string;
    readonly payloadSchema: Payload;
    readonly idempotencyKey: (payload: Payload["Type"]) => string;
    readonly deferred: DurableDeferred.DurableDeferred<Success, Error>;
}
/**
 * Creates a `DurableQueue` that waits for persisted items to finish processing
 * using a `DurableDeferred`.
 *
 * **Example** (Defining a durable queue with workers)
 *
 * ```ts import.meta.vitest
 * import { Effect, Layer, Schema } from "effect"
 * import { DurableQueue, Workflow } from "effect/workflow"
 *
 * // Define a DurableQueue that can be used to derive workers and offer items for
 * // processing.
 * const ApiQueue = DurableQueue.make({
 *   name: "ApiQueue",
 *   payload: {
 *     id: Schema.String
 *   },
 *   success: Schema.Void,
 *   error: Schema.Never,
 *   idempotencyKey(payload) {
 *     return payload.id
 *   }
 * })
 *
 * const MyWorkflow = Workflow.make("MyWorkflow", {
 *   payload: {
 *     id: Schema.String
 *   },
 *   idempotencyKey: ({ id }) => id
 * })
 *
 * const MyWorkflowLayer = MyWorkflow.toLayer(
 *   Effect.fnUntraced(function*() {
 *     // The workflow suspends until a worker completes this queue item.
 *     yield* DurableQueue.process(ApiQueue, { id: "api-call-1" })
 *     return "Workflow succeeded!"
 *   })
 * )
 *
 * const processed: Array<string> = []
 * const processApiCall = ({ id }: { readonly id: string }) => Effect.sync(() => processed.push(id))
 *
 * // Construct the worker layer without starting background workers in this example.
 * const ApiWorker = DurableQueue.worker(ApiQueue, processApiCall, {
 *   concurrency: 5
 * })
 *
 * const program = Effect.gen(function*() {
 *   // Exercise the finite handler directly instead of running a queue worker.
 *   yield* processApiCall({ id: "api-call-1" })
 *   return [Layer.isLayer(MyWorkflowLayer), Layer.isLayer(ApiWorker), processed] as const
 * })
 *
 * await Effect.runPromise(program) // => [true, true, ["api-call-1"]]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <Payload extends Schema.Top | Schema.Struct.Fields, Success extends Schema.Top = Schema.Void, Error extends Schema.Top = Schema.Never>(options: {
    readonly name: string;
    readonly payload: Payload;
    readonly idempotencyKey: (payload: Payload extends Schema.Struct.Fields ? Schema.Struct.Type<Payload> : Payload["Type"]) => string;
    readonly success?: Success | undefined;
    readonly error?: Error | undefined;
}) => DurableQueue<Payload extends Schema.Struct.Fields ? Schema.Struct<Payload> : Payload, Success, Error>;
/**
 * Adds an item to the queue and wait for a worker to process it.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export declare const process: <Payload extends Schema.Top, Success extends Schema.Top, Error extends Schema.Top>(self: DurableQueue<Payload, Success, Error>, payload: Payload["~type.make.in"], options?: {
    readonly retrySchedule?: Schedule.Schedule<any, PersistedQueue.PersistedQueueError> | undefined;
}) => Effect.Effect<Success["Type"], Error["Type"], WorkflowEngine | WorkflowInstance | PersistedQueue.PersistedQueueFactory | Payload["EncodingServices"] | Payload["DecodingServices"] | Success["DecodingServices"] | Error["DecodingServices"]>;
/**
 * Create a worker effect that processes items from the durable queue.
 *
 * @stability unstable
 * @category workers
 * @since 4.0.0
 */
export declare const makeWorker: <Payload extends Schema.Top, Success extends Schema.Top, Error extends Schema.Top, R>(self: DurableQueue<Payload, Success, Error>, f: (payload: Payload["Type"]) => Effect.Effect<Success["Type"], Error["Type"], R>, options?: {
    readonly concurrency?: number | undefined;
} | undefined) => Effect.Effect<never, never, WorkflowEngine | PersistedQueue.PersistedQueueFactory | R | Payload["EncodingServices"] | Payload["DecodingServices"] | Success["EncodingServices"] | Error["EncodingServices"]>;
/**
 * Create a layer that runs workers for the durable queue.
 *
 * @stability unstable
 * @category workers
 * @since 4.0.0
 */
export declare const worker: <Payload extends Schema.Top, Success extends Schema.Top, Error extends Schema.Top, R>(self: DurableQueue<Payload, Success, Error>, f: (payload: Payload["Type"]) => Effect.Effect<Success["Type"], Error["Type"], R>, options?: {
    readonly concurrency?: number | undefined;
} | undefined) => Layer.Layer<never, never, WorkflowEngine | PersistedQueue.PersistedQueueFactory | R | Payload["EncodingServices"] | Payload["DecodingServices"] | Success["EncodingServices"] | Error["EncodingServices"]>;
//# sourceMappingURL=DurableQueue.d.ts.map