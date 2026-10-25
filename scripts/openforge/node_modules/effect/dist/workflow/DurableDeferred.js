/**
 * Defines named wait points for durable workflow executions.
 *
 * A `DurableDeferred` has a stable name and schemas for the value that will be
 * recorded later. Workflows can await it, suspend when no result exists yet, and
 * resume after its result is recorded. Tokens identify the workflow name,
 * execution id, and deferred name so external code can complete the correct
 * wait point later.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Cause from "../Cause.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Base64Url from "../encoding/Base64Url.js";
import * as Exit from "../Exit.js";
import * as Filter from "../Filter.js";
import { dual } from "../Function.js";
import * as Option from "../Option.js";
import * as Schema from "../Schema.js";
import * as SchemaGetter from "../SchemaGetter.js";
import * as Workflow from "./Workflow.js";
const TypeId = "~effect/workflow/DurableDeferred";
/**
 * Creates a named durable deferred with optional success and error schemas for
 * persisted completion.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (name, options) => {
  const successSchema = options?.success ?? Schema.Void;
  const errorSchema = options?.error ?? Schema.Never;
  return {
    [TypeId]: TypeId,
    name,
    successSchema,
    errorSchema,
    exitSchema: Schema.Exit(Schema.toCodecJson(successSchema), Schema.toCodecJson(errorSchema), Schema.toCodecJson(Schema.Defect())),
    withActivityAttempt: Effect.gen(function* () {
      const attempt = yield* CurrentAttempt;
      return make(`${name}/${attempt}`, {
        success: successSchema,
        error: errorSchema
      });
    })
  };
};
const EngineTag = /*#__PURE__*/Context.Service("effect/workflow/WorkflowEngine");
const InstanceTag = /*#__PURE__*/Context.Service("effect/workflow/WorkflowEngine/WorkflowInstance");
const CurrentAttempt = /*#__PURE__*/Context.Reference("effect/workflow/Activity/CurrentAttempt", {
  defaultValue: () => 1
});
const await_ = /*#__PURE__*/Effect.fnUntraced(function* (self) {
  const engine = yield* EngineTag;
  const instance = yield* InstanceTag;
  // Register before the read so any later completion can preempt the run.
  instance.awaitedDeferreds.add(self.name);
  const exit = yield* Workflow.wrapActivityResult(engine.deferredResult(self), Option.isNone);
  if (Option.isNone(exit)) {
    return yield* Workflow.suspend(instance);
  }
  return yield* exit.value;
});
/**
 * @stability unstable
 */
