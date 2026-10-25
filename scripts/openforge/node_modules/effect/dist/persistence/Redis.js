/**
 * Redis support shared by persistence modules.
 *
 * This module defines a `Redis` service that can send Redis commands, subscribe
 * to pub/sub channels, and run Lua scripts. It does not create a Redis client
 * itself; callers provide the client-specific operations. The module also
 * provides helpers for describing Lua scripts, loading them once, and running
 * them later by their cached Redis id.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cache from "../Cache.js";
import * as Context from "../Context.js";
import * as Duration from "../Duration.js";
import * as Effect from "../Effect.js";
import * as Equal from "../Equal.js";
import * as Exit from "../Exit.js";
import { constant, identity } from "../Function.js";
import * as Hash from "../Hash.js";
import * as Queue from "../Queue.js";
import * as Schema from "../Schema.js";
import * as Scope from "../Scope.js";
/**
 * Service for sending Redis commands, subscribing to channels, and evaluating
 * cached Lua scripts.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class Redis extends /*#__PURE__*/Context.Service()("effect/persistence/Redis") {}
/**
 * Creates a `Redis` service from raw command and subscription operations.
 *
 * **Details**
 *
 * Lua scripts are loaded through `SCRIPT LOAD`, cached, and then invoked with
 * `EVALSHA`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = /*#__PURE__*/Effect.fnUntraced(function* (options) {
  const scriptCache = yield* Cache.makeWith(script => options.send("SCRIPT", "LOAD", script.lua), {
    capacity: Number.POSITIVE_INFINITY,
    timeToLive: exit => Exit.isSuccess(exit) ? Duration.infinity : Duration.zero
  });
  const eval_ = script => (...params) => {
    const evalSha = sha => options.send("EVALSHA", sha, script.numberOfKeys(...params).toString(), ...script.params(...params).map(param => String(param)));
    return Cache.get(scriptCache, script).pipe(Effect.flatMap(evalSha), Effect.catchIf(error => String(error.cause).includes("NOSCRIPT"), () => Cache.refresh(scriptCache, script).pipe(Effect.flatMap(evalSha))));
  };
  const subscribe = Effect.fnUntraced(function* (channel) {
    const queue = yield* Queue.unbounded();
    yield* Scope.addFinalizer(yield* Effect.scope, Queue.shutdown(queue));
    const onFailure = cause => Queue.failCause(queue, cause).pipe(Effect.asVoid);
    yield* options.subscribe(channel, message => {
      Queue.offerUnsafe(queue, message);
    }).pipe(Effect.flatMap(listen => Effect.forkScoped(Effect.catchCause(listen, onFailure))), Effect.catchCause(onFailure));
    return queue;
  });
  return identity({
    send: options.send,
    subscribe,
    eval: eval_
  });
});
const ErrorTypeId = "~effect/persistence/Redis/RedisError";
/**
 * Error raised by Redis command or script execution.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class RedisError extends /*#__PURE__*/Schema.Error(ErrorTypeId)({
  _tag: /*#__PURE__*/Schema.tag("RedisError"),
  cause: /*#__PURE__*/Schema.Defect()
}) {
  /**
   * Marks this value as a Redis persistence error for runtime guards.
   *
   * @stability unstable
   * @since 4.0.0
   */
  [ErrorTypeId] = ErrorTypeId;
}
const ScriptTypeId = "~effect/persistence/Redis/Script";
const ScriptProto = {
  [ScriptTypeId]: {
    params: identity,
    result: identity
  },
  withReturnType() {
    return this;
  },
  [Equal.symbol](that) {
    return this === that;
  },
  [Hash.symbol]() {
    return Hash.random(this);
  }
};
/**
 * Constructs a typed Redis Lua script descriptor.
 *
 * **Details**
 *
 * The result type defaults to `void` and can be refined with
 * `withReturnType`.
 *
 * @stability unstable
 * @category scripting
 * @since 4.0.0
 */
export const script = (f, options) => Object.setPrototypeOf({
  ...options,
  params: f,
  numberOfKeys: typeof options.numberOfKeys === "number" ? constant(options.numberOfKeys) : options.numberOfKeys
}, ScriptProto);
//# sourceMappingURL=Redis.js.map