import type { Brand } from "../Brand.ts";
import * as Context from "../Context.ts";
import type { Effect } from "../Effect.ts";
import type * as Sse from "../encoding/Sse.ts";
import type { HttpMethod } from "../http/HttpMethod.ts";
import * as HttpRouter from "../http/HttpRouter.ts";
import type { HttpServerRequest } from "../http/HttpServerRequest.ts";
import type { HttpServerResponse } from "../http/HttpServerResponse.ts";
import type * as Multipart from "../http/Multipart.ts";
import { type Pipeable } from "../Pipeable.ts";
import * as Schema from "../Schema.ts";
import type * as Stream from "../Stream.ts";
import type { Simplify } from "../Struct.ts";
import type * as Types from "../Types.ts";
import type * as HttpApiGroup from "./HttpApiGroup.ts";
import type * as HttpApiMiddleware from "./HttpApiMiddleware.ts";
import * as HttpApiSchema from "./HttpApiSchema.ts";
declare const TypeId = "~effect/http-api/HttpApiEndpoint";
/**
 * Returns `true` when a value is an `HttpApiEndpoint`, narrowing the value to the
 * endpoint interface.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isHttpApiEndpoint: (u: unknown) => u is Top;
type SuccessType<S> = S extends HttpApiSchema.WithHeaders<infer _Inner, infer _Headers> ? HttpApiSchema.withHeaders<SuccessType<_Inner>, _Headers["Type"]> : S extends HttpApiSchema.StreamSse<infer _Events, infer _Error, infer _Value> ? Stream.Stream<_Value, _Error["Type"], never> : S extends HttpApiSchema.StreamUint8Array ? Stream.Stream<Uint8Array, unknown, never> : S extends Schema.Constraint ? S["Type"] : never;
type SuccessEncodingServices<S> = S extends HttpApiSchema.StreamSse<infer _Events, infer _Error, infer _Value> ? _Events["EncodingServices"] | _Error["EncodingServices"] : S extends HttpApiSchema.StreamUint8Array ? never : S extends Schema.Constraint ? S["EncodingServices"] : never;
type SuccessDecodingServices<S> = S extends HttpApiSchema.StreamSse<infer _Events, infer _Error, infer _Value> ? _Events["DecodingServices"] | _Error["DecodingServices"] : S extends HttpApiSchema.StreamUint8Array ? never : S extends Schema.Constraint ? S["DecodingServices"] : never;
type UnwrapReadonlyArray<S> = S extends ReadonlyArray<infer A> ? A : S;
type ExtractBufferedSuccess<S extends SuccessConstraint> = Exclude<Extract<UnwrapReadonlyArray<S>, Schema.Top>, HttpApiSchema.StreamSchema | HttpApiSchema.WithHeaders<Schema.Top, Schema.Top>>;
type ExtractStreamSuccess<S extends SuccessConstraint> = UnwrapReadonlyArray<S> extends infer Success ? Success extends HttpApiSchema.StreamSchema ? Success : never : never;
type ExtractWithHeadersSuccess<S extends SuccessConstraint> = UnwrapReadonlyArray<S> extends infer Success ? Success extends HttpApiSchema.WithHeaders<infer _Inner, infer _Headers> ? HttpApiSchema.WithHeaders<_Inner extends HttpApiSchema.StreamSchema ? _Inner : Schema.toCodecJson<_Inner>, Schema.toCodecStringTree<_Headers>> : never : never;
type ToSuccessCodec<S extends SuccessConstraint> = [ExtractBufferedSuccess<S>] extends [never] ? ExtractStreamSuccess<S> | ExtractWithHeadersSuccess<S> : Schema.toCodecJson<ExtractBufferedSuccess<S>> | ExtractStreamSuccess<S> | ExtractWithHeadersSuccess<S>;
type ToJsonCodec<S> = [S] extends [never] ? never : [S] extends [Schema.Constraint] ? Schema.toCodecJson<S> : never;
type ToStringTreeCodec<S> = [S] extends [never] ? never : [S] extends [Schema.Struct.Fields] ? Schema.toCodecStringTree<Schema.Struct<S>> : [S] extends [Schema.Constraint] ? Schema.toCodecStringTree<S> : never;
type RequestFromParts<Endpoint, ParamsType, QueryType, PayloadType, HeadersType> = ([ParamsType] extends [never] ? {} : {
    readonly params: Simplify<ParamsType>;
}) & ([QueryType] extends [never] ? {} : {
    readonly query: Simplify<QueryType>;
}) & ([PayloadType] extends [never] ? {} : PayloadType extends Brand<HttpApiSchema.MultipartStreamTypeId> ? {
    readonly payload: Stream.Stream<Multipart.Part, Multipart.MultipartError>;
} : {
    readonly payload: Simplify<PayloadType>;
}) & ([HeadersType] extends [never] ? {} : {
    readonly headers: Simplify<HeadersType>;
}) & {
    readonly request: HttpServerRequest;
    readonly endpoint: Endpoint;
    readonly group: HttpApiGroup.Top;
};
type RequestRawFromParts<Endpoint, ParamsType, QueryType, HeadersType> = ([ParamsType] extends [never] ? {} : {
    readonly params: Simplify<ParamsType>;
}) & ([QueryType] extends [never] ? {} : {
    readonly query: Simplify<QueryType>;
}) & ([HeadersType] extends [never] ? {} : {
    readonly headers: Simplify<HeadersType>;
}) & {
    readonly request: HttpServerRequest;
    readonly endpoint: Endpoint;
    readonly group: HttpApiGroup.Top;
};
/**
 * Maps normalized media types to a payload encoding strategy and one or more
 * schemas. Each schema retains its declared content type in its encoding annotation.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type PayloadMap = ReadonlyMap<string, {
    readonly encoding: HttpApiSchema.PayloadEncoding;
    readonly schemas: readonly [Schema.Top, ...Array<Schema.Top>];
}>;
/**
 * Represents an API endpoint. An API endpoint is mapped to a single route on
 * the underlying `HttpRouter`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface HttpApiEndpoint<out Identifier extends string, out Method extends HttpMethod, out Path extends string, out Params extends Schema.Top = never, out Query extends Schema.Top = never, out Payload extends Schema.Top = never, out Headers extends Schema.Top = never, out Success extends Schema.Top = typeof HttpApiSchema.NoContent, out Error extends Schema.Top = never, in out Middleware = never, out MiddlewareServices = never> extends Pipeable {
    new (_: never): {};
    readonly [TypeId]: typeof TypeId;
    readonly "~Params": Params;
    readonly "~Query": Query;
    readonly "~Headers": Headers;
    readonly "~Payload": Payload;
    readonly "~Success": Success;
    readonly "~Error": Error;
    readonly "~Middleware": Middleware;
    readonly "~MiddlewareServices": MiddlewareServices;
    readonly "~Request": RequestFromParts<this, Params["Type"], Query["Type"], Payload["Type"], Headers["Type"]>;
    readonly "~RequestRaw": RequestRawFromParts<this, Params["Type"], Query["Type"], Headers["Type"]>;
    readonly identifier: Identifier;
    readonly path: Path;
    readonly method: Method;
    readonly params: Schema.Top | undefined;
    readonly query: Schema.Top | undefined;
    readonly headers: Schema.Top | undefined;
    readonly payload: PayloadMap;
    readonly success: ReadonlySet<Schema.Top>;
    readonly error: ReadonlySet<Schema.Top>;
    readonly annotations: Context.Context<never>;
    readonly middlewares: ReadonlySet<Context.Key<Middleware, any>>;
    readonly disableCodecs: boolean;
    /**
     * Add a prefix to the path of the endpoint.
     */
    prefix<const Prefix extends HttpRouter.PathInput>(prefix: Prefix): HttpApiEndpoint<Identifier, Method, `${Prefix}${Path}`, Params, Query, Payload, Headers, Success, Error, Middleware, MiddlewareServices>;
    /**
     * Add an `HttpApiMiddleware` to the endpoint.
     */
    middleware<I extends HttpApiMiddleware.AnyId, S>(middleware: Context.Key<I, S>): HttpApiEndpoint<Identifier, Method, Path, Params, Query, Payload, Headers, Success, Error, Middleware | I, HttpApiMiddleware.ApplyServices<I, MiddlewareServices>>;
    /**
     * Add an annotation on the endpoint.
     */
    annotate<I, S>(key: Context.Key<I, S>, value: Types.NoInfer<S>): HttpApiEndpoint<Identifier, Method, Path, Params, Query, Payload, Headers, Success, Error, Middleware, MiddlewareServices>;
    /**
     * Merge the annotations of the endpoint with the provided context.
     */
    annotateMerge<I>(annotations: Context.Context<I>): HttpApiEndpoint<Identifier, Method, Path, Params, Query, Payload, Headers, Success, Error, Middleware, MiddlewareServices>;
}
/**
 * A widened `HttpApiEndpoint` type used when the concrete method, path, schemas,
 * and middleware types are not needed.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Constraint {
    readonly [TypeId]: typeof TypeId;
    readonly identifier: string;
    readonly ["~Success"]: Schema.Constraint;
    readonly ["~Error"]: Schema.Constraint;
    readonly ["~Request"]: unknown;
    readonly ["~RequestRaw"]: unknown;
}
/**
 * A widened endpoint type that preserves request and middleware pipeline phantom fields.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface ConstraintRequest extends Constraint {
    readonly ["~Params"]: Schema.Constraint;
    readonly ["~Query"]: Schema.Constraint;
    readonly ["~Payload"]: Schema.Constraint;
    readonly ["~Headers"]: Schema.Constraint;
    readonly ["~Middleware"]: unknown;
}
/**
 * A widened endpoint type that preserves concrete runtime properties such as
 * method, path, schemas, annotations, and middleware sets.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Top extends HttpApiEndpoint<string, HttpMethod, string, Schema.Top, Schema.Top, Schema.Top, Schema.Top, Schema.Top, Schema.Top, any, unknown> {
}
/**
 * Extracts the endpoint identifier literal from an `HttpApiEndpoint`.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Identifier<Endpoint> = Endpoint extends Constraint ? Endpoint["identifier"] : never;
/**
 * Extracts the success schema associated with an endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Success<Endpoint> = Endpoint extends Constraint ? Endpoint["~Success"] : never;
/**
 * Extracts the error schema associated with an endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Error<Endpoint> = Endpoint extends Constraint ? Endpoint["~Error"] : never;
/**
 * Extracts the schema used for an endpoint's path parameters.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Params<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Params"] : never;
/**
 * Extracts the schema used for an endpoint's query parameters.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Query<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Query"] : never;
/**
 * Extracts the schema used for an endpoint's request payload.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Payload<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Payload"] : never;
/**
 * Extracts the schema used for an endpoint's request headers.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Headers<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Headers"] : never;
/**
 * Extracts the middleware identifiers attached to an endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Middleware<Endpoint> = Endpoint extends {
    readonly "~Middleware": infer M;
} ? M : never;
/**
 * Computes the services provided by the middleware attached to an endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type MiddlewareProvides<Endpoint> = HttpApiMiddleware.Provides<Middleware<Endpoint>>;
/**
 * Computes the client-side middleware services required by an endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type MiddlewareClient<Endpoint> = HttpApiMiddleware.MiddlewareClient<Middleware<Endpoint>>;
/**
 * Computes the error types that can be produced by the middleware attached to an
 * endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type MiddlewareError<Endpoint> = HttpApiMiddleware.Error<Middleware<Endpoint>>;
/**
 * Computes the full error value union for an endpoint, including the endpoint
 * error schema's type and errors introduced by middleware.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Errors<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Error"]["Type"] | HttpApiMiddleware.Error<Endpoint["~Middleware"]> : never;
/**
 * Computes the services required to encode an endpoint's error responses,
 * including services required by middleware error encoders.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ErrorServicesEncode<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Error"]["EncodingServices"] | HttpApiMiddleware.ErrorServicesEncode<Endpoint["~Middleware"]> : never;
/**
 * Builds the decoded request shape passed to a normal endpoint handler, including
 * available params, query, payload, headers, the raw request, endpoint, and group.
 * Multipart stream payloads are exposed as streams of parts.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Request<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Request"] : {};
/**
 * Builds the request shape passed to a raw endpoint handler, including decoded
 * params, query, and headers plus the raw request, endpoint, and group, while
 * leaving payload handling to the raw request.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type RequestRaw<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~RequestRaw"] : {};
/**
 * Builds the request object accepted by a generated client method, including the
 * params, query, headers, and payload required by the endpoint, plus optional
 * response mode and SSE decoding options. Multipart payloads are supplied as
 * `FormData`.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ClientRequest<Params extends Schema.Constraint, Query extends Schema.Constraint, Payload extends Schema.Constraint, Headers extends Schema.Constraint, ResponseMode extends ClientResponseMode> = (([Params["Type"]] extends [never] ? {} : {
    readonly params: Params["Type"];
}) & ([Query["Type"]] extends [never] ? {} : {
    readonly query: Query["Type"];
}) & ([Headers["Type"]] extends [never] ? {} : {
    readonly headers: Headers["Type"];
}) & ([Payload["Type"]] extends [never] ? {} : Payload["Type"] extends infer P ? P extends Brand<HttpApiSchema.MultipartTypeId> | Brand<HttpApiSchema.MultipartStreamTypeId> ? {
    readonly payload: FormData;
} : {
    readonly payload: Payload["Type"];
} : {
    readonly payload: Payload["Type"];
})) extends infer Req ? keyof Req extends never ? (void | {
    readonly responseMode?: ResponseMode;
    readonly sseOptions?: Sse.DecodeOptions | undefined;
}) : Req & {
    readonly responseMode?: ResponseMode;
    readonly sseOptions?: Sse.DecodeOptions | undefined;
} : void;
/**
 * Controls what a generated client method returns: the decoded success value,
 * the decoded value paired with the raw response, or only the raw response.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ClientResponseMode = "decoded-only" | "decoded-and-response" | "response-only";
/**
 * Computes the services required on the server to decode endpoint inputs and
 * encode endpoint success, error, and middleware error responses.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ServerServices<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Params"]["DecodingServices"] | Endpoint["~Query"]["DecodingServices"] | Endpoint["~Payload"]["DecodingServices"] | Endpoint["~Headers"]["DecodingServices"] | SuccessEncodingServices<Endpoint["~Success"]> | Endpoint["~Error"]["EncodingServices"] | HttpApiMiddleware.ErrorServicesEncode<Endpoint["~Middleware"]> : never;
/**
 * Computes the services required on the client to encode endpoint requests and
 * decode endpoint success or error responses.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ClientServices<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Params"]["EncodingServices"] | Endpoint["~Query"]["EncodingServices"] | Endpoint["~Payload"]["EncodingServices"] | Endpoint["~Headers"]["EncodingServices"] | SuccessDecodingServices<Endpoint["~Success"]> | Endpoint["~Error"]["DecodingServices"] : never;
/**
 * Extracts the additional services required by middleware applied to an endpoint.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type MiddlewareServices<Endpoint> = Endpoint extends {
    readonly "~MiddlewareServices": infer R;
} ? R : never;
/**
 * Computes the services required to decode an endpoint's error responses,
 * including services required by middleware error decoders.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ErrorServicesDecode<Endpoint> = Endpoint extends ConstraintRequest ? Endpoint["~Error"]["DecodingServices"] | HttpApiMiddleware.ErrorServicesDecode<Endpoint["~Middleware"]> : never;
/**
 * The normal server handler for an endpoint, accepting the decoded request shape
 * and returning either the endpoint success value or a custom `HttpServerResponse`.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Handler<Endpoint extends Constraint, E, R> = (request: Simplify<Endpoint["~Request"]>) => Effect<SuccessType<Endpoint["~Success"]> | HttpServerResponse, Endpoint["~Error"]["Type"] | E, R>;
/**
 * The raw server handler for an endpoint, receiving a request shape without a
 * decoded payload so the handler can read the raw `HttpServerRequest` directly.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type HandlerRaw<Endpoint extends Constraint, E, R> = (request: Simplify<Endpoint["~RequestRaw"]>) => Effect<SuccessType<Endpoint["~Success"]> | HttpServerResponse, Endpoint["~Error"]["Type"] | E, R>;
/**
 * Selects the endpoint with the specified identifier from a union of endpoints.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type WithIdentifier<Endpoints, Identifier extends string> = Extract<Endpoints, {
    readonly identifier: Identifier;
}>;
/**
 * Removes endpoints with the specified identifier from a union of endpoints.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ExcludeIdentifier<Endpoints, Identifier extends string> = Exclude<Endpoints, {
    readonly identifier: Identifier;
}>;
/**
 * Derives the normal handler type for the endpoint with the specified identifier
 * in an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type HandlerWithIdentifier<Endpoints extends Constraint, Identifier extends string, E, R> = Handler<WithIdentifier<Endpoints, Identifier>, E, R>;
/**
 * Derives the raw handler type for the endpoint with the specified identifier in
 * an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type HandlerRawWithIdentifier<Endpoints extends Constraint, Identifier extends string, E, R> = HandlerRaw<WithIdentifier<Endpoints, Identifier>, E, R>;
/**
 * Extracts the decoded success value type for the endpoint with the specified
 * identifier in an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type SuccessWithIdentifier<Endpoints extends Constraint, Identifier extends string> = Success<WithIdentifier<Endpoints, Identifier>> extends infer S ? SuccessType<S> : never;
/**
 * Computes the full error value union for the endpoint with the specified
 * identifier in an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ErrorsWithIdentifier<Endpoints extends Constraint, Identifier extends string> = Errors<WithIdentifier<Endpoints, Identifier>>;
/**
 * Computes the server-side service requirements for the endpoint with the
 * specified identifier in an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ServerServicesWithIdentifier<Endpoints extends Constraint, Identifier extends string> = ServerServices<WithIdentifier<Endpoints, Identifier>>;
/**
 * Extracts the middleware identifiers for the endpoint with the specified
 * identifier in an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type MiddlewareWithIdentifier<Endpoints extends Constraint, Identifier extends string> = Middleware<WithIdentifier<Endpoints, Identifier>>;
/**
 * Extracts the middleware service requirements for the endpoint with the
 * specified identifier in an endpoint union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type MiddlewareServicesWithIdentifier<Endpoints extends Constraint, Identifier extends string> = MiddlewareServices<WithIdentifier<Endpoints, Identifier>>;
/**
 * Removes services provided by the HTTP router and the selected endpoint's
 * middleware from a service requirement union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ExcludeProvidedWithIdentifier<Endpoints extends Constraint, Identifier extends string, R> = ExcludeProvided<WithIdentifier<Endpoints, Identifier>, R>;
/**
 * Removes services provided by the HTTP router and endpoint middleware from a
 * service requirement union.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ExcludeProvided<Endpoint extends Constraint, R> = Exclude<R, HttpRouter.Provided | HttpApiMiddleware.Provides<Middleware<Endpoint>>>;
/**
 * Returns an endpoint type with the supplied path prefix prepended while
 * preserving the endpoint's schemas, method, errors, and middleware.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type AddPrefix<Endpoint, Prefix extends HttpRouter.PathInput> = Endpoint extends HttpApiEndpoint<infer _Identifier, infer _Method, infer _Path, infer _Params, infer _Query, infer _Payload, infer _Headers, infer _Success, infer _Error, infer _M, infer _MR> ? HttpApiEndpoint<_Identifier, _Method, `${Prefix}${_Path}`, _Params, _Query, _Payload, _Headers, _Success, _Error, _M, _MR> : never;
/**
 * Returns an endpoint type with additional middleware applied and the endpoint's
 * middleware service requirements updated accordingly.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type AddMiddleware<Endpoint, M extends HttpApiMiddleware.AnyId> = Endpoint extends HttpApiEndpoint<infer _Identifier, infer _Method, infer _Path, infer _Params, infer _Query, infer _Payload, infer _Headers, infer _Success, infer _Error, infer _M, infer _MR> ? HttpApiEndpoint<_Identifier, _Method, _Path, _Params, _Query, _Payload, _Headers, _Success, _Error, _M | M, HttpApiMiddleware.ApplyServices<M, _MR>> : never;
/**
 * Constraint for path parameter schemas: each parameter must encode to
 * `string | undefined`, or the schema must encode to a record of those values.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ParamsConstraint = Record<string, Schema.Encoder<string | undefined, unknown>> | Schema.Encoder<Record<string, string | undefined>, unknown>;
/**
 * Constraint for header schemas: each header must encode to `string | undefined`,
 * or the schema must encode to a record of those values.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type HeadersConstraint = Record<string, Schema.Encoder<string | undefined, unknown>> | Schema.Encoder<Record<string, string | undefined>, unknown>;
/**
 * Constraint for query schemas: each field must encode to `string`, an array of
 * strings, or `undefined`, or the schema must encode to a record of those values.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type QueryConstraint = Record<string, Schema.Encoder<string | ReadonlyArray<string> | undefined, unknown>> | Schema.Encoder<string | ReadonlyArray<string> | undefined, unknown>;
/**
 * Payload schema depends on the HTTP method:
 * - for no-body methods, payload is modeled as query params, so each field must
 *   encode to `string | ReadonlyArray<string> | undefined` and OpenAPI can expand
 *   it into `in: query` parameters
 * - for body methods, payload may be any `Schema.Top` (or content-type keyed
 *   schemas) and OpenAPI uses `requestBody` instead of `parameters`
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type PayloadConstraint<Method extends HttpMethod> = Method extends HttpMethod.NoBody ? Record<string, Schema.Encoder<string | ReadonlyArray<string> | undefined, unknown>> : Schema.Top | ReadonlyArray<Schema.Top>;
/**
 * Payload constraint used when automatic codecs are enabled: no-body methods
 * accept field records for query-style encoding, while body methods accept one or
 * more schemas.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type PayloadConstraintCodecs<Method extends HttpMethod> = Method extends HttpMethod.NoBody ? Record<string, Schema.Top> : Schema.Top | ReadonlyArray<Schema.Top>;
/**
 * Constraint for success response schemas, allowing either a single schema or a
 * readonly array of schemas.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type SuccessConstraint = Schema.Top | ReadonlyArray<Schema.Top>;
/**
 * Constraint for error response schemas, allowing either a single schema or a
 * readonly array of schemas.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ErrorConstraint = Schema.Top | ReadonlyArray<Schema.Top>;
type ErrorSchema<S> = S extends HttpApiSchema.WithHeaders<infer Inner, Schema.Top> ? Inner : S;
type ErrorNoStream<S extends ErrorConstraint> = [
    Extract<ErrorSchema<S extends ReadonlyArray<Schema.Constraint> ? S[number] : S>, HttpApiSchema.StreamSchema>
] extends [never] ? S : never;
/**
 * Creates endpoint constructors for a specific HTTP method. The resulting
 * constructor builds an `HttpApiEndpoint` from an identifier, path, and optional request
 * and response schemas, applying automatic JSON or string-tree codecs unless
 * `disableCodecs` is enabled.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <Method extends HttpMethod>(method: Method) => {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<Method> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    }): HttpApiEndpoint<Identifier, Method, Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, Method extends HttpMethod.WithBody ? ToJsonCodec<ToSchema<Payload>> : ToStringTreeCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends ReadonlyArray<Schema.Constraint> ? Error[number] : Error>>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<Method> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    }): HttpApiEndpoint<Identifier, Method, Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends ReadonlyArray<Schema.Constraint> ? Error[number] : Error>;
};
type ToSchema<S extends Schema.Struct.Fields | Schema.Constraint | ReadonlyArray<Schema.Constraint>> = S extends Schema.Struct.Fields ? Schema.Struct<S> : S extends ReadonlyArray<Schema.Constraint> ? S[number] : S;
/**
 * Creates a `GET` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const get: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"GET"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "GET", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToStringTreeCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"GET"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "GET", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
/**
 * Creates a `QUERY` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const query: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"QUERY"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "QUERY", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToJsonCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"QUERY"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "QUERY", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
/**
 * Creates a `POST` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const post: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"POST"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "POST", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToJsonCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"POST"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "POST", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
/**
 * Creates a `PUT` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const put: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"PUT"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "PUT", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToJsonCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"PUT"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "PUT", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
/**
 * Creates a `PATCH` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const patch: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"PATCH"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "PATCH", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToJsonCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"PATCH"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "PATCH", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
declare const del: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"DELETE"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "DELETE", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToJsonCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"DELETE"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "DELETE", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
/**
 * @stability unstable
 */
