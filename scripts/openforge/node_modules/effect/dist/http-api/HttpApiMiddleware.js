/**
 * Declares middleware for schema-driven HTTP APIs.
 *
 * HTTP API middleware wraps endpoint execution on the server and, when the API
 * requires it, can also wrap requests made by generated clients. It is used for
 * cross-cutting behavior that belongs to the API contract, such as
 * authentication, authorization, logging, tracing, rate limiting,
 * request-scoped services, schema-error handling, and client request
 * decoration. This module defines the middleware service keys and helpers used
 * by `HttpApi` declarations.
 *
 * @stability unstable
 * @since 4.0.0
 */
/** @effect-diagnostics floatingEffect:skip-file */
/** @effect-diagnostics classSelfMismatch:off */
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import { getStackTraceLimit, setStackTraceLimit } from "../internal/stackTraceLimit.js";
import * as Layer from "../Layer.js";
import { hasProperty } from "../Predicate.js";
import { Scope } from "../Scope.js";
import { HttpApiSchemaError } from "./HttpApiError.js";
const TypeId = "~effect/http-api/HttpApiMiddleware";
const SecurityTypeId = "~effect/http-api/HttpApiMiddleware/Security";
/**
 * Returns `true` when an HTTP API middleware service is security middleware.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isSecurity = u => hasProperty(u, SecurityTypeId);
/**
 * Creates a `Context.Service` class for an HTTP API middleware implementation.
 *
 * **When to use**
 *
 * Use when you need an HTTP API middleware service whose configuration declares
 * required services, provided services, typed error schemas, security schemes,
 * client errors, or a matching client middleware requirement.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Service = () => (id, options) => {
  const limit = getStackTraceLimit();
  let creationError;
  if (limit !== 0) {
    setStackTraceLimit(2);
    creationError = new globalThis.Error();
    setStackTraceLimit(limit);
  }
  class Service extends Context.Service()(id) {}
  const self = Service;
  Object.defineProperty(Service, "stack", {
    get() {
      return creationError?.stack;
    }
  });
  self[TypeId] = TypeId;
  self.error = getError(options?.error);
  self.requiredForClient = options?.requiredForClient ?? false;
  if (options?.security !== undefined) {
    if (Object.keys(options.security).length === 0) {
      throw new Error("HttpApiMiddleware.Service: security object must not be empty");
    }
    self[SecurityTypeId] = SecurityTypeId;
    self.security = options.security;
  }
  return self;
};
function getError(error) {
  if (error === undefined) return new Set();
  return new Set(Array.isArray(error) ? error : [error]);
}
/**
 * Creates a middleware layer that transforms `HttpApiSchemaError` failures.
 *
 * **Details**
 *
 * The middleware catches schema errors produced while running an endpoint and uses
 * the supplied `transform` function to convert them into the middleware's declared
 * error schema.
 *
 * **Example** (Mapping schema errors to custom errors)
 *
 * ```ts import.meta.vitest
 * import { Effect, Schema, type Types } from "effect"
 * import { HttpRouter, HttpServerResponse } from "effect/http"
 * import {
 *   HttpApiEndpoint,
 *   HttpApiError,
 *   HttpApiGroup,
 *   HttpApiMiddleware
 * } from "effect/http-api"
 *
 * class CustomError extends Schema.TaggedError<CustomError>()("CustomError", {}) {}
 *
 * class ErrorHandler extends HttpApiMiddleware.Service<ErrorHandler>()("api/ErrorHandler", {
 *   error: CustomError
 * }) {}
 *
 * const messages: Array<string> = []
 * const ErrorHandlerLayer = HttpApiMiddleware.layerSchemaErrorTransform(
 *   ErrorHandler,
 *   (schemaError) =>
 *     Effect.sync(() => messages.push(`Mapping ${schemaError.kind} schema error`)).pipe(
 *       Effect.andThen(Effect.fail(new CustomError()))
 *     )
 * )
 *
 * const endpoint = HttpApiEndpoint.get("example", "/")
 * const group = HttpApiGroup.make("examples").add(endpoint)
 * const middlewareContext = {
 *   endpoint: endpoint as unknown as HttpApiEndpoint.Top,
 *   group: group as unknown as HttpApiGroup.Top
 * }
 * const Routes = HttpRouter.add(
 *   "GET",
 *   "/",
 *   Effect.gen(function*() {
 *     const applySchemaErrorTransform = yield* ErrorHandler
 *     const schemaError = yield* HttpApiError.HttpApiSchemaError.wrap(
 *       "Body",
 *       Schema.decodeUnknownEffect(Schema.String)(42)
 *     ).pipe(Effect.flip)
 *     const failingResponse = Effect.fail(schemaError as unknown as Types.unhandled)
 *     const result = yield* applySchemaErrorTransform(failingResponse, middlewareContext).pipe(
 *       Effect.match({
 *         onFailure: (error) => error instanceof CustomError ? error._tag : "UnexpectedError",
 *         onSuccess: () => "Success"
 *       })
 *     )
 *     return HttpServerResponse.text(result)
 *   })
 * ).pipe(HttpRouter.provideRequest(ErrorHandlerLayer))
 *
 * const program = Effect.acquireUseRelease(
 *   Effect.sync(() => HttpRouter.toWebHandler(Routes, { disableLogger: true })),
 *   ({ handler }) =>
 *     Effect.gen(function*() {
 *       const response = yield* Effect.promise(() => handler(new Request("http://localhost/")))
 *       const body = yield* Effect.promise(() => response.text())
 *       return body
 *     }),
 *   ({ dispose }) => Effect.promise(dispose)
 * )
 *
 * const body = await Effect.runPromise(program)
 * const result = [messages, body] // => [["Mapping Body schema error"], "CustomError"]
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerSchemaErrorTransform = (service, transform) => Layer.succeed(service, (httpEffect, options) => Effect.catch(httpEffect, e => HttpApiSchemaError.is(e) ? transform(e, options) : Effect.fail(e)));
/**
 * Provides a client-side middleware implementation for a middleware that is required by generated clients.
 *
 * **Details**
 *
 * The layer captures the surrounding services and makes the middleware available
 * through the `ForClient` service marker used by HTTP API clients.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export const layerClient = (tag, service) => Layer.effectContext(Effect.gen(function* () {
  const services = (yield* Effect.context()).pipe(Context.omit(Scope));
  const middleware = Effect.isEffect(service) ? yield* service : service;
  return Context.makeUnsafe(new Map([[`${tag.key}/Client`, options => Effect.updateContext(middleware(options), requestContext => Context.merge(services, requestContext))]]));
}));
//# sourceMappingURL=HttpApiMiddleware.js.map