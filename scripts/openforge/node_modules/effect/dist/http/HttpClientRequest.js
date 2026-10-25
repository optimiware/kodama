import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import { dual } from "../Function.js";
import * as Inspectable from "../Inspectable.js";
import { stringOrRedacted } from "../internal/redacted.js";
import * as Option from "../Option.js";
import { pipeArguments } from "../Pipeable.js";
import { hasProperty } from "../Predicate.js";
import { redact } from "../Redactable.js";
import * as Result from "../Result.js";
import * as Stream from "../Stream.js";
import * as Headers from "./Headers.js";
import * as HttpBody from "./HttpBody.js";
import { hasBody } from "./HttpMethod.js";
import * as bodyInternal from "./internal/httpBody.js";
import * as Url from "./Url.js";
import * as UrlParams from "./UrlParams.js";
const TypeId = "~effect/http/HttpClientRequest";
/**
 * Returns `true` when a value is an `HttpClientRequest`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpClientRequest = u => hasProperty(u, TypeId);
const Proto = {
  [TypeId]: TypeId,
  ...Inspectable.BaseProto,
  toJSON() {
    return {
      _id: "HttpClientRequest",
      method: this.method,
      url: this.url,
      urlParams: this.urlParams,
      hash: this.hash,
      headers: redact(this.headers),
      body: this.body.toJSON()
    };
  },
  pipe() {
    return pipeArguments(this, arguments);
  }
};
/**
 * Constructs an `HttpClientRequest` from fully normalized request components.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export function makeWith(method, url, urlParams, hash, headers, body) {
  const self = Object.create(Proto);
  self.method = method;
  self.url = url;
  self.urlParams = urlParams;
  self.hash = hash;
  self.headers = headers;
  self.body = body;
  return self;
}
/**
 * An empty `GET` request with no URL, query parameters, hash, headers, or body.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const empty = /*#__PURE__*/makeWith("GET", "", UrlParams.empty, /*#__PURE__*/Option.none(), Headers.empty, HttpBody.empty);
/**
 * Creates a request constructor for the specified HTTP method.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = method => (url, options) => modify(empty, {
  method,
  url,
  ...(options ?? undefined)
});
/**
 * Creates a `GET` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const get = /*#__PURE__*/make("GET");
/**
 * Creates a `QUERY` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const query = /*#__PURE__*/make("QUERY");
/**
 * Creates a `POST` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const post = /*#__PURE__*/make("POST");
/**
 * Creates a `PATCH` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const patch = /*#__PURE__*/make("PATCH");
/**
 * Creates a `PUT` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const put = /*#__PURE__*/make("PUT");
const del = /*#__PURE__*/make("DELETE");
/**
 * @stability unstable
 */
