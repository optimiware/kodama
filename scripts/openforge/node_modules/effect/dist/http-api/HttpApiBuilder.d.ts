import * as Effect from "../Effect.ts";
import type { FileSystem } from "../FileSystem.ts";
import type { Cookie } from "../http/Cookies.ts";
import type * as Etag from "../http/Etag.ts";
import type { HttpPlatform } from "../http/HttpPlatform.ts";
import * as HttpRouter from "../http/HttpRouter.ts";
import * as Request from "../http/HttpServerRequest.ts";
import { HttpServerRequest } from "../http/HttpServerRequest.ts";
import type { HttpServerResponse } from "../http/HttpServerResponse.ts";
import * as Layer from "../Layer.ts";
import type { Path } from "../Path.ts";
import { type Pipeable } from "../Pipeable.ts";
import * as Redacted from "../Redacted.ts";
import * as Scope from "../Scope.ts";
import type { NoInfer } from "../Types.ts";
import * as HttpApi from "./HttpApi.ts";
import * as HttpApiEndpoint from "./HttpApiEndpoint.ts";
import type * as HttpApiGroup from "./HttpApiGroup.ts";
import type * as HttpApiSecurity from "./HttpApiSecurity.ts";
/**
 * Registers an `HttpApi` with a `HttpRouter`.
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: <Id extends string, Groups extends HttpApiGroup.Constraint>(api: HttpApi.HttpApi<Id, Groups>, options?: {
    readonly openapiPath?: `/${string}` | undefined;
}) => Layer.Layer<never, never, Etag.Generator | HttpRouter.HttpRouter | FileSystem | HttpPlatform | Path | HttpApiGroup.ToService<Id, Groups>>;
/**
 * Create a `Layer` that implements all endpoints in an `HttpApi` group.
 *
 * **Details**
 *
 * The `build` function receives an unimplemented `Handlers` instance that can
 * be used to add handlers to the group. Implement endpoints with
 * `handlers.handle`.
 *
 * @stability unstable
 * @category handlers
 * @since 4.0.0
 */
export declare const group: <ApiId extends string, Groups extends HttpApiGroup.Constraint, const Identifier extends HttpApiGroup.Identifier<Groups>, Return>(api: HttpApi.HttpApi<ApiId, Groups>, groupIdentifier: Identifier, build: (handlers: Handlers.FromGroup<HttpApiGroup.WithIdentifier<Groups, Identifier>>) => Handlers.ValidateReturn<Return>) => Layer.Layer<HttpApiGroup.Service<ApiId, Identifier>, Handlers.Error<Return>, Exclude<Handlers.Context<Return>, Scope.Scope>>;
/**
 * Creates a reusable handler for a single endpoint in an API group.
 *
 * **Details**
 *
 * Returns the supplied callback unchanged, with its request, success, and error
 * types inferred from the endpoint, preserving the callback's service
 * requirements. Pass the result to `handlers.handle` when implementing the group.
 *
 * @stability unstable
 * @category handlers
 * @since 4.0.0
 */
