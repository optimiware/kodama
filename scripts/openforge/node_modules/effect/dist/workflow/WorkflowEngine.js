/**
 * Defines workflow engine services and an in-memory implementation.
 *
 * `WorkflowEngine` registers workflow handlers, runs executions, polls results,
 * resumes suspended runs, executes activities, stores durable deferred results,
 * and schedules durable clocks. `WorkflowInstance` holds the runtime state for
 * one workflow run. The in-memory layer is useful for tests and local
 * development.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cause from "../Cause.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Exit from "../Exit.js";
import * as Fiber from "../Fiber.js";
import * as FiberMap from "../FiberMap.js";
import * as Latch from "../Latch.js";
import * as Layer from "../Layer.js";
import * as Option from "../Option.js";
import * as Schedule from "../Schedule.js";
import * as Schema from "../Schema.js";
import * as Scope from "../Scope.js";
import * as Workflow from "./Workflow.js";
/**
 * Service that represents workflow runtimes, responsible for registering and
 * executing workflows and coordinating activities, durable deferreds,
 * interrupts, resumes, and clocks.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class WorkflowEngine extends /*#__PURE__*/Context.Service()("effect/workflow/WorkflowEngine") {}
/**
 * Service that contains workflow runtime state for one execution.
 *
 * **When to use**
 *
 * Use to read or update workflow execution, suspension, interruption,
 * lifetime, failure, and activity coordination state inside workflow engine
 * internals.
 *
 * **Details**
 *
 * The service stores the execution ID, workflow definition, long-lived scope,
 * suspension and interruption flags, the stored failure cause, and activity
 * coordination state for a single workflow run.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class WorkflowInstance extends /*#__PURE__*/Context.Service()("effect/workflow/WorkflowEngine/WorkflowInstance") {
  static initial(workflow, executionId, scope = Scope.makeUnsafe()) {
    return WorkflowInstance.of({
      executionId,
      workflow,
      scope,
      suspended: false,
      interrupted: false,
      abandoned: false,
      cause: undefined,
      awaitedDeferreds: new Set(),
      completedDeferreds: new Set(),
      activityState: {
        count: 0,
        latch: Latch.makeUnsafe()
      }
    });
  }
}
/**
 * Creates deferred state shared by workflow engines.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeDeferredState = () => {
  const running = new Map();
  return {
    trackRun: (instance, effect) => Effect.withFiber(fiber => {
      const run = {
        instance,
        fiber: fiber
      };
      running.set(instance.executionId, run);
      return Effect.ensuring(Effect.provideService(effect, WorkflowInstance, instance), Effect.sync(() => {
        if (running.get(instance.executionId) !== run) return;
        running.delete(instance.executionId);
      }));
    }),
    deferredDone: (executionId, name) => Effect.withFiber(current => {
      const run = running.get(executionId);
      if (!run) return Effect.void;
      if (run.fiber === current || run.fiber.pollUnsafe() || !run.instance.awaitedDeferreds.has(name)) {
        return Effect.void;
      }
      // The engine stores the completion before waking this run. Replay
      // observes it after the interrupted run reports suspension.
      run.instance.suspended = true;
      return Fiber.interrupt(run.fiber);
    })
  };
};
/**
 * Builds a typed `WorkflowEngine` service from a low-level encoded
 * implementation.
 *
 * **When to use**
 *
 * Use when wiring a trusted low-level workflow engine implementation into the
 * typed `WorkflowEngine` service.
 *
 * **Gotchas**
 *
 * The implementation must correctly persist, resume, and encode workflow state.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeUnsafe = options => WorkflowEngine.of({
  register: Effect.fnUntraced(function* (workflow, execute) {
    const services = yield* Effect.context();
    yield* options.register(workflow, (payload, executionId) => Effect.suspend(() => execute(payload, executionId)).pipe(Effect.updateContext(input => Context.merge(services, input))));
  }),
  execute: Effect.fnUntraced(function* (self, opts) {
    const payload = opts.payload;
    const executionId = opts.executionId;
    const suspendedRetrySchedule = opts.suspendedRetrySchedule ?? defaultRetrySchedule;
    yield* Effect.annotateCurrentSpan({
      executionId
    });
    let result = Option.none();
    // link interruption with parent workflow
    const parentInstance = yield* Effect.serviceOption(WorkflowInstance);
    if (Option.isSome(parentInstance)) {
      const instance = parentInstance.value;
      yield* Effect.addFinalizer(() => {
        if (!instance.interrupted || Option.isSome(result) && result.value._tag === "Complete") {
          return Effect.void;
        }
        return options.interrupt(self, executionId);
      });
    }
    const run = options.execute(self, {
      executionId,
      payload: payload,
      discard: opts.discard ?? false,
      parent: Option.getOrUndefined(parentInstance)
    });
    if (opts.discard) {
      yield* run;
      return executionId;
    }
    if (Option.isSome(parentInstance)) {
      const wrapped = yield* Workflow.wrapActivityResult(run, result => result._tag === "Suspended");
      result = Option.some(wrapped);
      if (wrapped._tag === "Suspended") {
        return yield* Workflow.suspend(parentInstance.value);
      }
      return yield* wrapped.exit;
    }
    let sleep;
    while (true) {
      const wrapped = yield* run;
      result = Option.some(wrapped);
      if (wrapped._tag === "Complete") {
        return yield* wrapped.exit;
      }
      sleep ??= (yield* Schedule.toStepWithSleep(suspendedRetrySchedule))(void 0).pipe(Effect.catch(() => Effect.die(`${self._tag}.execute: suspendedRetrySchedule exhausted`)));
      yield* sleep;
    }
  }),
  poll: options.poll,
  interrupt: options.interrupt,
  interruptUnsafe: options.interruptUnsafe,
  resume: options.resume,
  activityExecute: Effect.fnUntraced(function* (activity, attempt) {
    const result = yield* options.activityExecute(activity, attempt);
    if (result._tag === "Suspended") {
      return result;
    }
    const exit = yield* Effect.orDie(Schema.decodeEffect(activity.exitSchemaPartial)(toJsonExit(result.exit)));
    return new Workflow.Complete({
      exit
    });
  }),
  deferredResult: Effect.fnUntraced(function* (deferred) {
    const instance = yield* WorkflowInstance;
    yield* Effect.annotateCurrentSpan({
      executionId: instance.executionId
    });
    const exit = yield* options.deferredResult(deferred);
    if (Option.isNone(exit)) {
      return Option.none();
    }
    return Option.some(yield* Effect.orDie(Schema.decodeEffect(deferred.exitSchema)(toJsonExit(exit.value))));
  }, Effect.withSpan("WorkflowEngine.deferredResult", deferred => ({
    attributes: {
      name: deferred.name
    }
  }), {
    captureStackTrace: false
  })),
  deferredDone: Effect.fnUntraced(function* (deferred, opts) {
    return yield* options.deferredDone({
      workflowName: opts.workflowName,
      executionId: opts.executionId,
      deferredName: opts.deferredName,
      exit: yield* Schema.encodeEffect(deferred.exitSchema)(opts.exit)
    });
  }, Effect.withSpan("WorkflowEngine.deferredDone", (_, {
    deferredName,
    executionId
  }) => ({
    attributes: {
      name: deferredName,
      executionId
    }
  }), {
    captureStackTrace: false
  })),
  scheduleClock: (workflow, opts) => options.scheduleClock(workflow, opts).pipe(Effect.withSpan("WorkflowEngine.scheduleClock", {
    attributes: {
      executionId: opts.executionId,
      name: opts.clock.name
    }
  }, {
    captureStackTrace: false
  }))
});
const defaultRetrySchedule = /*#__PURE__*/Schedule.min([/*#__PURE__*/Schedule.exponential(200, 1.5), /*#__PURE__*/Schedule.spaced(30000)]);
/**
 * Layer that provides an in-memory `WorkflowEngine`.
 *
 * **When to use**
 *
 * Use to run tests and local development workflows where durability is not
 * needed.
 *
 * **Gotchas**
 *
 * This layer keeps state only in memory and is not suitable for production
 * workflows that require durability.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerMemory = /*#__PURE__*/Layer.effect(WorkflowEngine)(/*#__PURE__*/Effect.gen(function* () {
  const scope = yield* Effect.scope;
  const workflows = new Map();
  const executions = new Map();
  const activities = new Map();
  const deferredState = makeDeferredState();
  const resume = Effect.fnUntraced(function* (executionId) {
    const state = executions.get(executionId);
    if (!state) return;
    const exit = state.fiber?.pollUnsafe();
    if (exit && exit._tag === "Success" && exit.value._tag === "Complete") {
      return;
    } else if (state.fiber && !exit) {
      // A child can finish while the parent is still releasing its activity.
      // Preserve the wake until that run has published its suspended result.
      if (!state.resumeRequested) {
        state.resumeRequested = true;
        yield* Fiber.await(state.fiber).pipe(Effect.flatMap(() => {
          state.resumeRequested = false;
          return resume(executionId);
        }), Effect.forkIn(scope));
      }
      return;
    }
    const entry = workflows.get(state.instance.workflow._tag);
    const instance = WorkflowInstance.initial(state.instance.workflow, state.instance.executionId, state.instance.scope);
    state.instance = instance;
    state.fiber = yield* state.execute(state.payload, state.instance.executionId).pipe(Effect.onExit(() => {
      if (!state.interrupted) {
        return Effect.void;
      }
      instance.interrupted = true;
      instance.suspended = false;
      return Effect.withFiber(fiber => Effect.interruptible(Fiber.interrupt(fiber)));
    }), Workflow.intoResult, Effect.provideService(WorkflowEngine, engine), effect => deferredState.trackRun(instance, effect), Effect.tap(result => {
      if (!state.parent || result._tag !== "Complete") {
        return Effect.void;
      }
      return Effect.forkIn(resume(state.parent), scope);
    }), Effect.forkIn(entry.scope));
  });
  const deferredResults = new Map();
  const clocks = yield* FiberMap.make();
  const engine = makeUnsafe({
    register: Effect.fnUntraced(function* (workflow, execute) {
      workflows.set(workflow._tag, {
        workflow,
        execute,
        scope: yield* Effect.scope
      });
    }),
    execute: Effect.fnUntraced(function* (workflow, options) {
      const entry = workflows.get(workflow._tag);
      if (!entry) {
        return yield* Effect.orDie(Effect.fail(`Workflow ${workflow._tag} is not registered`));
      }
      let state = executions.get(options.executionId);
      if (!state) {
        state = {
          payload: options.payload,
          execute: entry.execute,
          instance: WorkflowInstance.initial(workflow, options.executionId),
          interrupted: false,
          resumeRequested: false,
          fiber: undefined,
          parent: options.parent?.executionId
        };
        executions.set(options.executionId, state);
        yield* resume(options.executionId);
      }
      if (options.discard) return;
      // Capture together so a wake that swaps in a replay cannot desync them.
      const instance = state.instance;
      const exit = yield* Fiber.await(state.fiber);
      if (Exit.isFailure(exit) && Cause.hasInterruptsOnly(exit.cause) && instance.suspended) {
        // A completion preempted the run; the caller retries into the replay.
        return new Workflow.Suspended({});
      }
      return yield* exit;
    }),
    interrupt: Effect.fnUntraced(function* (_workflow, executionId) {
      const state = executions.get(executionId);
      if (!state) return;
      state.interrupted = true;
      yield* resume(executionId);
    }),
    interruptUnsafe: Effect.fnUntraced(function* (_workflow, executionId) {
      const state = executions.get(executionId);
      if (!state) return;
      state.interrupted = true;
      if (state.fiber) {
        yield* Fiber.interrupt(state.fiber);
      }
    }),
    resume(_workflow, executionId) {
      return resume(executionId);
    },
    activityExecute: Effect.fnUntraced(function* (activity, attempt) {
      const instance = yield* WorkflowInstance;
      const activityId = `${instance.executionId}/${activity.name}/${attempt}`;
      let state = activities.get(activityId);
      if (state) {
        const exit = state.exit;
        if (exit && exit._tag === "Success" && exit.value._tag === "Suspended") {
          state.exit = undefined;
        } else if (exit) {
          return yield* exit;
        }
      } else {
        state = {
          exit: undefined
        };
        activities.set(activityId, state);
      }
      const activityInstance = WorkflowInstance.initial(instance.workflow, instance.executionId);
      activityInstance.interrupted = instance.interrupted;
      return yield* activity.executeEncoded.pipe(Workflow.intoResult, Effect.provideService(WorkflowInstance, activityInstance), Effect.onExit(exit => {
        state.exit = exit;
        return Effect.void;
      }));
    }),
    poll: (_workflow, executionId) => Effect.suspend(() => {
      const state = executions.get(executionId);
      if (!state) {
        return Effect.succeedNone;
      }
      const exit = state.fiber?.pollUnsafe();
      if (!exit) {
        return Effect.succeedNone;
      }
      return exit._tag === "Success" ? Effect.succeedSome(exit.value) : Effect.die(exit.cause);
    }),
    deferredResult: Effect.fnUntraced(function* (deferred) {
      const instance = yield* WorkflowInstance;
      const id = `${instance.executionId}/${deferred.name}`;
      return Option.fromNullishOr(deferredResults.get(id));
    }),
    deferredDone: options => Effect.suspend(() => {
      const id = `${options.executionId}/${options.deferredName}`;
      if (deferredResults.has(id)) return Effect.void;
      deferredResults.set(id, options.exit);
      const wake = Effect.andThen(deferredState.deferredDone(options.executionId, options.deferredName), resume(options.executionId));
      return Effect.flatMap(Effect.serviceOption(WorkflowInstance), instance =>
      // A workflow finalizer cannot wait for its own run's cleanup.
      // The engine scope owns the wake so cleanup still precedes replay.
      Option.isSome(instance) && instance.value.executionId === options.executionId ? wake.pipe(Effect.forkIn(scope), Effect.asVoid) : wake);
    }),
    scheduleClock: (workflow, options) => engine.deferredDone(options.clock.deferred, {
      workflowName: workflow._tag,
      executionId: options.executionId,
      deferredName: options.clock.deferred.name,
      exit: Exit.void
    }).pipe(Effect.delay(options.clock.duration), FiberMap.run(clocks, `${options.executionId}/${options.clock.name}`, {
      onlyIfMissing: true
    }), Effect.asVoid)
  });
  return engine;
}));
const toJsonExit = /*#__PURE__*/Exit.map(value => value ?? null);
//# sourceMappingURL=WorkflowEngine.js.map