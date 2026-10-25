/**
 * Describes an Effect HTTP API as groups of endpoints.
 *
 * An `HttpApi` value is data: it has an identifier, annotations, and groups of
 * endpoints that describe request inputs, responses, middleware, and route
 * metadata. The same description can be used by server builders, generated
 * clients, URL builders, OpenAPI generation, and reflection tools.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type { NonEmptyReadonlyArray } from "../Array.ts";
import * as Context from "../Context.ts";
import type { PathInput } from "../http/HttpRouter.ts";
import { type Pipeable } from "../Pipeable.ts";
import * as Predicate from "../Predicate.ts";
import type * as Schema from "../Schema.ts";
import type * as SchemaAST from "../SchemaAST.ts";
import * as HttpApiEndpoint from "./HttpApiEndpoint.ts";
import type * as HttpApiGroup from "./HttpApiGroup.ts";
import type * as HttpApiMiddleware from "./HttpApiMiddleware.ts";
declare const TypeId = "~effect/http-api/HttpApi";
/**
 * Returns `true` when a value is an `HttpApi`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpApi: (u: unknown) => u is Top;
/**
 * Groups indexed by their identifier.
 */
type GroupMap<Groups> = {
    readonly [Group in Groups as HttpApiGroup.Identifier<Group>]: Group;
};
/**
 * An `HttpApi` is a collection of HTTP API groups and endpoints that represents a
 * portion of your domain.
 *
 * **When to use**
 *
 * Use when endpoint implementations can be provided with `HttpApiBuilder.group`, and the
 * completed API can be registered with `HttpApiBuilder.layer`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface HttpApi<out Id extends string, in out Groups extends HttpApiGroup.Constraint = never> extends Pipeable {
    new (_: never): {};
    readonly [TypeId]: typeof TypeId;
    readonly identifier: Id;
    readonly groups: GroupMap<Groups>;
    readonly annotations: Context.Context<never>;
    /**
     * Add a `HttpApiGroup` to the `HttpApi`.
     */
    add<const A extends NonEmptyReadonlyArray<HttpApiGroup.Constraint>>(...groups: A): HttpApi<Id, Groups | A[number]>;
    /**
     * Adds every group from another `HttpApi` while preserving its annotation scope.
     *
     * **When to use**
     *
     * Use when you want to compose an API from groups declared and annotated under another API.
     *
     * **Details**
     *
     * The added API is flattened into this API rather than retained as a nested value. Each added group
     * is copied with the added API's annotations, leaving the added API unchanged. Annotation precedence
     * from least to most specific is this API, the added API, the group, and then the endpoint.
     *
     * **Gotchas**
     *
     * Annotations from the added API do not become top-level annotations of the result and do not affect
     * groups already present in this API. They remain scoped to the groups and endpoints being added.
     */
    addHttpApi<Id2 extends string, Groups2 extends HttpApiGroup.Constraint>(api: HttpApi<Id2, Groups2>): HttpApi<Id, Groups | Groups2>;
    /**
     * Prefix all endpoints in the `HttpApi`.
     */
    prefix<const Prefix extends PathInput>(prefix: Prefix): HttpApi<Id, HttpApiGroup.AddPrefix<Groups, Prefix>>;
    /**
     * Adds a middleware to every endpoint currently in the `HttpApi`.
     *
     * **Gotchas**
     *
     * Endpoints added after this method is called do not receive the middleware.
     */
    middleware<I extends HttpApiMiddleware.AnyId, S>(middleware: Context.Key<I, S>): HttpApi<Id, HttpApiGroup.AddMiddleware<Groups, I>>;
    /**
     * Annotate the `HttpApi`.
     */
    annotate<I, S>(tag: Context.Key<I, S>, value: S): HttpApi<Id, Groups>;
    /**
     * Annotate the `HttpApi` with a Context.
     */
    annotateMerge<I>(context: Context.Context<I>): HttpApi<Id, Groups>;
}
/**
 * An `HttpApi` value with its identifier and group types erased.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Constraint {
    readonly [TypeId]: typeof TypeId;
}
/**
 * An `HttpApi` with broad identifier and group types while retaining the concrete
 * runtime properties used by implementation helpers.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Top extends HttpApi<string, HttpApiGroup.Top> {
}
/**
 * Creates an empty `HttpApi` with the supplied identifier.
 *
 * **When to use**
 *
 * Use when you need to start defining an HTTP API, add groups with `add` or
 * `addHttpApi`, provide endpoint implementations with `HttpApiBuilder.group`,
 * and register the API with `HttpApiBuilder.layer`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <const Id extends string>(identifier: Id) => HttpApi<Id, never>;
/**
 * Describes the groups and endpoints in an `HttpApi`.
 *
 * **Details**
 *
 * The callbacks receive each group or endpoint with merged annotations, endpoint
 * middleware, and response schemas grouped by HTTP status.
 *
 * @stability unstable
 * @category reflection
 * @since 4.0.0
 */