export declare const handler: <ApiId extends string, Groups extends HttpApiGroup.Constraint, const GroupIdentifier extends HttpApiGroup.Identifier<Groups>, const EndpointIdentifier extends HttpApiGroup.EndpointsWithIdentifier<Groups, GroupIdentifier>["identifier"], R>(_api: HttpApi.HttpApi<ApiId, Groups>, _groupIdentifier: GroupIdentifier, _endpointIdentifier: EndpointIdentifier, f: HttpApiEndpoint.HandlerWithIdentifier<HttpApiGroup.EndpointsWithIdentifier<Groups, GroupIdentifier>, EndpointIdentifier, HttpApiEndpoint.MiddlewareError<HttpApiEndpoint.WithIdentifier<HttpApiGroup.EndpointsWithIdentifier<Groups, GroupIdentifier>, EndpointIdentifier>>, R>) => HttpApiEndpoint.HandlerWithIdentifier<HttpApiGroup.EndpointsWithIdentifier<Groups, GroupIdentifier>, EndpointIdentifier, HttpApiEndpoint.MiddlewareError<HttpApiEndpoint.WithIdentifier<HttpApiGroup.EndpointsWithIdentifier<Groups, GroupIdentifier>, EndpointIdentifier>>, R>;
declare const HandlersTypeId = "~effect/http-api/HttpApiBuilder/Handlers";
type HandlerRequirements<Endpoint extends HttpApiEndpoint.Constraint, R1, R = HttpApiEndpoint.ExcludeProvided<Endpoint, R1 | HttpApiEndpoint.ServerServices<Endpoint>>> = HttpApiEndpoint.Middleware<Endpoint> | HttpApiEndpoint.MiddlewareServices<Endpoint> | ([R] extends [never] ? never : HttpRouter.Request.From<"Requires", R>);
interface HandlerOptions {
    readonly uninterruptible?: boolean | undefined;
}
type HandleAllEntry<Endpoint extends HttpApiEndpoint.Constraint> = HttpApiEndpoint.Handler<Endpoint, HttpApiEndpoint.MiddlewareError<Endpoint>, unknown> | {
    readonly handler: HttpApiEndpoint.Handler<Endpoint, HttpApiEndpoint.MiddlewareError<Endpoint>, unknown>;
    readonly options?: HandlerOptions | undefined;
};
type HandleAllHandlers<EndpointsByIdentifier extends Record<string, HttpApiEndpoint.Constraint>> = {
    readonly [Identifier in keyof EndpointsByIdentifier]?: HandleAllEntry<EndpointsByIdentifier[Identifier]>;
};
type HandleAllExtraKeys<EndpointsByIdentifier extends Record<string, HttpApiEndpoint.Constraint>, HandlersByIdentifier> = {
    readonly [Identifier in Exclude<keyof HandlersByIdentifier, keyof EndpointsByIdentifier>]: never;
};
type NotHandledIdentifier<Identifier extends PropertyKey, HandledIdentifiers extends PropertyKey> = Identifier extends HandledIdentifiers ? never : unknown;
type HandleAllEntryHandler<Entry> = Entry extends {
    readonly handler: infer Handler;
} ? Handler : Entry;
type HandleAllRequirements<EndpointsByIdentifier extends Record<string, HttpApiEndpoint.Constraint>, HandlersByIdentifier extends HandleAllHandlers<EndpointsByIdentifier>> = {
    readonly [Identifier in keyof HandlersByIdentifier & keyof EndpointsByIdentifier]: HandleAllEntryHandler<HandlersByIdentifier[Identifier]> extends HttpApiEndpoint.Handler<EndpointsByIdentifier[Identifier], HttpApiEndpoint.MiddlewareError<EndpointsByIdentifier[Identifier]>, infer R1> ? HandlerRequirements<EndpointsByIdentifier[Identifier], R1> : never;
}[keyof HandlersByIdentifier & keyof EndpointsByIdentifier];
type HandlersResult<A> = A extends Effect.Effect<infer H, any, any> ? H : A;
type MissingHandlerNames<H extends Handlers<any, any, any>> = Exclude<keyof H["~EndpointsByIdentifier"], H["~HandledIdentifiers"]>;
type ValidateHandlersReturn<A, H = HandlersResult<A>, Missing = H extends Handlers<any, any, any> ? MissingHandlerNames<H> : never> = H extends Handlers<any, any, any> ? [Missing] extends [never] ? A : `Endpoint not handled: ${Missing & string}` : `Must return the implemented handlers`;
/**
 * Mutable handler collection for one `HttpApi` group.
 *
 * **Details**
 *
 * Each call to `handle` or `handleRaw` registers an endpoint implementation and
 * adds that endpoint identifier to the type-level set of implemented endpoints.
 * Endpoint identifiers that were already handled are rejected at the type level.
 *
 * @stability unstable
 * @category handlers
 * @since 4.0.0
 */
