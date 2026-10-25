import * as Duration from "../Duration.js";
import * as Api from "../ExecutionPlan.js";
import { dual, identity } from "../Function.js";
import * as Result from "../Result.js";
import * as Schedule from "../Schedule.js";
import { isEffect } from "./core.js";
import * as effect from "./effect.js";
import * as internalLayer from "./layer.js";
import * as internalSchedule from "./schedule.js";
/** @internal */
export const makeEventEmitter = (onEvent, currentMetadata) => {
  let lastStepIndex = -1;
  let stepAttempt = 0;
  const emit = event => effect.ignoreCause(onEvent(event));
  return {
    begin: effect.clockWith(clock => effect.suspend(() => {
      const meta = currentMetadata();
      if (meta.stepIndex !== lastStepIndex) {
        lastStepIndex = meta.stepIndex;
        stepAttempt = 0;
      }
      stepAttempt++;
      const state = {
        attempt: meta.attempt,
        stepAttempt,
        stepIndex: meta.stepIndex,
        startNanos: clock.monotonicTimeNanosUnsafe()
      };
      return effect.as(emit({
        _tag: "AttemptStart",
        attempt: state.attempt,
        stepAttempt: state.stepAttempt,
        stepIndex: state.stepIndex
      }), state);
    })),
    end: (state, exit) => effect.clockWith(clock => {
      const duration = Duration.nanos(clock.monotonicTimeNanosUnsafe() - state.startNanos);
      return emit(exit._tag === "Success" ? {
        _tag: "AttemptSuccess",
        attempt: state.attempt,
        stepAttempt: state.stepAttempt,
        stepIndex: state.stepIndex,
        duration
      } : {
        _tag: "AttemptFailure",
        attempt: state.attempt,
        stepAttempt: state.stepAttempt,
        stepIndex: state.stepIndex,
        duration,
        cause: exit.cause
      });
    })
  };
};
/** @internal */
export const withExecutionPlan = /*#__PURE__*/dual(args => isEffect(args[0]), (self, plan, options) => effect.suspend(() => {
  let i = 0;
  let meta = {
    attempt: 0,
    stepIndex: 0
  };
  const provideMeta = effect.provideServiceEffect(Api.CurrentMetadata, effect.sync(() => {
    meta = {
      attempt: meta.attempt + 1,
      stepIndex: i
    };
    return meta;
  }));
  const emitter = options?.onEvent === undefined ? undefined : makeEventEmitter(options.onEvent, () => meta);
  const instrument = emitter === undefined ? identity : attempt => effect.uninterruptibleMask(restore => effect.flatMap(emitter.begin, state => effect.onExit(restore(attempt), exit => emitter.end(state, exit))));
  let result;
  return effect.flatMap(effect.whileLoop({
    while: () => i < plan.steps.length && (result === undefined || Result.isFailure(result)),
    body() {
      const step = plan.steps[i];
      let nextEffect = provideMeta(instrument(internalLayer.provide(self, step.provide)));
      if (result) {
        let attempted = false;
        const wrapped = nextEffect;
        // ensure the schedule is applied at least once
        nextEffect = effect.suspend(() => {
          if (attempted) return wrapped;
          attempted = true;
          return effect.fromResult(result);
        });
        nextEffect = internalSchedule.retry(nextEffect, scheduleFromStep(step, false));
      } else {
        const schedule = scheduleFromStep(step, true);
        nextEffect = schedule ? internalSchedule.retry(nextEffect, schedule) : nextEffect;
      }
      return effect.result(nextEffect);
    },
    step(result_) {
      result = result_;
      i++;
    }
  }), () => effect.fromResult(result));
}));
/** @internal */
export const scheduleFromStep = (step, first) => {
  if (!first) {
    return internalSchedule.buildFromOptions({
      schedule: step.schedule ? step.schedule : step.attempts ? undefined : scheduleOnce,
      times: step.attempts,
      while: step.while
    });
  } else if (step.attempts === 1 || !(step.schedule || step.attempts)) {
    return undefined;
  }
  return internalSchedule.buildFromOptions({
    schedule: step.schedule,
    while: step.while,
    times: step.attempts ? step.attempts - 1 : undefined
  });
};
const scheduleOnce = /*#__PURE__*/Schedule.recurs(1);
//# sourceMappingURL=executionPlan.js.map