export { 
/**
 * Creates a `DELETE` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
del as delete };
/**
 * Creates a `HEAD` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const head: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"HEAD"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "HEAD", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToStringTreeCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"HEAD"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "HEAD", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
/**
 * Creates an `OPTIONS` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const options: {
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends Schema.Top | Schema.Struct.Fields = never, Query extends Schema.Top | Schema.Struct.Fields = never, Payload extends PayloadConstraintCodecs<"OPTIONS"> = never, Headers extends Schema.Top | Schema.Struct.Fields = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends Schema.Top | ReadonlyArray<Schema.Top> = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs?: false | undefined;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "OPTIONS", Path, ToStringTreeCodec<Params>, ToStringTreeCodec<Query>, ToStringTreeCodec<ToSchema<Payload>>, ToStringTreeCodec<Headers>, ToSuccessCodec<Success>, ToJsonCodec<Error extends readonly Schema.Constraint[] ? Error[number] : Error>, never, never>;
    <const Identifier extends string, const Path extends HttpRouter.PathInput, Params extends ParamsConstraint = never, Query extends QueryConstraint = never, Payload extends PayloadConstraint<"OPTIONS"> = never, Headers extends HeadersConstraint = never, const Success extends SuccessConstraint = HttpApiSchema.NoContent, const Error extends ErrorConstraint = never>(identifier: Identifier, path: Path, options?: {
        readonly disableCodecs: true;
        readonly params?: Params | undefined;
        readonly query?: Query | undefined;
        readonly headers?: Headers | undefined;
        readonly payload?: Payload | undefined;
        readonly success?: Success | undefined;
        readonly error?: (Error & ErrorNoStream<Types.NoInfer<Error>>) | undefined;
    } | undefined): HttpApiEndpoint<Identifier, "OPTIONS", Path, Params extends Schema.Struct.Fields ? Schema.Struct<Params> : Params, Query extends Schema.Struct.Fields ? Schema.Struct<Query> : Query, ToSchema<Payload>, ToSchema<Headers>, UnwrapReadonlyArray<Success>, Error extends readonly Schema.Constraint[] ? Error[number] : Error, never, never>;
};
//# sourceMappingURL=HttpApiEndpoint.d.ts.map