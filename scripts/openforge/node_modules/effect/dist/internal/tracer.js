import { getStackTraceLimit, setStackTraceLimit } from "./stackTraceLimit.js";
/** @internal */
export const addSpanStackTrace = options => {
  if (options?.captureStackTrace === false) {
    return options;
  } else if (options?.captureStackTrace !== undefined && typeof options.captureStackTrace !== "boolean") {
    return options;
  }
  const limit = getStackTraceLimit();
  if (limit === 0 && options?.captureStackTrace !== true) {
    return {
      ...options,
      captureStackTrace: false
    };
  }
  setStackTraceLimit(3);
  const traceError = new Error();
  setStackTraceLimit(limit);
  return {
    ...options,
    captureStackTrace: spanCleaner(() => traceError.stack)
  };
};
/** @internal */
export const makeStackCleaner = line => stack => {
  let cache;
  return () => {
    if (cache !== undefined) return cache;
    const trace = stack();
    if (!trace) return undefined;
    const lines = trace.split("\n");
    if (lines[line] !== undefined) {
      cache = lines[line].trim();
      return cache;
    }
  };
};
const spanCleaner = /*#__PURE__*/makeStackCleaner(3);
//# sourceMappingURL=tracer.js.map