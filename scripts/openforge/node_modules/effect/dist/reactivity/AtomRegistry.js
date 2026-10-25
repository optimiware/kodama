/**
 * Stores and runs atoms for one reactive runtime.
 *
 * An `AtomRegistry` evaluates atoms, caches their current values, tracks
 * dependencies, applies writes and refreshes, manages subscriptions, and
 * disposes unused nodes. Each registry is independent, so the same atom can hold
 * different values in different registries. Serializable atom values can also be
 * preloaded before the first read.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Exit from "../Exit.js";
import * as Fiber from "../Fiber.js";
import { constVoid, dual } from "../Function.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
import { hasProperty } from "../Predicate.js";
import * as Queue from "../Queue.js";
import { MixedScheduler } from "../Scheduler.js";
import * as Scope from "../Scope.js";
import * as Stream from "../Stream.js";
import * as Result from "./AsyncResult.js";
/**
 * The runtime type id used to identify `AtomRegistry` services and values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = "~effect/reactivity/AtomRegistry";
/**
 * Returns `true` when the value has the `AtomRegistry` type id.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isAtomRegistry = u => hasProperty(u, TypeId);
/**
 * Creates an `AtomRegistry`.
 *
 * **Details**
 *
 * Options can preload initial atom values, provide a custom task scheduler,
 * configure timeout bucket resolution, and set a default idle time-to-live for
 * unused atoms.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = options => new RegistryImpl(options?.initialValues, options?.scheduleTask, options?.timeoutResolution, options?.defaultIdleTTL);
/**
 * Service tag for the active atom runtime cache.
 *
 * **When to use**
 *
 * Use to access or provide the registry that stores atom values,
 * dependencies, subscriptions, and disposal state for a reactive lifetime.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const AtomRegistry = /*#__PURE__*/Context.Service(TypeId);
/**
 * Creates a layer that provides an `AtomRegistry` configured with the supplied
 * options.
 *
 * **Details**
 *
 * The registry is disposed when the layer scope is finalized.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerOptions = options => Layer.effect(AtomRegistry, Effect.gen(function* () {
  const scope = yield* Effect.scope;
  const registry = make({
    ...options,
    scheduleTask: options?.scheduleTask
  });
  yield* Scope.addFinalizer(scope, Effect.sync(() => registry.dispose()));
  return registry;
}));
/**
 * The default layer that provides a fresh `AtomRegistry`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layer = /*#__PURE__*/layerOptions();
// -----------------------------------------------------------------------------
// conversions
// -----------------------------------------------------------------------------
/**
 * Converts an atom in this registry into a stream.
 *
 * **Details**
 *
 * The stream emits the current value immediately, emits subsequent changes, and
 * unsubscribes from the registry when the stream scope closes.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toStream = /*#__PURE__*/dual(2, (self, atom) => Stream.callback(queue => Effect.suspend(() => {
  const fiber = Fiber.getCurrent();
  const scope = Context.getUnsafe(fiber.context, Scope.Scope);
  const cancel = self.subscribe(atom, value => Queue.offerUnsafe(queue, value), {
    immediate: true
  });
  return Scope.addFinalizer(scope, Effect.sync(cancel));
})));
/**
 * Converts an `AsyncResult` atom in this registry into a stream of successful
 * values.
 *
 * **Details**
 *
 * Initial results are skipped, failures fail the stream with their cause, and
 * duplicate stream values are dropped with `Stream.changes`.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toStreamResult = /*#__PURE__*/dual(2, (self, atom) => toStream(self, atom).pipe(Stream.filter(Result.isNotInitial), Stream.mapEffect(result => result._tag === "Success" ? Effect.succeed(result.value) : Effect.failCause(result.cause)), Stream.changes));