export interface Handlers<R, EndpointsByIdentifier extends Record<string, HttpApiEndpoint.Constraint> = {}, HandledIdentifiers extends keyof EndpointsByIdentifier = never> extends Pipeable {
    readonly [HandlersTypeId]: typeof HandlersTypeId;
    readonly "~EndpointsByIdentifier": EndpointsByIdentifier;
    readonly "~HandledIdentifiers": HandledIdentifiers;
    /**
     * Add the implementation for an unhandled `HttpApiEndpoint` to a `Handlers` group.
     */
    handle<Identifier extends keyof EndpointsByIdentifier, R1>(identifier: Identifier & NotHandledIdentifier<Identifier, HandledIdentifiers>, handler: HttpApiEndpoint.Handler<EndpointsByIdentifier[Identifier], HttpApiEndpoint.MiddlewareError<EndpointsByIdentifier[Identifier]>, R1>, options?: {
        readonly uninterruptible?: boolean | undefined;
    } | undefined): Handlers<R | HandlerRequirements<EndpointsByIdentifier[Identifier], R1>, EndpointsByIdentifier, HandledIdentifiers | Identifier>;
    /**
     * Add implementations for unhandled `HttpApiEndpoint`s in a `Handlers` group.
     */
    handleAll<const HandlersByIdentifier extends HandleAllHandlers<Omit<EndpointsByIdentifier, HandledIdentifiers>>>(handlers: HandlersByIdentifier & HandleAllExtraKeys<Omit<EndpointsByIdentifier, HandledIdentifiers>, HandlersByIdentifier>): Handlers<R | HandleAllRequirements<EndpointsByIdentifier, HandlersByIdentifier>, EndpointsByIdentifier, HandledIdentifiers | (keyof HandlersByIdentifier & keyof EndpointsByIdentifier)>;
    /**
     * Add the implementation for an unhandled `HttpApiEndpoint` to a `Handlers` group.
     * This version opts out of automatic payload decoding and provides the raw request.
     */
    handleRaw<Identifier extends keyof EndpointsByIdentifier, R1>(identifier: Identifier & NotHandledIdentifier<Identifier, HandledIdentifiers>, handler: HttpApiEndpoint.HandlerRaw<EndpointsByIdentifier[Identifier], HttpApiEndpoint.MiddlewareError<EndpointsByIdentifier[Identifier]>, R1>, options?: {
        readonly uninterruptible?: boolean | undefined;
    } | undefined): Handlers<R | HandlerRequirements<EndpointsByIdentifier[Identifier], R1>, EndpointsByIdentifier, HandledIdentifiers | Identifier>;
}
/**
 * Namespace containing helper types for `HttpApiBuilder` handler collections.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Handlers {
    /**
     * Creates a handler collection for a group where every endpoint in the group is
     * still awaiting an implementation.
     *
     * @stability unstable
     * @category handlers
     * @since 4.0.0
     */
    type FromGroup<Group extends HttpApiGroup.Constraint> = Handlers<never, Group["endpoints"]>;
    /**
     * Validates the return value of a group handler builder, preserving successful
     * handler collections and producing a descriptive type error when endpoints remain
     * unhandled.
     *
     * @stability unstable
     * @category handlers
     * @since 4.0.0
     */
    type ValidateReturn<A> = ValidateHandlersReturn<A>;
    /**
     * Extracts the error channel from an effect that produces a `Handlers`
     * collection, returning `never` for non-effectful handler collections.
     *
     * @stability unstable
     * @category handlers
     * @since 4.0.0
     */
    type Error<A> = A extends Effect.Effect<Handlers<infer _R, infer _EndpointsByIdentifier, infer _HandledIdentifiers>, infer _EX, infer _RX> ? _EX : never;
    /**
     * Extracts the services required by a handler collection, including both handler
     * requirements and the environment required to construct the handlers.
     *
     * @stability unstable
     * @category handlers
     * @since 4.0.0
     */
    type Context<A> = A extends Handlers<infer _R, infer _EndpointsByIdentifier, infer _HandledIdentifiers> ? _R : A extends Effect.Effect<Handlers<infer _R, infer _EndpointsByIdentifier, infer _HandledIdentifiers>, infer _EX, infer _RX> ? _R | _RX : never;
}
type EndpointReturn<Groups extends HttpApiGroup.Constraint, GroupIdentifier extends HttpApiGroup.Identifier<Groups>, EndpointIdentifier extends HttpApiGroup.Endpoints<HttpApiGroup.WithIdentifier<Groups, GroupIdentifier>>["identifier"], R, Endpoint extends HttpApiEndpoint.Constraint = HttpApiEndpoint.WithIdentifier<HttpApiGroup.Endpoints<HttpApiGroup.WithIdentifier<Groups, GroupIdentifier>>, EndpointIdentifier>> = Effect.Effect<Effect.Effect<HttpServerResponse, never, HttpServerRequest | HttpRouter.RouteContext | Request.ParsedSearchParams | Exclude<R, HttpApiEndpoint.MiddlewareProvides<Endpoint>>>, never, HttpApiEndpoint.ServerServices<Endpoint> | HttpApiEndpoint.Middleware<Endpoint> | HttpApiEndpoint.MiddlewareServices<Endpoint> | Etag.Generator | FileSystem | HttpPlatform | Path>;
/**
 * Builds the server-side HTTP effect for a single endpoint in an API group using
 * the endpoint metadata, middleware, codecs, and supplied handler.
 *
 * @stability unstable
 * @category handlers
 * @since 4.0.0
 */
export declare const endpoint: <ApiId extends string, Groups extends HttpApiGroup.Constraint, const GroupIdentifier extends HttpApiGroup.Identifier<Groups>, const EndpointIdentifier extends HttpApiGroup.Endpoints<HttpApiGroup.WithIdentifier<Groups, GroupIdentifier>>["identifier"], R>(api: HttpApi.HttpApi<ApiId, Groups>, groupIdentifier: GroupIdentifier, endpointIdentifier: EndpointIdentifier, handler: NoInfer<HttpApiEndpoint.HandlerWithIdentifier<HttpApiGroup.Endpoints<HttpApiGroup.WithIdentifier<Groups, GroupIdentifier>>, EndpointIdentifier, never, R>>) => EndpointReturn<Groups, GroupIdentifier, EndpointIdentifier, R>;
/**
 * Decodes credentials for an HTTP API security scheme from the current request,
 * supporting bearer, API key, and basic authentication inputs.
 *
 * @stability unstable
 * @category security
 * @since 4.0.0
 */
export declare const securityDecode: <Security extends HttpApiSecurity.HttpApiSecurity>(self: Security) => Effect.Effect<HttpApiSecurity.HttpApiSecurity.Type<Security>, never, HttpServerRequest | Request.ParsedSearchParams>;
/**
 * Registers a pre-response handler that sets an API-key cookie on the outgoing
 * response, defaulting the cookie to `secure` and `httpOnly` unless overridden.
 *
 * @stability unstable
 * @category security
 * @since 4.0.0
 */
export declare const securitySetCookie: (self: HttpApiSecurity.ApiKey, value: string | Redacted.Redacted, options?: Cookie["options"]) => Effect.Effect<void, never, HttpServerRequest>;
export {};
//# sourceMappingURL=HttpApiBuilder.d.ts.map