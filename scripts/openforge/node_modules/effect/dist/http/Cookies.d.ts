/**
 * Models HTTP cookies and cookie collections for requests and responses.
 *
 * A `Cookie` stores a name, value, encoded value, and standard cookie
 * attributes. A `Cookies` value is an immutable collection keyed by cookie
 * name. This module parses request `Cookie` headers, builds response
 * `Set-Cookie` headers, and provides helpers for adding, removing, merging, and
 * expiring cookies.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Data from "../Data.ts";
import * as Duration from "../Duration.ts";
import * as Inspectable from "../Inspectable.ts";
import * as Option from "../Option.ts";
import { type Pipeable } from "../Pipeable.ts";
import * as Record from "../Record.ts";
import * as Result from "../Result.ts";
import type * as Types from "../Types.ts";
declare const TypeId = "~effect/http/Cookies";
/**
 * Returns `true` when a value is a `Cookies` collection.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isCookies: (u: unknown) => u is Cookies;
/**
 * Immutable collection of HTTP cookies keyed by cookie name.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Cookies extends Pipeable, Inspectable.Inspectable {
    readonly [TypeId]: typeof TypeId;
    readonly cookies: Record.ReadonlyRecord<string, Cookie>;
}
declare const CookieTypeId = "~effect/http/Cookies/Cookie";
/**
 * HTTP cookie value with its decoded value, encoded value, and optional cookie
 * attributes such as domain, path, expiration, security, and same-site settings.
 *
 * @stability unstable
 * @category cookies
 * @since 4.0.0
 */
