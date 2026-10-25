import * as Cause from "../Cause.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Effectable from "../Effectable.js";
import { dual } from "../Function.js";
import * as Schedule from "../Schedule.js";
import * as Schema from "../Schema.js";
import * as DurableDeferred from "./DurableDeferred.js";
import { makeHashDigest } from "./internal/crypto.js";
import * as Workflow from "./Workflow.js";
const TypeId = "~effect/workflow/Activity";
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
export const make = options => {
  const successSchema = options.success ?? Schema.Void;
  const errorSchema = options.error ?? Schema.Never;
  const successSchemaJson = Schema.toCodecJson(successSchema);
  const errorSchemaJson = Schema.toCodecJson(errorSchema);
  // oxlint-disable-next-line prefer-const
  let execute;
  const executeWithoutInterrupt = retryOnInterrupt(options.name, options.interruptRetryPolicy)(options.execute);
  const self = {
    ...Effectable.Prototype({
      label: "Activity",
      evaluate(_) {
        return execute;
      }
    }),
    [TypeId]: TypeId,
    name: options.name,
    successSchema,
    errorSchema,
    exitSchema: Schema.Exit(successSchemaJson, errorSchemaJson, Schema.Defect()),
    exitSchemaPartial: Schema.Exit(successSchemaJson, errorSchemaJson, Schema.Unknown),
    annotations: options.annotations ?? Context.empty(),
    annotate(tag, value) {
      return make({
        ...options,
        annotations: Context.add(self.annotations, tag, value)
      });
    },
    annotateMerge(context) {
      return make({
        ...options,
        annotations: Context.merge(self.annotations, context)
      });
    },
    execute: executeWithoutInterrupt,
    executeEncoded: Effect.matchEffect(executeWithoutInterrupt, {
      onFailure: error => Effect.flatMap(Effect.orDie(Schema.encodeEffect(errorSchemaJson)(error)), Effect.fail),
      onSuccess: value => Effect.orDie(Schema.encodeEffect(successSchemaJson)(value))
    })
  };
  execute = makeExecute(self);
  return self;
};
const interruptRetryPolicy = /*#__PURE__*/Schedule.min([Schedule.exponential(400, 1.5), Schedule.spaced("10 seconds")]).pipe(/*#__PURE__*/Schedule.setInputType(), /*#__PURE__*/Schedule.while(meta => meta.attempt <= 10 && Cause.hasInterrupts(meta.input)));
const retryOnInterrupt = (name, policy = interruptRetryPolicy) => effect => Effect.flatMap(InstanceTag, instance => effect.pipe(Effect.sandbox,
// A suspension interrupts the activity body on purpose and must surface
// as a suspended result instead of being retried.
Effect.retry({
  schedule: policy,
  while: () => !instance.suspended
}), Effect.catch(cause => Cause.hasInterrupts(cause) && !instance.suspended ? Effect.die(`Activity "${name}" interrupted and retry attempts exhausted`) : Effect.failCause(cause))));
/**
 * Retries an effect with `Effect.retry` while updating `CurrentAttempt` for
 * each attempt.
 *
 * @stability unstable
 * @category error handling
 * @since 4.0.0
 */
export const retry = /*#__PURE__*/dual(2, (effect, options) => Effect.suspend(() => {
  let attempt = 1;
  return Effect.suspend(() => Effect.provideService(effect, CurrentAttempt, attempt++)).pipe(Effect.retry(options));
}));
/**
 * Context reference containing the current activity retry attempt, defaulting
 * to `1`.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const CurrentAttempt = /*#__PURE__*/Context.Reference("effect/workflow/Activity/CurrentAttempt", {
  defaultValue: () => 1
});
/**
 * Computes a deterministic activity idempotency key from the current workflow
 * execution ID, the supplied name, and optionally the current attempt.
 *
 * @stability unstable
 * @category idempotency
 * @since 4.0.0
 */
export const idempotencyKey = /*#__PURE__*/Effect.fnUntraced(function* (name, options) {
  const instance = yield* InstanceTag;
  let key = `${instance.executionId}`;
  if (options?.includeAttempt) {
    const attempt = yield* CurrentAttempt;
    key += `-${attempt}`;
  }
  key += `-${name}`;
  return yield* makeHashDigest(key);
});
/**
 * Runs a non-empty collection of activities as a durable race and returns the
 * first completed success or failure using unioned success and error schemas.
 *
 * @stability unstable
 * @category racing
 * @since 4.0.0
 */
export const raceAll = (name, activities) => DurableDeferred.raceAll({
  name: `Activity/${name}`,
  success: Schema.Union(activities.map(activity => activity.successSchema)),
  error: Schema.Union(activities.map(activity => activity.errorSchema)),
  effects: activities.map(activity => activity)
});
// -----------------------------------------------------------------------------
// internal
// -----------------------------------------------------------------------------
const EngineTag = /*#__PURE__*/Context.Service("effect/workflow/WorkflowEngine");
const InstanceTag = /*#__PURE__*/Context.Service("effect/workflow/WorkflowEngine/WorkflowInstance");
const makeExecute = /*#__PURE__*/Effect.fnUntraced(function* (activity) {
  const engine = yield* EngineTag;
  const instance = yield* InstanceTag;
  const attempt = yield* CurrentAttempt;
  yield* Effect.annotateCurrentSpan({
    executionId: instance.executionId
  });
  const result = yield* Workflow.wrapActivityResult(engine.activityExecute(activity, attempt), _ => _._tag === "Suspended");
  if (result._tag === "Suspended") {
    return yield* Workflow.suspend(instance);
  }
  return yield* result.exit;
}, (effect, activity) => Effect.withSpan(effect, activity.name, {
  captureStackTrace: false
}));
//# sourceMappingURL=Activity.js.map