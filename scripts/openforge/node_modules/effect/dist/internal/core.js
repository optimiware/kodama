import * as Equal from "../Equal.js";
import { format } from "../Formatter.js";
import { dual, identity } from "../Function.js";
import * as Hash from "../Hash.js";
import { NodeInspectSymbol } from "../Inspectable.js";
import { pipeArguments } from "../Pipeable.js";
import { hasProperty } from "../Predicate.js";
import { SingleShotGen } from "../Utils.js";
import * as InternalRecord from "./record.js";
/** @internal */
export const EffectTypeId = `~effect/Effect`;
/** @internal */
export const ExitTypeId = `~effect/Exit`;
const effectVariance = {
  _A: identity,
  _E: identity,
  _R: identity
};
/** @internal */
export const identifier = `${EffectTypeId}/identifier`;
/** @internal */
export const args = `${EffectTypeId}/args`;
/** @internal */
export const evaluate = `${EffectTypeId}/evaluate`;
/** @internal */
export const contA = `${EffectTypeId}/successCont`;
/** @internal */
export const contE = `${EffectTypeId}/failureCont`;
/** @internal */
export const contAll = `${EffectTypeId}/ensureCont`;
/** @internal */
export const Yield = /*#__PURE__*/Symbol.for("effect/Effect/Yield");
/** @internal */
export const PipeInspectableProto = {
  pipe() {
    return pipeArguments(this, arguments);
  },
  toJSON() {
    return {
      ...this
    };
  },
  toString() {
    return format(this.toJSON(), {
      ignoreToString: true,
      space: 2
    });
  },
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
};
/** @internal */
export const StructuralProto = {
  [Hash.symbol]() {
    return Hash.structureKeys(this, Object.keys(this));
  },
  [Equal.symbol](that) {
    const selfKeys = Object.keys(this);
    const thatKeys = Object.keys(that);
    if (selfKeys.length !== thatKeys.length) return false;
    for (let i = 0; i < selfKeys.length; i++) {
      if (selfKeys[i] !== thatKeys[i] || !Equal.equals(this[selfKeys[i]], that[selfKeys[i]])) {
        return false;
      }
    }
    return true;
  }
};
/** @internal */
export const EffectProto = {
  [EffectTypeId]: effectVariance,
  ...PipeInspectableProto,
  [Symbol.iterator]() {
    return new SingleShotGen(this);
  },
  toJSON() {
    return {
      _id: "Effect",
      op: this[identifier],
      ...(args in this ? {
        args: this[args]
      } : undefined)
    };
  }
};
/** @internal */
export const isEffect = u => hasProperty(u, EffectTypeId);
/** @internal */
export const isExit = u => hasProperty(u, ExitTypeId);
// ----------------------------------------------------------------------------
// Cause
// ----------------------------------------------------------------------------
/** @internal */
export const CauseTypeId = "~effect/Cause";
/** @internal */
export const CauseReasonTypeId = "~effect/Cause/Reason";
/** @internal */
export const isCause = self => hasProperty(self, CauseTypeId);
/** @internal */
export const isCauseReason = self => hasProperty(self, CauseReasonTypeId);
/** @internal */
export class CauseImpl {
  constructor(failures) {
    this[CauseTypeId] = CauseTypeId;
    this.reasons = failures;
  }
  pipe() {
    return pipeArguments(this, arguments);
  }
  toJSON() {
    return {
      _id: "Cause",
      failures: this.reasons.map(f => f.toJSON())
    };
  }
  toString() {
    return `Cause(${format(this.reasons)})`;
  }
  [NodeInspectSymbol]() {
    return this.toJSON();
  }
  [Equal.symbol](that) {
    return isCause(that) && this.reasons.length === that.reasons.length && this.reasons.every((e, i) => Equal.equals(e, that.reasons[i]));
  }
  [Hash.symbol]() {
    return Hash.array(this.reasons);
  }
}
const annotationsMap = /*#__PURE__*/new WeakMap();
/** @internal */
export class ReasonBase {
  [CauseReasonTypeId];
  annotations;
  _tag;
  constructor(_tag, annotations, originalError) {
    this[CauseReasonTypeId] = CauseReasonTypeId;
    this._tag = _tag;
    if (annotations !== constEmptyAnnotations && typeof originalError === "object" && originalError !== null && annotations.size > 0) {
      const prevAnnotations = annotationsMap.get(originalError);
      if (prevAnnotations) {
        annotations = new Map([...prevAnnotations, ...annotations]);
      }
      annotationsMap.set(originalError, annotations);
    }
    this.annotations = annotations;
  }
  annotate(annotations, options) {
    if (annotations.mapUnsafe.size === 0) return this;
    const newAnnotations = new Map(this.annotations);
    annotations.mapUnsafe.forEach((value, key) => {
      if (options?.overwrite !== true && newAnnotations.has(key)) return;
      newAnnotations.set(key, value);
    });
    const self = Object.assign(Object.create(Object.getPrototypeOf(this)), this);
    self.annotations = newAnnotations;
    return self;
  }
  pipe() {
    return pipeArguments(this, arguments);
  }
  toString() {
    return format(this);
  }
  [NodeInspectSymbol]() {
    return this.toString();
  }
}
/** @internal */
export const constEmptyAnnotations = /*#__PURE__*/new Map();
/** @internal */
export class Fail extends ReasonBase {
  constructor(error, annotations = constEmptyAnnotations) {
    super("Fail", annotations, error);
    this.error = error;
  }
  toString() {
    return `Fail(${format(this.error)})`;
  }
  toJSON() {
    return {
      _tag: "Fail",
      error: this.error
    };
  }
  [Equal.symbol](that) {
    return isFailReason(that) && Equal.equals(this.error, that.error) && Equal.equals(this.annotations, that.annotations);
  }
  [Hash.symbol]() {
    return Hash.combine(Hash.string(this._tag))(Hash.combine(Hash.hash(this.error))(Hash.hash(this.annotations)));
  }
}
/** @internal */
export const causeFromReasons = reasons => new CauseImpl(reasons);
const dedupeReasons = (self, that) => {
  // Avoid importing Array.ts into the core bundle.
  // Snapshot both arrays before invoking user-defined hash or equality methods.
  const buckets = new Map();
  const out = [];
  for (const reason of self.concat(that)) {
    const hash = Hash.hash(reason);
    const bucket = buckets.get(hash);
    if (bucket === undefined) {
      buckets.set(hash, [reason]);
    } else if (bucket.some(previous => Equal.equals(previous, reason))) {
      continue;
    } else {
      bucket.push(reason);
    }
    out.push(reason);
  }
  return out;
};
/** @internal */
export const causeCombine = /*#__PURE__*/dual(2, (self, that) => {
  if (self.reasons.length === 0) {
    return that;
  } else if (that.reasons.length === 0) {
    return self;
  }
  const newCause = new CauseImpl(dedupeReasons(self.reasons, that.reasons));
  return Equal.equals(self, newCause) ? self : newCause;
});
/** @internal */
export const causeEmpty = /*#__PURE__*/new CauseImpl([]);
/** @internal */
export const causeFail = error => new CauseImpl([new Fail(error)]);
/** @internal */
export class Die extends ReasonBase {
  constructor(defect, annotations = constEmptyAnnotations) {
    super("Die", annotations, defect);
    this.defect = defect;
  }
  toString() {
    return `Die(${format(this.defect)})`;
  }
  toJSON() {
    return {
      _tag: "Die",
      defect: this.defect
    };
  }
  [Equal.symbol](that) {
    return isDieReason(that) && Equal.equals(this.defect, that.defect) && Equal.equals(this.annotations, that.annotations);
  }
  [Hash.symbol]() {
    return Hash.combine(Hash.string(this._tag))(Hash.combine(Hash.hash(this.defect))(Hash.hash(this.annotations)));
  }
}
/** @internal */
export const causeDie = defect => new CauseImpl([new Die(defect)]);
/** @internal */
export const causeAnnotate = /*#__PURE__*/dual(args => isCause(args[0]), (self, annotations, options) => {
  if (annotations.mapUnsafe.size === 0) return self;
  return new CauseImpl(self.reasons.map(f => f.annotate(annotations, options)));
});
/** @internal */
export const isFailReason = self => self._tag === "Fail";
/** @internal */
export const isDieReason = self => self._tag === "Die";
/** @internal */
export const isInterruptReason = self => self._tag === "Interrupt";
function defaultEvaluate(_fiber) {
  return exitDie(`Effect.evaluate: Not implemented`);
}
/** @internal */
export const makePrimitiveProto = options => ({
  ...EffectProto,
  [identifier]: options.op,
  [evaluate]: options[evaluate] ?? defaultEvaluate,
  [contA]: options[contA],
  [contE]: options[contE],
  [contAll]: options[contAll]
});
/** @internal */
export const makePrimitive = options => {
  const Proto = makePrimitiveProto(options);
  const PrimitiveImpl = function (value) {
    this[args] = value;
  };
  PrimitiveImpl.prototype = Proto;
  return function (value) {
    return new PrimitiveImpl(value);
  };
};
/** @internal */
export const makeExit = options => {
  const Proto = {
    [ExitTypeId]: ExitTypeId,
    _tag: options.op,
    get [options.prop]() {
      return this[args];
    },
    ...makePrimitiveProto(options),
    toString() {
      return `${options.op}(${format(this[args])})`;
    },
    toJSON() {
      return {
        _id: "Exit",
        _tag: options.op,
        [options.prop]: this[args]
      };
    },
    [Equal.symbol](that) {
      return isExit(that) && that._tag === this._tag && Equal.equals(this[args], that[args]);
    },
    [Hash.symbol]() {
      return Hash.combine(Hash.string(options.op), Hash.hash(this[args]));
    }
  };
  const ExitPrimitive = function (value) {
    this[args] = value;
  };
  ExitPrimitive.prototype = Proto;
  return function (value) {
    return new ExitPrimitive(value);
  };
};
/** @internal */
export const exitSucceed = /*#__PURE__*/makeExit({
  op: "Success",
  prop: "value",
  [evaluate](fiber) {
    const cont = fiber.getCont(contA);
    return cont ? cont[contA](this[args], fiber, this) : fiber.yieldWith(this);
  }
});
/** @internal */
export const StackTraceKey = {
  key: "effect/Cause/StackTrace"
};
/** @internal */
export const InterruptorStackTrace = {
  key: "effect/Cause/InterruptorStackTrace"
};
/** @internal */
export const exitFailCause = /*#__PURE__*/makeExit({
  op: "Failure",
  prop: "cause",
  [evaluate](fiber) {
    let cause = this[args];
    let annotated = false;
    if (fiber.cache.stackFrame) {
      cause = causeAnnotate(cause, {
        mapUnsafe: new Map([[StackTraceKey.key, fiber.cache.stackFrame]])
      });
      annotated = true;
    }
    let cont = fiber.getCont(contE);
    const interruptedCause = fiber._interruptedCause;
    if (interruptedCause && fiber.interruptible) {
      // Drop typed failures only when interruption skips a recovery handler.
      // Interruptibility-restoration continuations have no identifier.
      let skippedHandler = false;
      while (cont && fiber.interruptible) {
        skippedHandler ||= identifier in cont;
        cont = fiber.getCont(contE);
      }
      if (skippedHandler) {
        cause = causeFromReasons(cause.reasons.filter(reason => reason._tag !== "Fail"));
      }
      cause = causeCombine(cause, interruptedCause);
      annotated = true;
    }
    return cont ? cont[contE](cause, fiber, annotated ? undefined : this) : fiber.yieldWith(annotated ? exitFailCause(cause) : this);
  }
});
/** @internal */
export const exitFail = e => exitFailCause(causeFail(e));
/** @internal */
export const exitDie = defect => exitFailCause(causeDie(defect));
/** @internal */
export const withFiber = /*#__PURE__*/makePrimitive({
  op: "WithFiber",
  [evaluate](fiber) {
    return this[args](fiber);
  }
});
/**
 * Accesses the current fiber to compute a value without a separate `succeed`
 * operation.
 *
 * @internal
 */
