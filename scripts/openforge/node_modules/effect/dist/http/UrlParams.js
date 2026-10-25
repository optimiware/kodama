/**
 * Models URL query parameters as ordered string pairs.
 *
 * `UrlParams` is used for HTTP client query strings, URL-encoded form bodies,
 * and server-side decoding. Values can be built from records, iterables, or
 * native `URLSearchParams`, then updated, serialized, converted to a `URL`, or
 * decoded with schemas.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Equal from "../Equal.js";
import * as Equ from "../Equivalence.js";
import { dual } from "../Function.js";
import * as Hash from "../Hash.js";
import { PipeInspectableProto } from "../internal/core.js";
import * as InternalRecord from "../internal/record.js";
import * as Option from "../Option.js";
import { hasProperty } from "../Predicate.js";
import * as Tuple from "../Tuple.js";
const TypeId = "~effect/http/UrlParams";
/**
 * Returns `true` when a value is a `UrlParams` instance.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isUrlParams = u => hasProperty(u, TypeId);
const Proto = {
  ...PipeInspectableProto,
  [TypeId]: TypeId,
  [Symbol.iterator]() {
    return this.params[Symbol.iterator]();
  },
  toJSON() {
    return {
      _id: "UrlParams",
      params: Object.fromEntries(this.params)
    };
  },
  [Equal.symbol](that) {
    return Equivalence(this, that);
  },
  [Hash.symbol]() {
    return Hash.array(this.params.flat());
  }
};
/**
 * Creates `UrlParams` from ordered string key-value pairs.
 *
 * **Details**
 *
 * The input pairs are used as-is and are not coerced or normalized.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = params => {
  const self = Object.create(Proto);
  self.params = params;
  return self;
};
/**
 * Creates `UrlParams` from a supported input shape.
 *
 * **Details**
 *
 * Primitive values are converted to strings, arrays produce repeated parameters,
 * nested records use bracket notation, and `undefined` values are omitted.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromInput = input => {
  if (isUrlParams(input)) {
    return input;
  }
  const parsed = fromInputNested(input);
  const out = [];
  for (let i = 0; i < parsed.length; i++) {
    if (Array.isArray(parsed[i][0])) {
      const [keys, value] = parsed[i];
      out.push([`${keys[0]}[${keys.slice(1).join("][")}]`, value]);
    } else {
      out.push(parsed[i]);
    }
  }
  return make(out);
};
const fromInputNested = input => {
  const entries = typeof input[Symbol.iterator] === "function" ? Arr.fromIterable(input) : Object.entries(input);
  const out = [];
  for (const [key, value] of entries) {
    if (Array.isArray(value)) {
      for (let i = 0; i < value.length; i++) {
        if (value[i] !== undefined) {
          out.push([key, String(value[i])]);
        }
      }
    } else if (value !== null && typeof value === "object") {
      const nested = fromInputNested(value);
      for (const [k, v] of nested) {
        out.push([[key, ...(typeof k === "string" ? [k] : k)], v]);
      }
    } else if (value !== undefined) {
      out.push([key, String(value)]);
    }
  }
  return out;
};
/**
 * Provides an order-sensitive `Equivalence` instance for `UrlParams`.
 *
 * **Details**
 *
 * Two values are equivalent when they contain the same key-value pairs in the same
 * order.
 *
 * @stability unstable
 * @category instances
 * @since 4.0.0
 */
export const Equivalence = /*#__PURE__*/Equ.make((a, b) => arrayEquivalence(a.params, b.params));
const arrayEquivalence = /*#__PURE__*/Arr.makeEquivalence(/*#__PURE__*/Tuple.makeEquivalence([/*#__PURE__*/Equ.strictEqual(), /*#__PURE__*/Equ.strictEqual()]));
/**
 * An empty `UrlParams` value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const empty = /*#__PURE__*/make([]);
