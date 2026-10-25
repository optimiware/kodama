import * as Context from "../Context.js";
import * as Duration from "../Duration.js";
import * as Effect from "../Effect.js";
import * as Exit from "../Exit.js";
import * as Fiber from "../Fiber.js";
import { identity } from "../Function.js";
import { pipeArguments } from "../Pipeable.js";
import * as Scope from "../Scope.js";
const TypeId = "~effect/RcRef";
const stateEmpty = {
  _tag: "Empty"
};
const stateClosed = {
  _tag: "Closed"
};
const variance = {
  _A: identity,
  _E: identity
};
class RcRefImpl {
  [TypeId] = variance;
  pipe() {
    return pipeArguments(this, arguments);
  }
  state = stateEmpty;
  acquire;
  context;
  scope;
  idleTimeToLive;
  constructor(acquire, context, scope, idleTimeToLive) {
    this.acquire = acquire;
    this.context = context;
    this.scope = scope;
    this.idleTimeToLive = idleTimeToLive;
  }
}
/** @internal */
export const make = options => Effect.withFiber(fiber => {
  const context = fiber.context;
  const scope = Context.get(context, Scope.Scope);
  const ref = new RcRefImpl(options.acquire, context, scope, options.idleTimeToLive !== undefined ? Duration.fromInputUnsafe(options.idleTimeToLive) : undefined);
  return Effect.as(Scope.addFinalizerExit(scope, () => {
    const state = ref.state;
    ref.state = stateClosed;
    if (state._tag === "Acquired") return Scope.close(state.scope, Exit.void);
    if (state._tag === "Acquiring") return Fiber.interrupt(state.fiber);
    return Effect.void;
  }), ref);
});
const getState = (self, restore) => {
  switch (self.state._tag) {
    case "Closed":
      {
        return Effect.interrupt;
      }
    case "Acquired":
      {
        const state = self.state;
        state.refCount++;
        return state.fiber ? Effect.as(Fiber.interrupt(state.fiber), state) : Effect.succeed(state);
      }
    case "Acquiring":
      {
        const state = self.state;
        state.awaiters++;
        return awaitAcquiring(self, state, restore);
      }
    case "Empty":
      {
        // The caller that starts the acquisition counts as its first awaiter.
        const acquiring = {
          _tag: "Acquiring",
          fiber: undefined,
          awaiters: 1,
          acquired: undefined
        };
        self.state = acquiring;
        const scope = Scope.makeUnsafe();
        // Acquire on a detached fiber so the acquisition is shared by every
        // caller and only interrupted once all of them have been interrupted.
        const acquire = Effect.provideContext(self.acquire, Context.add(self.context, Scope.Scope, scope)).pipe(Effect.flatMap(value => {
          // The ref was closed, or the acquisition was abandoned.
          if (self.state !== acquiring) {
            return Effect.interrupt;
          }
          const state = {
            _tag: "Acquired",
            value,
            scope,
            fiber: undefined,
            // Every caller still waiting holds a reference.
            refCount: acquiring.awaiters,
            invalidated: false
          };
          self.state = state;
          acquiring.acquired = state;
          return Effect.succeed(state);
        }), Effect.onExit(exit => {
          if (Exit.isSuccess(exit)) return Effect.void;
          if (self.state === acquiring) self.state = stateEmpty;
          return Scope.close(scope, exit);
        }));
        acquiring.fiber = Effect.runForkWith(Fiber.getCurrent().context)(acquire);
        return awaitAcquiring(self, acquiring, restore);
      }
  }
};
const awaitAcquiring = (self, acquiring, restore) => Effect.onExit(restore(Fiber.join(acquiring.fiber)), exit => {
  const acquired = acquiring.acquired;
  if (acquired !== undefined) {
    // The acquisition counted this caller as a reference, possibly before
    // its fiber exited. Release it if the caller was interrupted before it
    // could register its finalizer.
    return Exit.isFailure(exit) ? release(self, acquired) : Effect.void;
  }
  // Abandon the acquisition once every caller has left, unless it already
  // failed (a failed acquisition allocated no references).
  if (--acquiring.awaiters > 0 || self.state !== acquiring) return Effect.void;
  // Detach it first so new callers start a fresh acquisition instead of
  // joining one that is being interrupted.
  self.state = stateEmpty;
  return Fiber.interrupt(acquiring.fiber);
});
/** @internal */
export const get = self_ => {
  const self = self_;
  return Effect.uninterruptibleMask(restore => Effect.flatMap(Effect.scope, scope => Effect.flatMap(getState(self, restore), state => Effect.as(Scope.addFinalizerExit(scope, () => release(self, state)), state.value))));
};
const release = (self, state) => {
  state.refCount--;
  if (state.refCount > 0) {
    return Effect.void;
  }
  const idleTimeToLive = self.idleTimeToLive;
  if (idleTimeToLive === undefined || state.invalidated) {
    if (self.state === state) {
      self.state = stateEmpty;
    }
    return Scope.close(state.scope, Exit.void);
  } else if (!Duration.isFinite(idleTimeToLive)) {
    return Effect.void;
  }
  state.fiber = Effect.uninterruptibleMask(restore => Effect.flatMap(restore(Effect.sleep(idleTimeToLive)), () => {
    if (self.state === state && state.refCount === 0) {
      self.state = stateEmpty;
      return Scope.close(state.scope, Exit.void);
    }
    return Effect.void;
  })).pipe(Effect.ensuring(Effect.sync(() => {
    state.fiber = undefined;
  })), Effect.runForkWith(self.context), Fiber.runIn(self.scope));
  return Effect.void;
};
/** @internal */
export const invalidate = self_ => {
  const self = self_;
  return Effect.uninterruptible(Effect.suspend(() => {
    if (self.state._tag !== "Acquired") {
      return Effect.void;
    }
    const state = self.state;
    self.state = stateEmpty;
    state.invalidated = true;
    if (state.refCount > 0) {
      return Effect.void;
    }
    state.fiber?.interruptUnsafe();
    return Scope.close(state.scope, Exit.void);
  }));
};
//# sourceMappingURL=rcRef.js.map