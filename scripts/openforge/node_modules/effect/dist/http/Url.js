/**
 * Parses and edits platform `URL` values.
 *
 * The HTTP modules use the standard `URL` object as their URL representation.
 * This module adds safe parsing and helpers that return updated copies when
 * changing credentials, host, path, protocol, query, or hash parts. Query
 * strings can also be read or updated through `UrlParams`.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cause from "../Cause.js";
import * as Data from "../Data.js";
import { dual } from "../Function.js";
import * as Redacted from "../Redacted.js";
import * as Result from "../Result.js";
import * as UrlParams from "./UrlParams.js";
/**
 * Error returned when constructing a `URL` fails.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class UrlError extends /*#__PURE__*/Data.TaggedError("UrlError") {}
/**
 * Creates a `URL` safely by appending `UrlParams` and an optional hash to a URL string.
 *
 * **Details**
 *
 * Returns a `Result` that fails with `UrlError` if the URL cannot be constructed.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (url, params, hash) => Result.try({
  try: () => {
    const urlInstance = new URL(url, baseUrl());
    for (let i = 0; i < params.params.length; i++) {
      const [key, value] = params.params[i];
      if (value !== undefined) {
        urlInstance.searchParams.append(key, value);
      }
    }
    if (hash !== undefined) {
      urlInstance.hash = hash;
    }
    return urlInstance;
  },
  catch: cause => new UrlError({
    cause
  })
});
const baseUrl = () => {
  if ("location" in globalThis && globalThis.location !== undefined && globalThis.location.origin !== undefined && globalThis.location.pathname !== undefined) {
    return location.origin + location.pathname;
  }
  return undefined;
};
/**
 * Parses a URL string safely into a `URL` object, returning a `Result` type for
 * error handling.
 *
 * **Details**
 *
 * This function converts a string into a `URL` object, enabling safe URL
 * parsing with built-in error handling. If the string is invalid or fails to
 * parse, this function does not throw an error; instead, it wraps the error in
 * a `IllegalArgumentError` and returns it as the `Failure` value of an
 * `Result`. The `Success` value contains the successfully parsed `URL`.
 *
 * An optional `base` parameter can be provided to resolve relative URLs. If
 * specified, the function interprets the input `url` as relative to this
 * `base`. This is especially useful when dealing with URLs that might not be
 * fully qualified.
 *
 * **Example** (Parsing absolute and relative URLs)
 *
 * ```ts import.meta.vitest
 * import { Result } from "effect"
 * import { Url } from "effect/http"
 *
 * // Parse an absolute URL
 * //
 * //      ┌─── Result<URL, IllegalArgumentError>
 * //      ▼
 * const parsed = Url.fromString("https://example.com/path")
 *
 * Result.map(parsed, (url) => url.toString()) // => Result.succeed("https://example.com/path")
 *
 * // Parse a relative URL with a base
 * const relativeParsed = Url.fromString("/relative-path", "https://example.com")
 *
 * Result.map(relativeParsed, (url) => url.toString()) // => Result.succeed("https://example.com/relative-path")
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromString = (url, base) => Result.try({
  try: () => new URL(url, base),
  catch: () => new Cause.IllegalArgumentError(`Invalid URL: "${url}"${base !== undefined ? ` with base "${base}"` : ""}`)
});
/**
 * Updates a cloned `URL` with a callback, allowing multiple changes at once.
 *
 * **Example** (Mutating URL credentials)
 *
 * ```ts import.meta.vitest
 * import { Url } from "effect/http"
 *
 * const myUrl = new URL("https://example.com")
 *
 * const mutatedUrl = Url.mutate(myUrl, (url) => {
 *   url.username = "user"
 *   url.password = "pass"
 * })
 *
 * mutatedUrl.toString() // => "https://user:pass@example.com/"
 * ```
 *
 * @stability unstable
 * @category transforming
 * @since 4.0.0
 */
export const mutate = /*#__PURE__*/dual(2, (self, f) => {
  const copy = new URL(self);
  f(copy);
  return copy;
});
/**
 * @internal
 */