export {
/**
 * Waits for the durable deferred, suspending the current workflow when no
 * persisted completion is available.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
await_ as await };
/**
 * Runs an effect and records its exit into the durable deferred, resuming
 * workflows that are waiting on that deferred.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const into = /*#__PURE__*/dual(2, (effect, self) => Effect.contextWith(context => {
  const engine = Context.get(context, EngineTag);
  const parentInstance = Context.get(context, InstanceTag);
  const instance = {
    ...parentInstance
  };
  return Effect.onExit(Effect.provideService(effect, InstanceTag, instance), Effect.fnUntraced(function* (exit) {
    if (Exit.isFailure(exit)) {
      const [interrupts, reasons] = Arr.partition(exit.cause.reasons, Filter.fromPredicate(Cause.isInterruptReason));
      if (interrupts.length === exit.cause.reasons.length) {
        // An interrupt-only exit is never a result: the effect was
        // suspended, preempted or interrupted, so record nothing.
        if (instance.suspended) {
          parentInstance.suspended = true;
        }
        return;
      } else if (interrupts.length > 0) {
        exit = Exit.failCause(Cause.fromReasons(reasons));
      }
    }
    yield* engine.deferredDone(self, {
      workflowName: instance.workflow._tag,
      executionId: instance.executionId,
      deferredName: self.name,
      exit
    });
  }));
}));
/**
 * Runs effects as a durable race, returning a previously persisted result when
 * present or completing a named deferred with the first result.
 *
 * @stability unstable
 * @category racing
 * @since 4.0.0
 */
export const raceAll = options => {
  const deferred = make(`raceAll/${options.name}`, {
    success: options.success,
    error: options.error
  });
  return Effect.gen(function* () {
    const engine = yield* EngineTag;
    const exit = yield* engine.deferredResult(deferred);
    if (Option.isSome(exit)) {
      return yield* exit.value;
    }
    return yield* into(Effect.raceAll(options.effects), deferred);
  });
};
/**
 * Runtime brand identifier for durable deferred tokens.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TokenTypeId = "~effect/workflow/DurableDeferred/Token";
/**
 * Schema for branded durable deferred tokens.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Token = /*#__PURE__*/Schema.String.pipe(/*#__PURE__*/Schema.brand(TokenTypeId));
/**
 * Schema for a decoded durable deferred token containing the workflow
 * name, execution ID, and deferred name.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class TokenParsed extends /*#__PURE__*/Schema.Class("effect/workflow/DurableDeferred/TokenParsed")({
  workflowName: Schema.String,
  executionId: Schema.String,
  deferredName: Schema.String
}) {
  /**
   * Encodes the parsed workflow, execution, and deferred names back into a token.
   *
   * @stability unstable
   * @since 4.0.0
   */
  get asToken() {
    return Base64Url.encode(JSON.stringify([this.workflowName, this.executionId, this.deferredName]));
  }
  /**
   * Schema for decoding and encoding durable deferred tokens as strings.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static FromString = /*#__PURE__*/Schema.String.pipe(/*#__PURE__*/Schema.decodeTo(/*#__PURE__*/Schema.fromJsonString(/*#__PURE__*/Schema.Tuple([Schema.String, Schema.String, Schema.String])), {
    decode: /*#__PURE__*/SchemaGetter.decodeBase64UrlString(),
    encode: /*#__PURE__*/SchemaGetter.encodeBase64Url()
  }), /*#__PURE__*/Schema.decodeTo(TokenParsed, {
    decode: /*#__PURE__*/SchemaGetter.transform(([workflowName, executionId, deferredName]) => new TokenParsed({
      workflowName,
      executionId,
      deferredName
    })),
    encode: /*#__PURE__*/SchemaGetter.transform(parsed => [parsed.workflowName, parsed.executionId, parsed.deferredName])
  }));
  /**
   * Decodes a durable deferred token string into its parsed components.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static fromString = /*#__PURE__*/Schema.decodeSync(TokenParsed.FromString);
  /**
   * Encodes parsed durable deferred token components into a token string.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static encode = /*#__PURE__*/Schema.encodeSync(TokenParsed.FromString);
}
/**
 * Creates a token for a durable deferred using the current workflow instance's
 * workflow name and execution ID.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const token = /*#__PURE__*/Effect.fnUntraced(function* (self) {
  const instance = yield* InstanceTag;
  return tokenFromExecutionId(self, instance);
});
/**
 * Creates a durable deferred token from an explicit workflow, execution ID,
 * and deferred name.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const tokenFromExecutionId = /*#__PURE__*/dual(2, (self, options) => new TokenParsed({
  workflowName: options.workflow._tag,
  executionId: options.executionId,
  deferredName: self.name
}).asToken);
/**
 * Creates a durable deferred token by deriving the workflow execution ID from
 * the supplied workflow payload.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const tokenFromPayload = /*#__PURE__*/dual(2, (self, options) => Effect.map(options.workflow.executionId(options.payload), executionId => tokenFromExecutionId(self, {
  workflow: options.workflow,
  executionId
})));
/**
 * Completes the durable deferred identified by a token with the supplied exit,
 * encoding the result through the deferred schemas.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const done = /*#__PURE__*/dual(2, /*#__PURE__*/Effect.fnUntraced(function* (self, options) {
  const engine = yield* EngineTag;
  const token = TokenParsed.fromString(options.token);
  yield* engine.deferredDone(self, {
    workflowName: token.workflowName,
    executionId: token.executionId,
    deferredName: token.deferredName,
    exit: options.exit
  });
}));
/**
 * Completes the durable deferred identified by a token with a successful
 * value.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const succeed = /*#__PURE__*/dual(2, (self, options) => done(self, {
  token: options.token,
  exit: Exit.succeed(options.value)
}));
/**
 * Completes the durable deferred identified by a token with a typed failure.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const fail = /*#__PURE__*/dual(2, (self, options) => done(self, {
  token: options.token,
  exit: Exit.fail(options.error)
}));
/**
 * Completes the durable deferred identified by a token with a failure cause.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const failCause = /*#__PURE__*/dual(2, (self, options) => done(self, {
  token: options.token,
  exit: Exit.failCause(options.cause)
}));
//# sourceMappingURL=DurableDeferred.js.map