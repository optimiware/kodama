import * as Context from "../../Context.js";
import * as Deferred from "../../Deferred.js";
import * as Effect from "../../Effect.js";
import * as Exit from "../../Exit.js";
import * as Fiber from "../../Fiber.js";
import * as MutableHashMap from "../../MutableHashMap.js";
import * as MutableRef from "../../MutableRef.js";
import * as Scope from "../../Scope.js";
/**
 * @internal
 */
export class ResourceMap {
  lookup;
  entries;
  isClosed;
  constructor(lookup, entries, isClosed) {
    this.lookup = lookup;
    this.entries = entries;
    this.isClosed = isClosed;
  }
  static make = /*#__PURE__*/Effect.fnUntraced(function* (lookup, options) {
    const scope = yield* Effect.scope;
    const services = yield* Effect.context();
    const isClosed = MutableRef.make(false);
    const entries = options?.referential ? {
      _tag: "Referential",
      map: new Map()
    } : {
      _tag: "Equal",
      map: MutableHashMap.empty()
    };
    yield* Scope.addFinalizerExit(scope, exit => {
      MutableRef.set(isClosed, true);
      return Effect.forEach(entries.map, ([key, {
        fiber,
        scope
      }]) => {
        backingDelete(entries, key);
        return Effect.exit(Effect.andThen(Fiber.interrupt(fiber), Scope.close(scope, exit)));
      }, {
        concurrency: "unbounded",
        discard: true
      });
    });
    return new ResourceMap((key, scope) => Effect.provide(lookup(key), Context.add(services, Scope.Scope, scope)), entries, isClosed);
  });
  hasUnsafe(key) {
    return backingGet(this.entries, key) !== undefined;
  }
  keysUnsafe() {
    return Array.from(this.entries.map, ([key]) => key);
  }
  get(key) {
    return Effect.uninterruptibleMask(restore => {
      if (MutableRef.get(this.isClosed)) {
        return Effect.interrupt;
      }
      const existing = backingGet(this.entries, key);
      if (existing) {
        return this.awaitEntry(key, existing, restore);
      }
      const entry = {
        scope: Scope.makeUnsafe(),
        deferred: Deferred.makeUnsafe(),
        fiber: undefined,
        awaiters: 0
      };
      backingSet(this.entries, key, entry);
      // Run the lookup on a detached fiber so it is shared by every caller and
      // only interrupted once all of them have been interrupted.
      entry.fiber = Effect.runForkWith(Fiber.getCurrent().context)(Effect.onExit(this.lookup(key, entry.scope), exit => {
        if (exit._tag === "Success") {
          return Deferred.done(entry.deferred, exit);
        }
        this.deleteEntry(key, entry);
        return Effect.andThen(Deferred.done(entry.deferred, exit), Scope.close(entry.scope, exit));
      }));
      return this.awaitEntry(key, entry, restore);
    });
  }
  awaitEntry(key, entry, restore) {
    if (Deferred.isDoneUnsafe(entry.deferred)) return Deferred.await(entry.deferred);
    entry.awaiters++;
    return Effect.onExit(restore(Deferred.await(entry.deferred)), () => {
      if (--entry.awaiters > 0 || Deferred.isDoneUnsafe(entry.deferred)) {
        return Effect.void;
      }
      // Detach the abandoned entry first so new callers start a fresh lookup
      // instead of joining one that is being interrupted.
      this.deleteEntry(key, entry);
      return Fiber.interrupt(entry.fiber);
    });
  }
  deleteEntry(key, entry) {
    // Never delete a replacement entry created after this one was removed.
    if (backingGet(this.entries, key) === entry) {
      backingDelete(this.entries, key);
    }
  }
  remove(key) {
    return Effect.suspend(() => {
      const entry = backingGet(this.entries, key);
      if (!entry) {
        return Effect.void;
      }
      backingDelete(this.entries, key);
      return Scope.close(entry.scope, Exit.void);
    });
  }
  removeIgnore(key) {
    return Effect.catchCause(this.remove(key), cause => Effect.annotateLogs(Effect.logDebug(cause), {
      module: "ResourceMap",
      method: "removeIgnore",
      key
    }));
  }
}
const backingGet = (map, key) => {
  if (map._tag === "Equal") {
    return MutableHashMap.get(map.map, key).valueOrUndefined;
  }
  return map.map.get(key);
};
const backingSet = (map, key, entry) => {
  if (map._tag === "Equal") {
    MutableHashMap.set(map.map, key, entry);
  } else {
    map.map.set(key, entry);
  }
};
const backingDelete = (map, key) => {
  if (map._tag === "Equal") {
    MutableHashMap.remove(map.map, key);
  } else {
    map.map.delete(key);
  }
};
//# sourceMappingURL=resourceMap.js.map