/**
 * Returns all values for a query parameter key in insertion order.
 *
 * **Details**
 *
 * Returns an empty array when the key is absent.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const getAll = /*#__PURE__*/dual(2, (self, key) => Arr.reduce(self.params, [], (acc, [k, value]) => {
  if (k === key) {
    acc.push(value);
  }
  return acc;
}));
/**
 * Returns the first value for a query parameter key safely.
 *
 * **When to use**
 *
 * Use when duplicate query parameters are ordered and the first occurrence has
 * precedence.
 *
 * **Details**
 *
 * Returns `Option.none` when the key is absent.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const getFirst = /*#__PURE__*/dual(2, (self, key) => Arr.findFirst(self.params, ([k]) => k === key).pipe(Option.map(([, value]) => value)));
/**
 * Returns the last value for a query parameter key safely.
 *
 * **When to use**
 *
 * Use when duplicate query parameters are ordered and the last occurrence has
 * precedence.
 *
 * **Details**
 *
 * Returns `Option.none` when the key is absent.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const getLast = /*#__PURE__*/dual(2, (self, key) => Arr.findLast(self.params, ([k]) => k === key).pipe(Option.map(([, value]) => value)));
/**
 * Sets a query parameter to a single value.
 *
 * **Details**
 *
 * Existing values for the same key are removed, and the new value is appended to
 * the end.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const set = /*#__PURE__*/dual(3, (self, key, value) => make(Arr.append(Arr.filter(self.params, ([k]) => k !== key), [key, String(value)])));
/**
 * Transforms the underlying ordered key-value pairs of `UrlParams`.
 *
 * **Details**
 *
 * The result is wrapped in a new `UrlParams` value.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const transform = /*#__PURE__*/dual(2, (self, f) => make(f(self.params)));
/**
 * Sets multiple query parameters from input.
 *
 * **Details**
 *
 * Keys present in the input replace existing values for those keys, while
 * unmentioned existing parameters are preserved.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setAll = /*#__PURE__*/dual(2, (self, input) => {
  const params = fromInput(input).params.slice();
  const keys = new Set();
  for (let i = 0; i < params.length; i++) {
    keys.add(params[i][0]);
  }
  for (let i = 0; i < self.params.length; i++) {
    if (keys.has(self.params[i][0])) continue;
    params.push(self.params[i]);
  }
  return make(params);
});
/**
 * Appends a query parameter value without removing existing values for the key.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const append = /*#__PURE__*/dual(3, (self, key, value) => make(Arr.append(self.params, [key, String(value)])));
/**
 * Appends all query parameters produced from the supplied input.
 *
 * **Details**
 *
 * Existing parameters are preserved.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const appendAll = /*#__PURE__*/dual(2, (self, input) => transform(self, Arr.appendAll(fromInput(input).params)));
/**
 * Removes all query parameter values for the specified key.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const remove = /*#__PURE__*/dual(2, (self, key) => transform(self, Arr.filter(([k]) => k !== key)));
/**
 * Serializes `UrlParams` to a URL query string without a leading question mark.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toString = input => new URLSearchParams(fromInput(input).params).toString();
/**
 * Builds a `Record` containing all the key-value pairs in the given `UrlParams`
 * as `string` (if only one value for a key) or a `NonEmptyArray<string>`
 * (when more than one value for a key)
 *
 * **Example** (Converting parameters to a record)
 *
 * ```ts import.meta.vitest
 * import { UrlParams } from "effect/http"
 *
 * const urlParams = UrlParams.fromInput({
 *   a: 1,
 *   b: true,
 *   c: "string",
 *   e: [1, 2, 3]
 * })
 * UrlParams.toRecord(urlParams) // => { a: "1", b: "true", c: "string", e: ["1", "2", "3"] }
 * ```
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toRecord = self => {
  const out = {};
  for (const [k, value] of self.params) {
    if (!Object.hasOwn(out, k)) {
      InternalRecord.assignProperty(out, k, value);
    } else {
      const current = out[k];
      if (typeof current === "string") {
        InternalRecord.assignProperty(out, k, [current, value]);
      } else {
        current.push(value);
      }
    }
  }
  return out;
};
/**
 * Builds a readonly record from `UrlParams`.
 *
 * **Details**
 *
 * Keys with one value map to a string, and keys with multiple values map to a
 * non-empty readonly array of strings.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toReadonlyRecord = toRecord;
//# sourceMappingURL=UrlParams.js.map