export const withFiberSucceed = /*#__PURE__*/makePrimitive({
  op: "WithFiberSucceed",
  [evaluate](fiber) {
    const value = this[args](fiber);
    const cont = fiber.getCont(contA);
    return cont ? cont[contA](value, fiber) : fiber.yieldWith(exitSucceed(value));
  }
});
/** @internal */
export const YieldableError = /*#__PURE__*/function () {
  class YieldableError extends globalThis.Error {}
  const proto = /*#__PURE__*/makePrimitiveProto({
    op: "YieldableError",
    [evaluate]() {
      return exitFail(this);
    }
  });
  delete proto.toString;
  Object.assign(YieldableError.prototype, proto);
  return YieldableError;
}();
/** @internal */
export const Error = /*#__PURE__*/function () {
  const plainArgsSymbol = /*#__PURE__*/Symbol.for("effect/Data/Error/plainArgs");
  return class Base extends YieldableError {
    constructor(args) {
      super(args?.message, args?.cause ? {
        cause: args.cause
      } : undefined);
      if (args) {
        InternalRecord.assignProperties(this, args);
        // @effect-diagnostics-next-line floatingEffect:off
        Object.defineProperty(this, plainArgsSymbol, {
          value: args,
          enumerable: false
        });
      }
    }
    toJSON() {
      return {
        ...this[plainArgsSymbol],
        ...this
      };
    }
  };
}();
/** @internal */
export const TaggedError = tag => {
  class Base extends Error {
    _tag = tag;
  }
  ;
  Base.prototype.name = tag;
  return Base;
};
/** @internal */
export const NoSuchElementErrorTypeId = "~effect/Cause/NoSuchElementError";
/** @internal */
export const isNoSuchElementError = u => hasProperty(u, NoSuchElementErrorTypeId);
/** @internal */
export class NoSuchElementError extends /*#__PURE__*/TaggedError("NoSuchElementError") {
  [NoSuchElementErrorTypeId] = NoSuchElementErrorTypeId;
  constructor(message) {
    super({
      message
    });
  }
}
/** @internal */
export const DoneTypeId = "~effect/Cause/Done";
/** @internal */
export const isDone = u => hasProperty(u, DoneTypeId);
const DoneVoid = {
  [DoneTypeId]: DoneTypeId,
  _tag: "Done",
  value: undefined
};
/** @internal */
export const Done = value => {
  if (value === undefined) return DoneVoid;
  return {
    [DoneTypeId]: DoneTypeId,
    _tag: "Done",
    value
  };
};
const doneVoid = /*#__PURE__*/exitFail(DoneVoid);
/** @internal */
export const done = value => {
  if (value === undefined) return doneVoid;
  return exitFail(Done(value));
};
//# sourceMappingURL=core.js.map