export interface Cookie extends Inspectable.Inspectable {
    readonly [CookieTypeId]: typeof CookieTypeId;
    readonly name: string;
    readonly value: string;
    readonly valueEncoded: string;
    readonly options?: {
        readonly domain?: string | undefined;
        readonly expires?: Date | undefined;
        readonly maxAge?: Duration.Input | undefined;
        readonly path?: string | undefined;
        readonly priority?: "low" | "medium" | "high" | undefined;
        readonly httpOnly?: boolean | undefined;
        readonly secure?: boolean | undefined;
        readonly partitioned?: boolean | undefined;
        readonly sameSite?: "lax" | "strict" | "none" | undefined;
    } | undefined;
}
/**
 * Returns `true` when a value is a `Cookie`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isCookie: (u: unknown) => u is Cookie;
declare const CookieErrorTypeId = "~effect/http/Cookies/CookieError";
/**
 * Error reason describing why cookie construction failed, such as invalid name,
 * value, domain, path, or infinite max-age.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class CookiesErrorReason extends Data.Error<{
    readonly _tag: "InvalidCookieName" | "InvalidCookieValue" | "InvalidCookieDomain" | "InvalidCookiePath" | "CookieInfinityMaxAge";
    readonly cause?: unknown;
}> {
}
declare const CookiesError_base: new <A extends Record<string, any> = {}>(args: Types.VoidIfEmpty<{ readonly [P in keyof A as P extends "_tag" ? never : P]: A[P]; }>) => import("../Cause.ts").YieldableError & {
    readonly _tag: "CookiesError";
} & Readonly<A>;
/**
 * Error returned when a cookie name, value, domain, path, or max-age option is invalid.
 *
 * **Details**
 *
 * Inspect `reason` to determine the specific validation failure.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export declare class CookiesError extends CookiesError_base<{
    readonly reason: CookiesErrorReason;
}> {
    /**
     * Creates a cookie error from a reason tag and optional cause.
     *
     * @stability unstable
     * @since 4.0.0
     */
    static fromReason(reason: CookiesError["reason"]["_tag"], cause?: unknown): CookiesError;
    /**
     * Marks this value as a cookie validation error for runtime guards.
     *
     * @stability unstable
     * @since 4.0.0
     */
    readonly [CookieErrorTypeId] = "~effect/http/Cookies/CookieError";
    /**
     * Uses the concrete cookie error reason as the public message.
     *
     * @stability unstable
     * @since 4.0.0
     */
    get message(): "CookieInfinityMaxAge" | "InvalidCookieDomain" | "InvalidCookieName" | "InvalidCookiePath" | "InvalidCookieValue";
}
/**
 * Checks whether a value is a `CookiesError`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isCookiesError: (u: unknown) => u is CookiesError;
/**
 * Creates a `Cookies` collection from an existing readonly record of cookies keyed by cookie name.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fromReadonlyRecord: (cookies: Record.ReadonlyRecord<string, Cookie>) => Cookies;
/**
 * Create a Cookies object from an Iterable
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fromIterable: (cookies: Iterable<Cookie>) => Cookies;
/**
 * Create a Cookies object from a set of Set-Cookie headers
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const fromSetCookie: (headers: Iterable<string> | string) => Cookies;
/**
 * An empty Cookies object
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const empty: Cookies;
/**
 * Returns `true` when the `Cookies` collection contains no cookies.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export declare const isEmpty: (self: Cookies) => boolean;
/**
 * Creates a cookie, validating the name, encoded value, domain, path, and finite `maxAge`.
 *
 * **Details**
 *
 * Returns a `CookiesError` in the `Result` failure channel when validation fails.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare function makeCookie(name: string, value: string, options?: Cookie["options"] | undefined): Result.Result<Cookie, CookiesError>;
/**
 * Create a new cookie, throwing an error if invalid
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeCookieUnsafe: (name: string, value: string, options?: Cookie["options"] | undefined) => Cookie;
/**
 * Adds a cookie to a Cookies object
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setCookie: {
    /**
     * Adds a cookie to a Cookies object
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookie: Cookie): (self: Cookies) => Cookies;
    /**
     * Adds a cookie to a Cookies object
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, cookie: Cookie): Cookies;
};
/**
 * Adds multiple cookies to a Cookies object
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setAllCookie: {
    /**
     * Adds multiple cookies to a Cookies object
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Iterable<Cookie>): (self: Cookies) => Cookies;
    /**
     * Adds multiple cookies to a Cookies object
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, cookies: Iterable<Cookie>): Cookies;
};
/**
 * Combines two Cookies objects, removing duplicates from the first
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const merge: {
    /**
     * Combines two Cookies objects, removing duplicates from the first
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (that: Cookies): (self: Cookies) => Cookies;
    /**
     * Combines two Cookies objects, removing duplicates from the first
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, that: Cookies): Cookies;
};
/**
 * Removes a cookie by name
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const remove: {
    /**
     * Removes a cookie by name
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string): (self: Cookies) => Cookies;
    /**
     * Removes a cookie by name
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string): Cookies;
};
/**
 * Gets a cookie from a Cookies object safely.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const get: {
    /**
     * Gets a cookie from a Cookies object safely.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string): (self: Cookies) => Option.Option<Cookie>;
    /**
     * Gets a cookie from a Cookies object safely.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string): Option.Option<Cookie>;
};
/**
 * Gets the decoded value of a cookie by name safely.
 *
 * **Details**
 *
 * Returns `Option.none()` when the cookie is not present.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const getValue: {
    /**
     * Gets the decoded value of a cookie by name safely.
     *
     * **Details**
     *
     * Returns `Option.none()` when the cookie is not present.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string): (self: Cookies) => Option.Option<string>;
    /**
     * Gets the decoded value of a cookie by name safely.
     *
     * **Details**
     *
     * Returns `Option.none()` when the cookie is not present.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string): Option.Option<string>;
};
/**
 * Creates and adds a cookie safely by name and value.
 *
 * **Details**
 *
 * The cookie fields are validated first; invalid input returns a `CookiesError` in the `Result` failure channel.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const set: {
    /**
     * Creates and adds a cookie safely by name and value.
     *
     * **Details**
     *
     * The cookie fields are validated first; invalid input returns a `CookiesError` in the `Result` failure channel.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, value: string, options?: Cookie["options"]): (self: Cookies) => Result.Result<Cookies, CookiesError>;
    /**
     * Creates and adds a cookie safely by name and value.
     *
     * **Details**
     *
     * The cookie fields are validated first; invalid input returns a `CookiesError` in the `Result` failure channel.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string, value: string, options?: Cookie["options"]): Result.Result<Cookies, CookiesError>;
};
/**
 * Creates and adds a cookie by name and value, throwing if the cookie fields are invalid.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setUnsafe: {
    /**
     * Creates and adds a cookie by name and value, throwing if the cookie fields are invalid.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, value: string, options?: Cookie["options"]): (self: Cookies) => Cookies;
    /**
     * Creates and adds a cookie by name and value, throwing if the cookie fields are invalid.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string, value: string, options?: Cookie["options"]): Cookies;
};
/**
 * Adds an expired cookie safely with an empty value, `Max-Age=0`, and an epoch `Expires` value.
 *
 * **Details**
 *
 * Returns a `CookiesError` in the `Result` failure channel when the name or options are invalid.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const expireCookie: {
    /**
     * Adds an expired cookie safely with an empty value, `Max-Age=0`, and an epoch `Expires` value.
     *
     * **Details**
     *
     * Returns a `CookiesError` in the `Result` failure channel when the name or options are invalid.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, options?: Omit<NonNullable<Cookie["options"]>, "expires" | "maxAge">): (self: Cookies) => Result.Result<Cookies, CookiesError>;
    /**
     * Adds an expired cookie safely with an empty value, `Max-Age=0`, and an epoch `Expires` value.
     *
     * **Details**
     *
     * Returns a `CookiesError` in the `Result` failure channel when the name or options are invalid.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string, options?: Omit<NonNullable<Cookie["options"]>, "expires" | "maxAge">): Result.Result<Cookies, CookiesError>;
};
/**
 * Adds an expired cookie to a Cookies object, throwing an error if invalid
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const expireCookieUnsafe: {
    /**
     * Adds an expired cookie to a Cookies object, throwing an error if invalid
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (name: string, options?: Omit<NonNullable<Cookie["options"]>, "expires" | "maxAge">): (self: Cookies) => Cookies;
    /**
     * Adds an expired cookie to a Cookies object, throwing an error if invalid
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, name: string, options?: Omit<NonNullable<Cookie["options"]>, "expires" | "maxAge">): Cookies;
};
/**
 * Creates and adds multiple cookies safely from name/value/options tuples.
 *
 * **Details**
 *
 * If any tuple is invalid, returns the first `CookiesError` and leaves the original collection unchanged.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setAll: {
    /**
     * Creates and adds multiple cookies safely from name/value/options tuples.
     *
     * **Details**
     *
     * If any tuple is invalid, returns the first `CookiesError` and leaves the original collection unchanged.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Iterable<readonly [name: string, value: string, options?: Cookie["options"]]>): (self: Cookies) => Result.Result<Cookies, CookiesError>;
    /**
     * Creates and adds multiple cookies safely from name/value/options tuples.
     *
     * **Details**
     *
     * If any tuple is invalid, returns the first `CookiesError` and leaves the original collection unchanged.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, cookies: Iterable<readonly [name: string, value: string, options?: Cookie["options"]]>): Result.Result<Cookies, CookiesError>;
};
/**
 * Adds multiple cookies to a Cookies object, throwing an error if invalid
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const setAllUnsafe: {
    /**
     * Adds multiple cookies to a Cookies object, throwing an error if invalid
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (cookies: Iterable<readonly [name: string, value: string, options?: Cookie["options"]]>): (self: Cookies) => Cookies;
    /**
     * Adds multiple cookies to a Cookies object, throwing an error if invalid
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    (self: Cookies, cookies: Iterable<readonly [name: string, value: string, options?: Cookie["options"]]>): Cookies;
};
/**
 * Serializes a cookie into a string.
 *
 * **Details**
 *
 * Adapted from https://github.com/fastify/fastify-cookie under MIT License
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare function serializeCookie(self: Cookie): string;
/**
 * Serializes a `Cookies` object into a Cookie header.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const toCookieHeader: (self: Cookies) => string;
/**
 * Converts a `Cookies` collection to a record of decoded cookie values keyed by cookie name.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const toRecord: (self: Cookies) => Record<string, string>;
/**
 * Serializes a `Cookies` collection into an array of `Set-Cookie` header values.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export declare const toSetCookieHeaders: (self: Cookies) => Array<string>;
/**
 * Parses a cookie header into a record of key-value pairs
 *
 * **Details**
 *
 * Adapted from https://github.com/fastify/fastify-cookie under MIT License
 *
 * @stability unstable
 * @category decoding
 * @since 4.0.0
 */
export declare function parseHeader(header: string): Record<string, string>;
export {};
//# sourceMappingURL=Cookies.d.ts.map