const immutableURLSetter = property => dual(2, (url, value) => mutate(url, url => {
  url[property] = value;
}));
/**
 * Updates the hash fragment of the URL.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setHash = /*#__PURE__*/immutableURLSetter("hash");
/**
 * Updates the host (domain and port) of the URL.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setHost = /*#__PURE__*/immutableURLSetter("host");
/**
 * Updates the domain of the URL without modifying the port.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setHostname = /*#__PURE__*/immutableURLSetter("hostname");
/**
 * Replaces the entire URL string.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setHref = /*#__PURE__*/immutableURLSetter("href");
/**
 * Updates the password used for authentication.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setPassword = /*#__PURE__*/dual(2, (url, password) => mutate(url, url => {
  url.password = typeof password === "string" ? password : Redacted.value(password);
}));
/**
 * Updates the path of the URL.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setPathname = /*#__PURE__*/immutableURLSetter("pathname");
/**
 * Updates the port of the URL.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setPort = /*#__PURE__*/immutableURLSetter("port");
/**
 * Updates the protocol (e.g., `http`, `https`).
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setProtocol = /*#__PURE__*/immutableURLSetter("protocol");
/**
 * Updates the query string of the URL.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setSearch = /*#__PURE__*/immutableURLSetter("search");
/**
 * Updates the username used for authentication.
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setUsername = /*#__PURE__*/immutableURLSetter("username");
/**
 * Updates the query parameters of a URL.
 *
 * **Details**
 *
 * This function allows you to set or replace the query parameters of a `URL`
 * object using the provided `UrlParams`. It creates a new `URL` object with the
 * updated parameters, leaving the original object unchanged.
 *
 * **Example** (Replacing query parameters)
 *
 * ```ts import.meta.vitest
 * import { Url, UrlParams } from "effect/http"
 *
 * const myUrl = new URL("https://example.com?foo=bar")
 *
 * // Write parameters
 * const updatedUrl = Url.setUrlParams(
 *   myUrl,
 *   UrlParams.fromInput([["key", "value"]])
 * )
 *
 * updatedUrl.toString() // => "https://example.com/?key=value"
 * ```
 *
 * @stability unstable
 * @category setters
 * @since 4.0.0
 */
export const setUrlParams = /*#__PURE__*/dual(2, (url, urlParams) => mutate(url, url => {
  url.search = UrlParams.toString(UrlParams.fromInput(urlParams));
}));
/**
 * Retrieves the query parameters from a URL.
 *
 * **Details**
 *
 * This function extracts the query parameters from a `URL` object and returns
 * them as `UrlParams`. The resulting structure can be easily manipulated or
 * inspected.
 *
 * **Example** (Reading query parameters)
 *
 * ```ts import.meta.vitest
 * import { Url, UrlParams } from "effect/http"
 *
 * const myUrl = new URL("https://example.com?foo=bar")
 *
 * // Read parameters
 * const params = Url.urlParams(myUrl)
 *
 * UrlParams.toString(params) // => "foo=bar"
 * ```
 *
 * @stability unstable
 * @category getters
 * @since 4.0.0
 */
export const urlParams = url => UrlParams.fromInput(url.searchParams);
/**
 * Reads the query parameters of a URL, modifies them, and updates the URL.
 *
 * **Details**
 *
 * This function provides a functional way to interact with query parameters by
 * reading the current parameters, applying a transformation function, and then
 * writing the updated parameters back to the URL. It returns a new `URL` object
 * with the modified parameters, ensuring immutability.
 *
 * **Example** (Modifying query parameters)
 *
 * ```ts import.meta.vitest
 * import { Url, UrlParams } from "effect/http"
 *
 * const myUrl = new URL("https://example.com?foo=bar")
 *
 * const changedUrl = Url.modifyUrlParams(myUrl, UrlParams.append("key", "value"))
 *
 * changedUrl.toString() // => "https://example.com/?foo=bar&key=value"
 * ```
 *
 * @stability unstable
 * @category transforming
 * @since 4.0.0
 */
export const modifyUrlParams = /*#__PURE__*/dual(2, (url, f) => mutate(url, url => {
  const params = f(UrlParams.fromInput(url.searchParams));
  url.search = UrlParams.toString(params);
}));
//# sourceMappingURL=Url.js.map