export {
/**
 * Creates a `DELETE` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
del as delete };
/**
 * Creates a `HEAD` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const head = /*#__PURE__*/make("HEAD");
/**
 * Creates an `OPTIONS` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const options = /*#__PURE__*/make("OPTIONS");
/**
 * Creates a `TRACE` request for the specified URL.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const trace = /*#__PURE__*/make("TRACE");
/**
 * Applies request options to an `HttpClientRequest`, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const modify = /*#__PURE__*/dual(2, (self, options) => {
  let result = self;
  if (options.method) {
    result = setMethod(result, options.method);
  }
  if (options.url) {
    result = setUrl(result, options.url);
  }
  if (options.headers) {
    result = setHeaders(result, options.headers);
  }
  if (options.urlParams) {
    result = setUrlParams(result, options.urlParams);
  }
  if (options.hash) {
    result = setHash(result, options.hash);
  }
  if (options.body) {
    result = setBody(result, options.body);
  }
  if (options.accept) {
    result = accept(result, options.accept);
  }
  if (options.acceptJson) {
    result = acceptJson(result);
  }
  return result;
});
/**
 * Sets the HTTP method on a request, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setMethod = /*#__PURE__*/dual(2, (self, method) => makeWith(method, self.url, self.urlParams, self.hash, self.headers, self.body));
/**
 * Sets a single request header, replacing any existing value for that header.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setHeader = /*#__PURE__*/dual(3, (self, key, value) => makeWith(self.method, self.url, self.urlParams, self.hash, Headers.set(self.headers, key, value), self.body));
/**
 * Sets multiple request headers from an input collection, replacing existing values with matching names.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setHeaders = /*#__PURE__*/dual(2, (self, input) => makeWith(self.method, self.url, self.urlParams, self.hash, Headers.setAll(self.headers, input), self.body));
/**
 * Transforms the request headers with the provided function, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const updateHeaders = /*#__PURE__*/dual(2, (self, f) => makeWith(self.method, self.url, self.urlParams, self.hash, f(self.headers), self.body));
/**
 * Removes a single request header by name, returning a new request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const removeHeader = /*#__PURE__*/dual(2, (self, key) => updateHeaders(self, Headers.remove(key)));
/**
 * Sets the `Authorization` header using HTTP Basic authentication credentials.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const basicAuth = /*#__PURE__*/dual(3, (self, username, password) => setHeader(self, "Authorization", `Basic ${btoa(`${stringOrRedacted(username)}:${stringOrRedacted(password)}`)}`));
/**
 * Sets the `Authorization` header using a bearer token.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bearerToken = /*#__PURE__*/dual(2, (self, token) => setHeader(self, "Authorization", `Bearer ${stringOrRedacted(token)}`));
/**
 * Sets the `Accept` header to the specified media type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const accept = /*#__PURE__*/dual(2, (self, mediaType) => setHeader(self, "Accept", mediaType));
/**
 * Sets the `Accept` header to `application/json`.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const acceptJson = /*#__PURE__*/accept("application/json");
/**
 * Sets the request URL. When given a `URL`, its search parameters and hash are extracted into the request's structured fields.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setUrl = /*#__PURE__*/dual(2, (self, url) => {
  if (typeof url === "string") {
    return makeWith(self.method, url, self.urlParams, self.hash, self.headers, self.body);
  }
  const clone = new URL(url.toString());
  const urlParams = UrlParams.fromInput(clone.searchParams);
  const hash = Option.fromNullishOr(clone.hash === "" ? undefined : clone.hash.slice(1));
  clone.search = "";
  clone.hash = "";
  return makeWith(self.method, clone.toString(), urlParams, hash, self.headers, self.body);
});
/**
 * Prepends a URL segment to the request URL, inserting or trimming one slash as needed.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const prependUrl = /*#__PURE__*/dual(2, (self, path) => {
  if (path === "") return self;
  return makeWith(self.method, joinSegments(path, self.url), self.urlParams, self.hash, self.headers, self.body);
});
/**
 * Appends a URL segment to the request URL, inserting or trimming one slash as needed.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const appendUrl = /*#__PURE__*/dual(2, (self, path) => {
  if (path === "") return self;
  return makeWith(self.method, joinSegments(self.url, path), self.urlParams, self.hash, self.headers, self.body);
});
const joinSegments = (first, second) => {
  const endsWithSlash = first.endsWith("/");
  const startsWithSlash = second.startsWith("/");
  const needsTrim = endsWithSlash && startsWithSlash;
  const needsSlash = !endsWithSlash && !startsWithSlash;
  return needsTrim ? first + second.slice(1) : needsSlash ? first + "/" + second : first + second;
};
/**
 * Updates the request URL by applying a function to the current URL string.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const updateUrl = /*#__PURE__*/dual(2, (self, f) => makeWith(self.method, f(self.url), self.urlParams, self.hash, self.headers, self.body));
/**
 * Sets one query parameter, replacing existing values for that parameter name.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setUrlParam = /*#__PURE__*/dual(3, (self, key, value) => makeWith(self.method, self.url, UrlParams.set(self.urlParams, key, value), self.hash, self.headers, self.body));
/**
 * Sets query parameters from an input collection, replacing existing values for matching names.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setUrlParams = /*#__PURE__*/dual(2, (self, input) => makeWith(self.method, self.url, UrlParams.setAll(self.urlParams, input), self.hash, self.headers, self.body));
/**
 * Appends one query parameter value without removing existing values for the same name.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const appendUrlParam = /*#__PURE__*/dual(3, (self, key, value) => makeWith(self.method, self.url, UrlParams.append(self.urlParams, key, value), self.hash, self.headers, self.body));
/**
 * Appends query parameters from an input collection without removing existing values for matching names.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const appendUrlParams = /*#__PURE__*/dual(2, (self, input) => makeWith(self.method, self.url, UrlParams.appendAll(self.urlParams, input), self.hash, self.headers, self.body));
/**
 * Sets the URL fragment on a request without the leading `#`.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setHash = /*#__PURE__*/dual(2, (self, hash) => makeWith(self.method, self.url, self.urlParams, Option.some(hash), self.headers, self.body));
/**
 * Removes the URL fragment from a request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const removeHash = self => makeWith(self.method, self.url, self.urlParams, Option.none(), self.headers, self.body);
/**
 * Sets the request body and updates `Content-Type` and `Content-Length` headers from the body metadata when available.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setBody = /*#__PURE__*/dual(2, (self, body) => {
  return makeWith(self.method, self.url, self.urlParams, self.hash, bodyInternal.updateHeaders(self.headers, body), body);
});
/**
 * Sets a `Uint8Array` request body with an optional content type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyUint8Array = /*#__PURE__*/dual(args => isHttpClientRequest(args[0]), (self, body, contentType) => setBody(self, HttpBody.uint8Array(body, contentType)));
/**
 * Sets a text request body with an optional content type.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyText = /*#__PURE__*/dual(args => isHttpClientRequest(args[0]), (self, body, contentType) => setBody(self, HttpBody.text(body, contentType)));
/**
 * Encodes a value as a JSON request body and sets it on the request, failing with `HttpBodyError` if encoding fails.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyJson = /*#__PURE__*/dual(2, (self, body) => Effect.map(HttpBody.json(body), body => setBody(self, body)));
/**
 * Sets a JSON request body using unsafe JSON encoding.
 *
 * **When to use**
 *
 * Use when the request body is known to be JSON-serializable and a synchronous
 * `HttpClientRequest` result is needed.
 *
 * **Gotchas**
 *
 * JSON encoding may throw instead of failing in the Effect error channel.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyJsonUnsafe = /*#__PURE__*/dual(2, (self, body) => setBody(self, HttpBody.jsonUnsafe(body)));
/**
 * Creates a schema-based JSON body encoder that sets the encoded value on a request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const schemaBodyJson = (schema, options) => {
  const encode = HttpBody.jsonSchema(schema, options);
  return dual(2, (self, body) => Effect.map(encode(body), body => setBody(self, body)));
};
/**
 * Sets an `application/x-www-form-urlencoded` request body from URL parameter input.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyUrlParams = /*#__PURE__*/dual(2, (self, input) => setBody(self, HttpBody.urlParams(UrlParams.fromInput(input))));
/**
 * Sets a `FormData` request body.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyFormData = /*#__PURE__*/dual(2, (self, body) => setBody(self, HttpBody.formData(body)));
/**
 * Creates a `FormData` request body from record-style entries and sets it on the request.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyFormDataRecord = /*#__PURE__*/dual(2, (self, entries) => setBody(self, HttpBody.formDataRecord(entries)));
/**
 * Sets a streaming `Uint8Array` request body with optional content type and content length metadata.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyStream = /*#__PURE__*/dual(args => isHttpClientRequest(args[0]), (self, body, options) => setBody(self, HttpBody.stream(body, options?.contentType, options?.contentLength)));
/**
 * Creates a file-backed request body from a filesystem path and sets it on the request.
 *
 * **Details**
 *
 * Uses {@link HttpBody.file} to validate ranges lazily and calculate the EOF-clamped content length with exact
 * bigint arithmetic. Invalid range inputs or a final length above `Number.MAX_SAFE_INTEGER` fail with
 * `PlatformError` / `BadArgument`. Larger sizes, offsets, and byte counts are valid when the final length is
 * representable as a safe integer. The request's Content-Length header contains that exact length.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const bodyFile = /*#__PURE__*/dual(args => isHttpClientRequest(args[0]), (self, path, options) => Effect.map(HttpBody.file(path, options), body => setBody(self, body)));
/**
 * Builds a `URL` from the request URL, query parameters, and hash, returning `Option.none()` if the URL is invalid.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export function toUrl(self) {
  const r = Url.make(self.url, self.urlParams, Option.getOrUndefined(self.hash));
  if (Result.isSuccess(r)) {
    return Option.some(r.success);
  }
  return Option.none();
}
/**
 * Converts a Web `Request` into an `HttpClientRequest`, preserving method, URL, headers, and supported request bodies.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const fromWeb = request => {
  const method = request.method.toUpperCase();
  return modify(empty, {
    method,
    url: new URL(request.url),
    headers: request.headers,
    body: fromWebBody(request, method)
  });
};
const fromWebBody = (request, method) => {
  if (!hasBody(method) || request.body === null) {
    return HttpBody.empty;
  }
  return HttpBody.raw(request.body, {
    contentType: request.headers.get("content-type") ?? undefined,
    contentLength: bodyInternal.parseContentLength(request.headers.get("content-length"))
  });
};
/**
 * Converts an `HttpClientRequest` safely to a Web `Request` as a `Result`, failing when the request URL is invalid.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toWebResult = (self, options) => {
  const url = Url.make(self.url, self.urlParams, Option.getOrUndefined(self.hash));
  if (Result.isFailure(url)) {
    return Result.fail(url.failure);
  }
  const requestInit = {
    method: self.method,
    headers: self.headers
  };
  if (options?.signal) {
    requestInit.signal = options.signal;
  }
  if (hasBody(self.method)) {
    switch (self.body._tag) {
      case "Empty":
        {
          break;
        }
      case "Raw":
        {
          requestInit.body = self.body.body;
          if (isReadableStream(self.body.body)) {
            ;
            requestInit.duplex = "half";
          }
          break;
        }
      case "Uint8Array":
        {
          requestInit.body = self.body.body;
          break;
        }
      case "FormData":
        {
          requestInit.body = self.body.formData;
          break;
        }
      case "Stream":
        {
          requestInit.body = Stream.toReadableStreamWith(self.body.stream, options?.context ?? Context.empty());
          requestInit.duplex = "half";
          break;
        }
    }
  }
  return Result.try({
    try: () => new Request(url.success, requestInit),
    catch: cause => new Url.UrlError({
      cause
    })
  });
};
const isReadableStream = u => typeof ReadableStream !== "undefined" && u instanceof ReadableStream;
/**
 * Converts an `HttpClientRequest` to a Web `Request`, failing with `UrlError` when the request URL is invalid.
 *
 * @stability unstable
 * @category converting
 * @since 4.0.0
 */
export const toWeb = (self, options) => Effect.contextWith(context => Effect.fromResult(toWebResult(self, {
  context: context,
  signal: options?.signal
})));
//# sourceMappingURL=HttpClientRequest.js.map