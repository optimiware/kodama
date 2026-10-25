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
import * as Arr from "../Array.ts";
import * as Equ from "../Equivalence.ts";
import type { Inspectable } from "../Inspectable.ts";
import * as Option from "../Option.ts";
import type { Pipeable } from "../Pipeable.ts";
import type { ReadonlyRecord } from "../Record.ts";
declare const TypeId = "~effect/http/UrlParams";
/**
 * Immutable collection of URL query parameters.
 *
 * **Details**
 *
 * Parameters are stored as ordered string key-value pairs and can contain multiple
 * values for the same key.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface UrlParams extends Pipeable, Inspectable, Iterable<readonly [string, string]> {
    readonly [TypeId]: typeof TypeId;
    readonly params: ReadonlyArray<readonly [string, string]>;
}
/**
 * Returns `true` when a value is a `UrlParams` instance.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isUrlParams: (u: unknown) => u is UrlParams;
/**
 * Input accepted when constructing `UrlParams`.
 *
 * **Details**
 *
 * Values can be provided as a coercible record, an iterable of key-value pairs, or
 * a native `URLSearchParams` value.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Input = UrlParams | CoercibleRecordInput | Iterable<readonly [string, Coercible]> | URLSearchParams;
type CoercibleRecordInput = CoercibleRecord & {
    readonly [Symbol.iterator]?: never;
};
/**
 * Primitive value that can be converted into a URL parameter string.
 *
 * **Gotchas**
 *
 * `undefined` values are skipped when constructing from input.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Coercible = string | number | bigint | boolean | null | undefined;
/**
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
type CoercibleRecordField<A> = A extends Coercible ? A : A extends ReadonlyArray<infer Item> ? ReadonlyArray<Item extends Coercible ? Item : never> : A extends object ? CoercibleRecord<A> : never;
/**
 * Record input whose fields can be coerced into URL parameter values.
 *
 * **Details**
 *
 * Nested records are rendered using bracket notation, and arrays produce repeated
 * parameters.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type CoercibleRecord<A extends object = any> = {
    readonly [K in keyof A]: CoercibleRecordField<A[K]>;
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
export declare const make: (params: ReadonlyArray<readonly [string, string]>) => UrlParams;
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
export declare const fromInput: (input: Input) => UrlParams;
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
export declare const Equivalence: Equ.Equivalence<UrlParams>;
/**
 * An empty `UrlParams` value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const empty: UrlParams;
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
export declare const getAll: {
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
    (key: string): (self: UrlParams) => ReadonlyArray<string>;
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
    (self: UrlParams, key: string): ReadonlyArray<string>;
};
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
export declare const getFirst: {
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
    (key: string): (self: UrlParams) => Option.Option<string>;
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
    (self: UrlParams, key: string): Option.Option<string>;
};
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
export declare const getLast: {
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
    (key: string): (self: UrlParams) => Option.Option<string>;
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
    (self: UrlParams, key: string): Option.Option<string>;
};
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
export declare const set: {
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
    (key: string, value: Coercible): (self: UrlParams) => UrlParams;
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
    (self: UrlParams, key: string, value: Coercible): UrlParams;
};
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
export declare const transform: {
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
    (f: (params: UrlParams["params"]) => UrlParams["params"]): (self: UrlParams) => UrlParams;
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
    (self: UrlParams, f: (params: UrlParams["params"]) => UrlParams["params"]): UrlParams;
};
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
export declare const setAll: {
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
    (input: Input): (self: UrlParams) => UrlParams;
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
    (self: UrlParams, input: Input): UrlParams;
};
/**
 * Appends a query parameter value without removing existing values for the key.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const append: {
    /**
     * Appends a query parameter value without removing existing values for the key.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string, value: Coercible): (self: UrlParams) => UrlParams;
    /**
     * Appends a query parameter value without removing existing values for the key.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: UrlParams, key: string, value: Coercible): UrlParams;
};
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
export declare const appendAll: {
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
    (input: Input): (self: UrlParams) => UrlParams;
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
    (self: UrlParams, input: Input): UrlParams;
};
/**
 * Removes all query parameter values for the specified key.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const remove: {
    /**
     * Removes all query parameter values for the specified key.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (key: string): (self: UrlParams) => UrlParams;
    /**
     * Removes all query parameter values for the specified key.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: UrlParams, key: string): UrlParams;
};
/**
 * Serializes `UrlParams` to a URL query string without a leading question mark.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export declare const toString: (input: Input) => string;
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
export declare const toRecord: (self: UrlParams) => Record<string, string | Arr.NonEmptyArray<string>>;
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
export declare const toReadonlyRecord: (self: UrlParams) => ReadonlyRecord<string, string | Arr.NonEmptyReadonlyArray<string>>;
export {};
//# sourceMappingURL=UrlParams.d.ts.map