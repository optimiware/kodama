import * as Deferred from "../../Deferred.js";
import * as Effect from "../../Effect.js";
import * as Exit from "../../Exit.js";
import * as Fiber from "../../Fiber.js";
import * as Scope from "../../Scope.js";
import { aroundEntity } from "./interruptors.js";
/**
 * A replaceable resource whose acquired value identifies its generation.
 *
 * @internal
 */
export class ResourceRef {
  static from = /*#__PURE__*/Effect.fnUntraced(function* (parentScope, acquire, teardownAddress) {
    // Close the ref before joining releases, so waiters wake even if release stalls.
    const releaseScope = Scope.forkUnsafe(parentScope);
    const ref = new ResourceRef(releaseScope, acquire, teardownAddress);
    yield* Scope.addFinalizerExit(parentScope, exit => ref.close(exit));
    const pending = ref.pending(undefined, Deferred.makeUnsafe());
    const value = yield* acquire(pending.scope);
    if (ref.state === pending) ref.publish(pending, value);
    return ref;
  });
  state = {
    _tag: "Closed"
  };
  releaseScope;
  acquire;
  teardownAddress;
  constructor(releaseScope, acquire, teardownAddress) {
    this.releaseScope = releaseScope;
    this.acquire = acquire;
    this.teardownAddress = teardownAddress;
  }
  /** Returns the value only after acquisition and preparation complete. */
  getUnsafe() {
    return this.state._tag === "Ready" ? this.state.value : undefined;
  }
  /** Waits for a ready value; interrupts when the ref closes. */
  await = /*#__PURE__*/Effect.suspend(() => {
    const s = this.state;
    switch (s._tag) {
      case "Closed":
        return Effect.interrupt;
      case "Ready":
        return Effect.succeed(s.value);
      case "Pending":
      case "Failed":
        return Deferred.await(s.ready);
    }
  });
  /**
   * Replaces the generation, holding waiters until acquisition and `prepare` finish.
   * Returns `undefined` if closed, if `from` differs from the ready or preparing
   * value, or if `from` is omitted while a rebuild is pending.
   *
   * Acceptance immediately blocks waiters; run the returned effect to release
   * the predecessor and build its replacement. Closing or superseding the ref
   * interrupts the rebuild. Failure wakes waiters with its cause; the next
   * rebuild must omit `from` and await cleanup. Interruption during predecessor
   * release instead carries waiters forward without awaiting that release again.
   */
  rebuildUnsafe(options) {
    const s = this.state;
    const from = options?.from;
    let pending;
    switch (s._tag) {
      case "Closed":
        return undefined;
      case "Ready":
        {
          if (from !== undefined && from !== s.value) return undefined;
          pending = this.pending(s.scope, Deferred.makeUnsafe());
          break;
        }
      case "Failed":
        {
          if (from !== undefined) return undefined;
          pending = this.pending(s.releasing, s.ready);
          break;
        }
      case "Pending":
        {
          if (from === undefined || s.value !== from) return undefined;
          s.work?.interruptUnsafe();
          pending = this.pending(s.scope, s.ready);
          break;
        }
    }
    return this.run(pending, options?.prepare);
  }
  pending(releases, ready) {
    const pending = {
      _tag: "Pending",
      scope: Scope.makeUnsafe(),
      releases,
      releasing: false,
      ready,
      value: undefined,
      work: undefined
    };
    this.state = pending;
    return pending;
  }
  publish(pending, value) {
    this.state = {
      _tag: "Ready",
      scope: pending.scope,
      value
    };
    Deferred.doneUnsafe(pending.ready, Exit.succeed(value));
  }
  run(pending, prepare) {
    const work = Effect.suspend(() => {
      if (this.state !== pending) return Effect.interrupt;
      const releases = pending.releases;
      if (!releases) return Effect.void;
      pending.releasing = true;
      return Effect.andThen(Fiber.isFiber(releases) ? Fiber.join(releases) : Effect.flatMap(this.release(releases), Fiber.join), Effect.sync(() => {
        pending.releasing = false;
      }));
    }).pipe(Effect.andThen(Effect.suspend(() => this.acquire(pending.scope))), Effect.tap(value => {
      if (this.state !== pending) return Effect.interrupt;
      pending.value = value;
      return prepare ? prepare(value) : Effect.void;
    }), Effect.flatMap(value => {
      if (this.state !== pending) return Effect.interrupt;
      this.publish(pending, value);
      return Effect.void;
    }), Effect.onExit(exit => {
      if (Exit.isSuccess(exit) || this.state !== pending) {
        return Effect.void;
      }
      if (pending.releasing) {
        // No resource was acquired. Carry waiters forward without rejoining the stalled release.
        this.state = {
          _tag: "Failed",
          ready: pending.ready,
          releasing: undefined
        };
        return Effect.void;
      }
      return Effect.map(this.release(pending.scope), releasing => {
        if (this.state !== pending) return;
        this.state = {
          _tag: "Failed",
          ready: Deferred.makeUnsafe(),
          releasing
        };
        Deferred.doneUnsafe(pending.ready, Exit.failCause(exit.cause));
      });
    }));
    return Effect.flatMap(Effect.forkChild(work, {
      startImmediately: true
    }), fiber => {
      pending.work = fiber;
      // Replaced or closed before the fiber handle existed.
      if (this.state !== pending) fiber.interruptUnsafe();
      return Fiber.join(fiber);
    });
  }
  // A stalled release must not prevent rebuild interruption.
  release(scope) {
    const close = Scope.close(scope, Exit.void);
    return Effect.forkIn(this.teardownAddress ? aroundEntity(this.teardownAddress, close) : close, this.releaseScope, {
      startImmediately: true
    });
  }
  close(exit) {
    const s = this.state;
    this.state = {
      _tag: "Closed"
    };
    switch (s._tag) {
      case "Closed":
        return Effect.void;
      case "Ready":
        return Scope.close(s.scope, exit);
      case "Failed":
        {
          Deferred.doneUnsafe(s.ready, Effect.interrupt);
          return Effect.void;
        }
      case "Pending":
        {
          Deferred.doneUnsafe(s.ready, Effect.interrupt);
          s.work?.interruptUnsafe();
          const close = Scope.close(s.scope, exit);
          return s.releases && !Fiber.isFiber(s.releases) ? Effect.andThen(Scope.close(s.releases, exit), close) : close;
        }
    }
  }
}
//# sourceMappingURL=resourceRef.js.map