/**
 * Reads an `AsyncResult` atom from this registry as an effect.
 *
 * **Details**
 *
 * The effect waits for the result to leave `Initial`, and also waits through
 * waiting results when `suspendOnWaiting` is enabled.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const getResult = /*#__PURE__*/dual(args => isAtomRegistry(args[0]), (self, atom, options) => {
  const suspendOnWaiting = options?.suspendOnWaiting ?? false;
  return Effect.callback(resume => {
    const result = self.get(atom);
    if (result._tag !== "Initial" && !(suspendOnWaiting && result.waiting)) {
      return resume(Result.toExit(result));
    }
    const cancel = self.subscribe(atom, value => {
      if (value._tag !== "Initial" && !(suspendOnWaiting && value.waiting)) {
        resume(Result.toExit(value));
        cancel();
      }
    });
    return Effect.sync(cancel);
  });
});
/**
 * Mounts an atom in this registry for the lifetime of the current scope.
 *
 * **Details**
 *
 * The atom is subscribed with a no-op listener and the subscription is released
 * when the scope finalizer runs.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const mount = /*#__PURE__*/dual(2, (self, atom) => Effect.acquireRelease(Effect.sync(() => self.mount(atom)), release => Effect.sync(release)));
// -----------------------------------------------------------------------------
// internal
// -----------------------------------------------------------------------------
const constImmediate = {
  immediate: true
};
const invoke = f => {
  f();
};
const noFailure = {};
// Runs `f` for every item, then rethrows the first failure. One failing step
// must not skip the others: those would leak a lifetime or miss an update.
function runAll(items, f) {
  let failure = noFailure;
  for (const item of items) {
    try {
      f(item);
    } catch (error) {
      if (failure === noFailure) failure = error;
    }
  }
  if (failure !== noFailure) throw failure;
}
const SerializableTypeId = "~effect-atom/atom/Atom/Serializable";
const atomKey = atom => SerializableTypeId in atom ? atom[SerializableTypeId].key : atom;
class RegistryImpl {
  [TypeId];
  timeoutResolution;
  defaultIdleTTL;
  scheduler;
  schedulerAsync;
  dispatcher;
  onNodeAdded;
  onNodeRemoved;
  constructor(initialValues, scheduleTask, timeoutResolution, defaultIdleTTL) {
    this[TypeId] = TypeId;
    this.scheduler = new MixedScheduler("sync", scheduleTask);
    this.schedulerAsync = new MixedScheduler("async", scheduleTask);
    this.dispatcher = this.schedulerAsync.makeDispatcher();
    this.defaultIdleTTL = defaultIdleTTL;
    if (timeoutResolution === undefined && defaultIdleTTL !== undefined) {
      this.timeoutResolution = Math.round(defaultIdleTTL / 2);
    } else {
      this.timeoutResolution = timeoutResolution ?? 1000;
    }
    if (initialValues !== undefined) {
      for (const [atom, value] of initialValues) {
        let target = atom;
        while (target.initialValueTarget) {
          target = target.initialValueTarget;
        }
        this.ensureNode(target).setInitialValue(value);
      }
    }
  }
  nodes = /*#__PURE__*/new Map();
  preloadedSerializable = /*#__PURE__*/new Map();
  timeoutBuckets = /*#__PURE__*/new Map();
  nodeTimeoutBucket = /*#__PURE__*/new Map();
  disposed = false;
  getNodes() {
    return this.nodes;
  }
  get(atom) {
    return this.ensureNode(atom).value();
  }
  set(atom, value) {
    atom.write(this.ensureNode(atom).writeContext, value);
  }
  setSerializable(key, encoded) {
    this.preloadedSerializable.set(key, encoded);
  }
  modify(atom, f) {
    const node = this.ensureNode(atom);
    const result = f(node.value());
    atom.write(node.writeContext, result[1]);
    return result[0];
  }
  update(atom, f) {
    const node = this.ensureNode(atom);
    atom.write(node.writeContext, f(node.value()));
  }
  refresh = atom => {
    if (atom.refresh !== undefined) {
      atom.refresh(this.refresh);
    } else {
      this.invalidateAtom(atom);
    }
  };
  subscribe(atom, f, options) {
    const node = this.ensureNode(atom);
    if (options?.immediate) {
      f(node.value());
    } else if (node.state === NodeState.stale) {
      // a listener hears the next change, which only reaches an up-to-date node
      node.value();
    }
    const remove = node.subscribe(function () {
      f(node._value);
    });
    return () => {
      remove();
      if (node.canBeRemoved) {
        this.scheduleNodeRemoval(node);
      }
    };
  }
  mount(atom) {
    return this.subscribe(atom, constVoid, constImmediate);
  }
  atomHasTtl(atom) {
    return !atom.keepAlive && atom.idleTTL !== 0 && (atom.idleTTL !== undefined || this.defaultIdleTTL !== undefined);
  }
  ensureNode(atom) {
    const key = atomKey(atom);
    let node = this.nodes.get(key);
    if (node === undefined) {
      node = this.createNode(atom);
      this.nodes.set(key, node);
      this.onNodeAdded?.(node);
    } else if (this.atomHasTtl(atom)) {
      this.removeNodeTimeout(node);
    }
    if (typeof key === "string" && this.preloadedSerializable.has(key)) {
      const encoded = this.preloadedSerializable.get(key);
      this.preloadedSerializable.delete(key);
      const decoded = atom[SerializableTypeId].decode(encoded);
      let target = atom;
      while (target.initialValueTarget) {
        target = target.initialValueTarget;
      }
      if (target === atom) {
        node.setValue(decoded);
      } else {
        this.ensureNode(target).setInitialValue(decoded);
      }
    }
    return node;
  }
  createNode(atom) {
    if (this.disposed) {
      throw new Error(`Cannot access Atom ${atom}: registry is disposed`);
    }
    if (!atom.keepAlive) {
      this.scheduleAtomRemoval(atom);
    }
    return new NodeImpl(this, atom);
  }
  invalidateAtom = atom => {
    this.ensureNode(atom).invalidate();
  };
  scheduleAtomRemoval(atom) {
    this.dispatcher.scheduleTask(() => {
      const node = this.nodes.get(atomKey(atom));
      if (node !== undefined && node.canBeRemoved) {
        this.removeNode(node);
      }
    }, 0);
  }
  scheduleNodeRemoval(node) {
    this.dispatcher.scheduleTask(() => {
      if (node.canBeRemoved) {
        this.removeNode(node);
      }
    }, 0);
  }
  removeNode(node) {
    if (this.atomHasTtl(node.atom)) {
      this.setNodeTimeout(node);
    } else {
      this.nodes.delete(atomKey(node.atom));
      node.remove();
      this.onNodeRemoved?.(node);
    }
  }
  setNodeTimeout(node) {
    if (this.nodeTimeoutBucket.has(node)) {
      return;
    }
    let idleTTL = node.atom.idleTTL ?? this.defaultIdleTTL;
    if (this.#currentSweepTTL !== null) {
      idleTTL -= this.#currentSweepTTL;
      if (idleTTL <= 0) {
        this.nodes.delete(atomKey(node.atom));
        node.remove();
        this.onNodeRemoved?.(node);
        return;
      }
    }
    const ttl = Math.ceil(idleTTL / this.timeoutResolution) * this.timeoutResolution;
    const timestamp = Date.now() + ttl;
    const bucket = timestamp - timestamp % this.timeoutResolution + this.timeoutResolution;
    let entry = this.timeoutBuckets.get(bucket);
    if (entry === undefined) {
      entry = [new Set(), setTimeout(() => this.sweepBucket(bucket), bucket - Date.now())];
      this.timeoutBuckets.set(bucket, entry);
    }
    entry[0].add(node);
    this.nodeTimeoutBucket.set(node, bucket);
  }
  removeNodeTimeout(node) {
    const bucket = this.nodeTimeoutBucket.get(node);
    if (bucket === undefined) return;
    this.nodeTimeoutBucket.delete(node);
    this.scheduleNodeRemoval(node);
    const [nodes, handle] = this.timeoutBuckets.get(bucket);
    nodes.delete(node);
    if (nodes.size === 0) {
      clearTimeout(handle);
      this.timeoutBuckets.delete(bucket);
    }
  }
  #currentSweepTTL = null;
  sweepBucket(bucket) {
    const nodes = this.timeoutBuckets.get(bucket)[0];
    this.timeoutBuckets.delete(bucket);
    runAll(nodes, this.removeIdleNode);
  }
  removeIdleNode = node => {
    this.nodeTimeoutBucket.delete(node);
    if (!node.canBeRemoved) return;
    this.nodes.delete(atomKey(node.atom));
    this.onNodeRemoved?.(node);
    this.#currentSweepTTL = node.atom.idleTTL ?? this.defaultIdleTTL;
    try {
      node.remove();
    } finally {
      this.#currentSweepTTL = null;
    }
  };
  reset() {
    this.timeoutBuckets.forEach(([, handle]) => clearTimeout(handle));
    this.timeoutBuckets.clear();
    this.nodeTimeoutBucket.clear();
    const nodes = Array.from(this.nodes.values());
    this.nodes.clear();
    // Mark all nodes before removing any, so a parent cannot be rescheduled for removal.
    for (const node of nodes) {
      node.state = NodeState.removed;
    }
    runAll(nodes, this.destroyNode);
  }
  destroyNode = node => {
    node.remove();
    this.onNodeRemoved?.(node);
  };
  dispose() {
    this.disposed = true;
    this.reset();
  }
}
const NodeFlags = {
  alive: 1,
  // 1 << 0
  initialized: 2,
  // 1 << 1,
  waitingForValue: 4 // 1 << 2
};
const NodeState = {
  uninitialized: NodeFlags.alive | NodeFlags.waitingForValue,
  stale: NodeFlags.alive | NodeFlags.initialized | NodeFlags.waitingForValue,
  valid: NodeFlags.alive | NodeFlags.initialized,
  removed: 0
};
class NodeImpl {
  constructor(registry, atom) {
    this.registry = registry;
    this.atom = atom;
    this.writeContext = new WriteContextImpl(registry, this);
  }
  registry;
  atom;
  state = NodeState.uninitialized;
  lifetime;
  writeContext;
  preserveInitialValueOnBuild = false;
  invalidatedDuringBuild = false;
  failed = false;
  // Every parent that holds this node as a child. A stale node keeps the
  // parents of its last build, so they are not released and their next change
  // still reaches it. A build drops the parents it did not read.
  parents = /*#__PURE__*/new Set();
  children = /*#__PURE__*/new Set();
  listeners = /*#__PURE__*/new Set();
  observedChildren = 0;
  currentState() {
    switch (this.state) {
      case NodeState.uninitialized:
        return "uninitialized";
      case NodeState.stale:
        return "stale";
      case NodeState.valid:
        return "valid";
      default:
        return "removed";
    }
  }
  get canBeRemoved() {
    return !this.atom.keepAlive && this.listeners.size === 0 && this.children.size === 0 && this.state !== 0;
  }
  get building() {
    return this.lifetime !== undefined && this.lifetime.reads !== undefined;
  }
  // An observed node is rebuilt as soon as it goes stale, because something
  // waits for its value without reading it. The count spares walking the
  // children on every invalidation.
  get isObserved() {
    return !this.atom.lazy || this.listeners.size > 0 || this.observedChildren > 0;
  }
  childObserved(delta) {
    const observed = this.isObserved;
    this.observedChildren += delta;
    if (observed !== this.isObserved) {
      this.observedChanged(delta);
    }
  }
  observedChanged(delta) {
    for (const parent of this.parents) {
      parent.childObserved(delta);
    }
  }
  _value = undefined;
  value() {
    // a build can be superseded by a nested one, and that one staled again
    // before the outer read returns
    while ((this.state & NodeFlags.waitingForValue) !== 0) {
      this.build();
    }
    return this._value;
  }
  build() {
    // a lifetime still attached to a node that needs a value belongs to a
    // build that failed, or to one that is running and is now superseded
    this.disposeLifetime();
    this.invalidatedDuringBuild = false;
    const lifetime = makeLifetime(this);
    this.lifetime = lifetime;
    let value;
    let read = false;
    try {
      value = this.atom.read(lifetime);
      read = true;
    } finally {
      const reads = lifetime.reads;
      lifetime.reads = undefined;
      if (this.lifetime === lifetime) {
        this.dropUnread(reads);
        if (!read) this.fail();
      }
    }
    if (this.lifetime === lifetime && (this.state & NodeFlags.waitingForValue) !== 0) {
      if (this.preserveInitialValueOnBuild) {
        this.preserveInitialValueOnBuild = false;
        this.failed = false;
        this.state = NodeState.valid;
      } else {
        this.setValue(value);
      }
    }
    if (lifetime.failure !== noFailure) throw lifetime.failure;
  }
  // A build that threw leaves its dependents holding, or reading, a value
  // this node no longer stands behind, so they go stale and rebuild when
  // pulled or observed. The read's error is the one reported.
  fail() {
    this.failed = true;
    try {
      this.invalidateChildren();
    } catch {
      // an observed dependent rebuilt and met the same failure
    }
  }
  readTracked(atom, lifetime) {
    const parent = this.registry.ensureNode(atom);
    // the dependency holds even when the parent cannot produce a value, so
    // that its recovery reaches this node; a build superseded while the
    // parent builds has the edge dropped by its replacement
    if (!this.parents.has(parent)) {
      this.parents.add(parent);
      parent.children.add(this);
      if (this.isObserved) {
        parent.childObserved(1);
      }
    }
    try {
      return parent.value();
    } catch (error) {
      // the parent may have got its value and failed only to update another
      // dependent: this build still takes the value, and rethrows once it is done
      if (lifetime.reads === undefined || parent.state !== NodeState.valid) throw error;
      if (lifetime.failure === noFailure) lifetime.failure = error;
      return parent._value;
    } finally {
      // read once the parent has answered: its commit while it is being
      // pulled is the value this build is about to receive
      if (lifetime.reads !== undefined && !lifetime.disposed) {
        lifetime.reads.add(parent);
      }
    }
  }
  dropUnread(reads) {
    if (reads.size === this.parents.size) return;
    const observed = this.isObserved;
    for (const parent of this.parents) {
      if (reads.has(parent)) continue;
      this.parents.delete(parent);
      parent.children.delete(this);
      if (observed) {
        parent.childObserved(-1);
      }
      if (parent.canBeRemoved) {
        this.registry.scheduleNodeRemoval(parent);
      }
    }
  }
  valueOption() {
    if ((this.state & NodeFlags.initialized) === 0) {
      return Option.none();
    }
    return Option.some(this._value);
  }
  setInitialValue(value) {
    if ((this.state & NodeFlags.initialized) === 0) {
      this.preserveInitialValueOnBuild = true;
      this.state = NodeState.stale;
      this._value = value;
      if (this.listeners.size > 0) {
        this.announce();
      }
      return;
    }
    this.setValue(value);
  }
  setValue(value) {
    // dependents that failed along with this node get another try
    const recovered = this.failed;
    this.failed = false;
    const initialized = (this.state & NodeFlags.initialized) !== 0;
    this.state = NodeState.valid;
    if (initialized && this.atom.equals(this._value, value)) {
      if (recovered) this.invalidateChildren();
      return;
    }
    this._value = value;
    let failure = noFailure;
    try {
      this.invalidateChildren();
    } catch (error) {
      failure = error;
    }
    // the value is committed, so its listeners hear of it even when a dependent failed
    if (this.listeners.size > 0) {
      try {
        this.announce();
      } catch (error) {
        if (failure === noFailure) failure = error;
      }
    }
    if (failure !== noFailure) throw failure;
  }
  announce() {
    if (batchState.phase === BatchPhase.collect) {
      batchState.notify.add(this);
    } else {
      this.notify();
    }
  }
  invalidate() {
    if (this.state === NodeState.valid) {
      this.state = NodeState.stale;
      try {
        this.disposeLifetime();
      } finally {
        this.propagate();
      }
    } else if (this.building && batchState.phase !== BatchPhase.collect) {
      // the running build read a parent that has changed since
      this.build();
    } else {
      this.propagate();
    }
  }
  propagate() {
    if (batchState.phase === BatchPhase.collect) {
      if (this.building) {
        this.invalidatedDuringBuild = true;
      }
      batchState.stale.push(this);
    } else if (this.isObserved) {
      this.value();
    } else {
      this.invalidateChildren();
    }
  }
  invalidateChildren() {
    if (this.children.size === 0) {
      return;
    }
    let failure = noFailure;
    // a snapshot: a rebuild adds and drops children
    for (const child of Array.from(this.children)) {
      if (!child.affectedBy(this)) continue;
      try {
        child.invalidate();
      } catch (error) {
        if (failure === noFailure) failure = error;
      }
    }
    if (failure !== noFailure) throw failure;
  }
  // Whether a change of the parent reaches this node: it holds the parent's
  // value, or a running build has already read it. A stale node is stale
  // already, and a build that has not read the parent yet will read the new
  // value. The exception is an observed node that is still stale: its rebuild
  // failed, and a parent that has a value again is its chance to recover.
  affectedBy(parent) {
    if (this.state === NodeState.valid) return true;
    const reads = this.lifetime?.reads;
    if (reads !== undefined) return reads.has(parent);
    return this.isObserved && parent.state === NodeState.valid;
  }
  notify() {
    if (batchState.phase === BatchPhase.commit) {
      batchState.notify.delete(this);
    }
    runAll(this.listeners, invoke);
  }
  disposeLifetime() {
    const lifetime = this.lifetime;
    if (lifetime !== undefined) {
      this.lifetime = undefined;
      lifetime.dispose();
    }
  }
  remove() {
    const observed = this.isObserved;
    this.state = NodeState.removed;
    this.listeners.clear();
    try {
      this.disposeLifetime();
    } finally {
      const parents = Array.from(this.parents);
      this.parents.clear();
      runAll(parents, parent => {
        parent.children.delete(this);
        if (observed) {
          parent.childObserved(-1);
        }
        if (parent.canBeRemoved) {
          this.registry.removeNode(parent);
        }
      });
    }
  }
  subscribe(listener) {
    const observed = this.isObserved;
    this.listeners.add(listener);
    if (!observed) {
      this.observedChanged(1);
    }
    return () => {
      if (this.listeners.delete(listener) && !this.isObserved) {
        this.observedChanged(-1);
      }
    };
  }
}
const LifetimeProto = {
  get registry() {
    return this.node.registry;
  },
  addFinalizer(f) {
    if (this.disposed) return f();
    this.finalizers ??= [];
    this.finalizers.push(f);
  },
  get(atom) {
    if (this.disposed) {
      return this.node.registry.get(atom);
    }
    return this.node.readTracked(atom, this);
  },
  result(atom, options) {
    if (this.disposed || this.isFn) {
      return this.resultOnce(atom, options);
    }
    const result = this.get(atom);
    if (options?.suspendOnWaiting && result.waiting) {
      return Effect.never;
    }
    switch (result._tag) {
      case "Initial":
        {
          return Effect.never;
        }
      case "Failure":
        {
          return Exit.failCause(result.cause);
        }
      case "Success":
        {
          return Effect.succeed(result.value);
        }
    }
  },
  resultOnce(atom, options) {
    return Effect.callback(resume => {
      const result = this.once(atom);
      if (result._tag !== "Initial" && !(options?.suspendOnWaiting && result.waiting)) {
        return resume(Result.toExit(result));
      }
      const cancel = this.node.registry.subscribe(atom, result => {
        if (result._tag === "Initial" || options?.suspendOnWaiting && result.waiting) return;
        cancel();
        resume(Result.toExit(result));
      }, {
        immediate: false
      });
      return Effect.sync(cancel);
    });
  },
  setResult(atom, value) {
    if (this.disposed) return Effect.never;
    this.node.registry.set(atom, value);
    return this.resultOnce(atom, {
      suspendOnWaiting: true
    });
  },
  some(atom) {
    if (this.disposed || this.isFn) {
      return this.someOnce(atom);
    }
    const result = this.get(atom);
    return result._tag === "None" ? Effect.never : Effect.succeed(result.value);
  },
  someOnce(atom) {
    return Effect.callback(resume => {
      const result = this.once(atom);
      if (Option.isSome(result)) {
        return resume(Effect.succeed(result.value));
      }
      const cancel = this.node.registry.subscribe(atom, result => {
        if (Option.isNone(result)) return;
        cancel();
        resume(Effect.succeed(result.value));
      }, {
        immediate: false
      });
      return Effect.sync(cancel);
    });
  },
  once(atom) {
    return this.node.registry.get(atom);
  },
  self() {
    if (this.disposed) return Option.none();
    return this.node.valueOption();
  },
  refresh(atom) {
    if (this.disposed) return;
    this.node.registry.refresh(atom);
  },
  refreshSelf() {
    if (this.disposed) return;
    this.node.invalidate();
  },
  mount(atom) {
    if (this.disposed) return;
    this.addFinalizer(this.node.registry.mount(atom));
  },
  subscribe(atom, f, options) {
    if (this.disposed) return;
    this.addFinalizer(this.node.registry.subscribe(atom, f, options));
  },
  setSelf(a) {
    if (this.disposed) return;
    this.node.setValue(a);
  },
  set(atom, value) {
    if (this.disposed) return;
    this.node.registry.set(atom, value);
  },
  stream(atom, options) {
    if (this.disposed) return Stream.empty;
    return Stream.callback(queue => Effect.sync(() => {
      this.subscribe(atom, value => Queue.offerUnsafe(queue, value), {
        immediate: !options?.withoutInitialValue
      });
    }));
  },
  streamResult(atom, options) {
    return this.stream(atom, options).pipe(Stream.filter(Result.isNotInitial), Stream.mapEffect(result => result._tag === "Success" ? Effect.succeed(result.value) : Effect.failCause(result.cause)));
  },
  dispose() {
    this.disposed = true;
    if (this.finalizers === undefined) {
      return;
    }
    const finalizers = this.finalizers;
    this.finalizers = undefined;
    runAll(finalizers.reverse(), invoke);
  }
};
const makeLifetime = node => {
  function get(atom) {
    if (get.disposed || get.isFn) {
      return node.registry.get(atom);
    }
    return node.readTracked(atom, get);
  }
  Object.setPrototypeOf(get, LifetimeProto);
  get.isFn = false;
  get.disposed = false;
  get.finalizers = undefined;
  get.node = node;
  get.reads = new Set();
  get.failure = noFailure;
  return get;
};
class WriteContextImpl {
  constructor(registry, node) {
    this.registry = registry;
    this.node = node;
  }
  registry;
  node;
  get(atom) {
    return this.registry.get(atom);
  }
  set(atom, value) {
    return this.registry.set(atom, value);
  }
  setSelf(value) {
    return this.node.setValue(value);
  }
  refreshSelf() {
    return this.node.invalidate();
  }
}
// -----------------------------------------------------------------------------
// batching
// -----------------------------------------------------------------------------
/**
 * @internal
 */
