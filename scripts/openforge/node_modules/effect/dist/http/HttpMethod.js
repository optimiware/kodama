/**
 * Defines supported HTTP method names for the unstable HTTP modules.
 *
 * Values are uppercase string literals such as `"GET"` and `"POST"`, matching
 * the method tokens used by HTTP requests and routes. This module also includes
 * helpers for checking whether a method can carry a request body and whether an
 * unknown value is one of the supported methods.
 *
 * @stability unstable
 * @since 4.0.0
 */
/**
 * Returns `true` when a method can carry a request body and narrows it to `HttpMethod.WithBody`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const hasBody = method => method !== "GET" && method !== "HEAD" && method !== "OPTIONS" && method !== "TRACE";
/**
 * Provides a readonly set containing every supported `HttpMethod` literal.
 *
 * **When to use**
 *
 * Use when you need to iterate over or test membership against every supported
 * HTTP method literal.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const all = /*#__PURE__*/new Set(["GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS", "TRACE", "QUERY"]);
/**
 * Provides tuples mapping each supported HTTP method to its short
 * request-constructor name.
 *
 * **When to use**
 *
 * Use when you need the mapping from supported HTTP method literals to their
 * short request-constructor names.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const allShort = [["GET", "get"], ["POST", "post"], ["PUT", "put"], ["DELETE", "del"], ["PATCH", "patch"], ["HEAD", "head"], ["OPTIONS", "options"], ["TRACE", "trace"], ["QUERY", "query"]];
/**
 * Checks whether a value is a `HttpMethod`.
 *
 * **Example** (Checking HTTP method values)
 *
 * ```ts import.meta.vitest
 * import { HttpMethod } from "effect/http"
 *
 * HttpMethod.isHttpMethod("GET") // => true
 * HttpMethod.isHttpMethod("get") // => false
 * HttpMethod.isHttpMethod(1) // => false
 * ```
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpMethod = u => all.has(u);
//# sourceMappingURL=HttpMethod.js.map