export declare const reflect: <Id extends string, Groups extends HttpApiGroup.Constraint>(self: HttpApi<Id, Groups>, options: {
    readonly predicate?: Predicate.Predicate<{
        readonly endpoint: HttpApiEndpoint.Top;
        readonly group: HttpApiGroup.Top;
    }> | undefined;
    readonly onGroup: (options: {
        readonly group: HttpApiGroup.Top;
        readonly mergedAnnotations: Context.Context<never>;
    }) => void;
    readonly onEndpoint: (options: {
        readonly group: HttpApiGroup.Top;
        readonly endpoint: HttpApiEndpoint.Top;
        readonly mergedAnnotations: Context.Context<never>;
        readonly middleware: ReadonlySet<HttpApiMiddleware.AnyService>;
        readonly successes: ReadonlyMap<number, readonly [Schema.Top, ...Array<Schema.Top>]>;
        readonly errors: ReadonlyMap<number, readonly [Schema.Top, ...Array<Schema.Top>]>;
    }) => void;
}) => void;
declare const AdditionalSchemas_base: Context.ServiceClass<AdditionalSchemas, "effect/http-api/HttpApi/AdditionalSchemas", readonly Schema.Constraint[]>;
/**
 * Adds additional schemas to components/schemas.
 * The provided schemas must have a `identifier` annotation.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class AdditionalSchemas extends AdditionalSchemas_base {
}
declare const ParseOptions_base: Context.ServiceClass<ParseOptions, "effect/http-api/HttpApi/ParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for server and client codecs, set on an API, group, or
 * endpoint.
 *
 * **Details**
 *
 * Each codec slot has its own annotation:
 *
 * - `ParamsParseOptions` for path params
 * - `QueryParseOptions` for the query string
 * - `HeadersParseOptions` for request headers and `WithHeaders` response headers
 * - `PayloadParseOptions` for request bodies
 * - `SuccessParseOptions` for success bodies
 * - `ErrorParseOptions` for error bodies
 *
 * A slot annotation at any level takes precedence over `ParseOptions` at any
 * level. If neither is set, Schema defaults apply. Options are replaced, not
 * merged. For the same annotation, endpoint overrides group, which overrides
 * API. Annotate the API before passing it to `HttpApiBuilder.group` or
 * `HttpApiBuilder.endpoint`.
 *
 * **Gotchas**
 *
 * Header codecs receive all HTTP headers, including undeclared transport
 * headers such as `content-type`, `content-length`, `host`, `user-agent` and
 * proxy headers. Without `HeadersParseOptions`, headers use `ParseOptions`:
 *
 * - `onExcessProperty: "error"` rejects real requests and `WithHeaders`
 *   responses with transport headers.
 * - `onExcessProperty: "preserve"` includes transport headers in the decoded
 *   value.
 *
 * Set `HeadersParseOptions` to `{}` at the API level to use Schema defaults for
 * headers, even if an endpoint sets `ParseOptions`.
 *
 * **Example** (Strict bodies with default header parsing)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApi, HttpApiEndpoint, HttpApiGroup } from "effect/http-api"
 *
 * const api = HttpApi.make("Api")
 *   .add(
 *     HttpApiGroup.make("users").add(
 *       HttpApiEndpoint.post("create", "/users", {
 *         headers: { "x-api-key": Schema.String },
 *         payload: { name: Schema.String }
 *       })
 *     )
 *   )
 *   .annotate(HttpApi.ParseOptions, { onExcessProperty: "error" })
 *   .annotate(HttpApi.HeadersParseOptions, {})
 * ```
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ParseOptions extends ParseOptions_base {
}
declare const ParamsParseOptions_base: Context.ServiceClass<ParamsParseOptions, "effect/http-api/HttpApi/ParamsParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for path params: server decoding, client encoding, and
 * `HttpApiClient.urlBuilder`. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ParamsParseOptions extends ParamsParseOptions_base {
}
declare const QueryParseOptions_base: Context.ServiceClass<QueryParseOptions, "effect/http-api/HttpApi/QueryParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for the query string: server decoding, client encoding,
 * and `HttpApiClient.urlBuilder`. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class QueryParseOptions extends QueryParseOptions_base {
}
declare const HeadersParseOptions_base: Context.ServiceClass<HeadersParseOptions, "effect/http-api/HttpApi/HeadersParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for request headers and the headers of `WithHeaders`
 * responses: server decoding/encoding and client encoding/decoding, including
 * buffered and streamed responses. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class HeadersParseOptions extends HeadersParseOptions_base {
}
declare const PayloadParseOptions_base: Context.ServiceClass<PayloadParseOptions, "effect/http-api/HttpApi/PayloadParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for request bodies, including multipart payloads:
 * server decoding and client encoding. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class PayloadParseOptions extends PayloadParseOptions_base {
}
declare const SuccessParseOptions_base: Context.ServiceClass<SuccessParseOptions, "effect/http-api/HttpApi/SuccessParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for success bodies, including streams, SSE events, and
 * the body of `WithHeaders` responses: server encoding and client decoding. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class SuccessParseOptions extends SuccessParseOptions_base {
}
declare const ErrorParseOptions_base: Context.ServiceClass<ErrorParseOptions, "effect/http-api/HttpApi/ErrorParseOptions", SchemaAST.ParseOptions>;
/**
 * Schema parse options for error bodies: server encoding and client decoding. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ErrorParseOptions extends ErrorParseOptions_base {
}
export {};
//# sourceMappingURL=HttpApi.d.ts.map