export const BatchPhase = {
  disabled: 0,
  collect: 1,
  commit: 2
};
/**
 * @internal
 */
export const batchState = {
  phase: BatchPhase.disabled,
  depth: 0,
  stale: [],
  notify: /*#__PURE__*/new Set()
};
/**
 * @internal
 */
export function batch(f) {
  const previousPhase = batchState.phase;
  batchState.phase = BatchPhase.collect;
  batchState.depth++;
  let failure = noFailure;
  try {
    f();
  } catch (error) {
    failure = error;
  }
  if (batchState.depth === 1) {
    // what `f` changed before it failed is committed, so it is still rebuilt and heard
    const {
      notify,
      stale
    } = batchState;
    let i = 0;
    do {
      batchState.phase = BatchPhase.collect;
      for (; i < stale.length; i++) {
        try {
          batchRebuildNode(stale[i]);
        } catch (error) {
          if (failure === noFailure) failure = error;
        }
      }
      batchState.phase = BatchPhase.commit;
      for (const node of notify) {
        try {
          node.notify();
        } catch (error) {
          if (failure === noFailure) failure = error;
        }
      }
    } while (i < stale.length);
    batchState.stale = [];
    notify.clear();
  }
  batchState.depth--;
  batchState.phase = previousPhase;
  if (failure !== noFailure) throw failure;
}
function batchRebuildNode(node) {
  if (node.state === NodeState.valid) {
    if (!node.invalidatedDuringBuild) {
      return;
    }
    node.invalidatedDuringBuild = false;
    node.state = NodeState.stale;
    node.disposeLifetime();
  }
  node.value();
}
//# sourceMappingURL=AtomRegistry.js.map