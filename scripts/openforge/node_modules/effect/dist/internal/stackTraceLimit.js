/**
 * Check if `Error.stackTraceLimit` is writable.
 * Returns `false` if the property is frozen, non-writable, or `Error` is non-extensible.
 *
 * @internal
 */
export const isStackTraceLimitWritable = () => {
  const desc = Object.getOwnPropertyDescriptor(Error, "stackTraceLimit");
  if (desc === undefined) {
    return Object.isExtensible(Error);
  }
  return Object.hasOwn(desc, "writable") ? desc.writable === true : desc.set !== undefined;
};
// Cache the check result since it won't change during runtime
const canWriteStackTraceLimit = /*#__PURE__*/isStackTraceLimitWritable();
/**
 * Get the current `Error.stackTraceLimit` value.
 * Returns `undefined` if the property doesn't exist.
 *
 * @internal
 */
export const getStackTraceLimit = () => Error.stackTraceLimit;
/**
 * Safely set `Error.stackTraceLimit` if possible, otherwise no-op.
 *
 * Accepts `undefined` so a value read via {@link getStackTraceLimit} can be
 * restored faithfully.
 *
 * @internal
 */
export const setStackTraceLimit = value => {
  if (canWriteStackTraceLimit) {
    ;
    Error.stackTraceLimit = value;
  }
};
//# sourceMappingURL=stackTraceLimit.js.map