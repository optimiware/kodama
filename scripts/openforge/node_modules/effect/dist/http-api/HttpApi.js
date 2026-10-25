import * as Context from "../Context.js";
import * as InternalRecord from "../internal/record.js";
import { pipeArguments } from "../Pipeable.js";
import * as Predicate from "../Predicate.js";
import * as Record from "../Record.js";
import * as HttpApiEndpoint from "./HttpApiEndpoint.js";
import * as HttpApiSchema from "./HttpApiSchema.js";
const TypeId = "~effect/http-api/HttpApi";
/**
 * Returns `true` when a value is an `HttpApi`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpApi = u => Predicate.hasProperty(u, TypeId);
const Proto = {
  [TypeId]: TypeId,
  pipe() {
    return pipeArguments(this, arguments);
  },
  add(...toAdd) {
    const groups = {
      ...this.groups
    };
    for (const group of toAdd) {
      InternalRecord.assignProperty(groups, group.identifier, group);
    }
    return makeProto({
      ...optionsFromApi(this),
      groups
    });
  },
  addHttpApi(api) {
    const newGroups = {
      ...this.groups
    };
    for (const key of Object.keys(api.groups)) {
      const group = api.groups[key];
      InternalRecord.assignProperty(newGroups, key, group.annotateMerge(Context.merge(api.annotations, group.annotations)));
    }
    return makeProto({
      ...optionsFromApi(this),
      groups: newGroups
    });
  },
  prefix(prefix) {
    return makeProto({
      ...optionsFromApi(this),
      groups: Record.map(this.groups, group => group.prefix(prefix))
    });
  },
  middleware(tag) {
    return makeProto({
      ...optionsFromApi(this),
      groups: Record.map(this.groups, group => group.middleware(tag))
    });
  },
  annotate(key, value) {
    return makeProto({
      ...optionsFromApi(this),
      annotations: Context.add(this.annotations, key, value)
    });
  },
  annotateMerge(annotations) {
    return makeProto({
      ...optionsFromApi(this),
      annotations: Context.merge(this.annotations, annotations)
    });
  }
};
const optionsFromApi = api => ({
  identifier: api.identifier,
  groups: api.groups,
  annotations: api.annotations
});
const makeProto = options => {
  function HttpApi() {}
  Object.setPrototypeOf(HttpApi, Proto);
  return Object.assign(HttpApi, options);
};
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
export const make = identifier => makeProto({
  identifier,
  groups: {},
  annotations: Context.empty()
});
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
export const reflect = (self, options) => {
  const groups = Object.values(self.groups);
  for (const group of groups) {
    const groupAnnotations = Context.merge(self.annotations, group.annotations);
    options.onGroup({
      group,
      mergedAnnotations: groupAnnotations
    });
    const endpoints = Object.values(group.endpoints);
    for (const endpoint of endpoints) {
      if (options.predicate && !options.predicate({
        endpoint,
        group
      })) continue;
      options.onEndpoint({
        group,
        endpoint,
        middleware: endpoint.middlewares,
        mergedAnnotations: Context.merge(groupAnnotations, endpoint.annotations),
        successes: extractResponseContent(HttpApiEndpoint.getSuccessSchemas(endpoint), HttpApiSchema.getStatusSuccessSchema),
        errors: extractResponseContent(HttpApiEndpoint.getErrorSchemas(endpoint), HttpApiSchema.getStatusErrorSchema)
      });
    }
  }
};
// -------------------------------------------------------------------------------------
const extractResponseContent = (schemas, getStatus) => {
  const map = new Map();
  schemas.forEach(add);
  return map;
  function add(schema) {
    const body = HttpApiSchema.isWithHeaders(schema) ? schema.schema : schema;
    if (HttpApiSchema.isStreamSchema(body)) return;
    const status = getStatus(schema);
    const schemas = map.get(status);
    if (schemas === undefined) {
      map.set(status, [schema]);
    } else {
      schemas.push(schema);
    }
  }
};
/**
 * Adds additional schemas to components/schemas.
 * The provided schemas must have a `identifier` annotation.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class AdditionalSchemas extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/AdditionalSchemas") {}
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
export class ParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/ParseOptions") {}
/**
 * Schema parse options for path params: server decoding, client encoding, and
 * `HttpApiClient.urlBuilder`. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class ParamsParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/ParamsParseOptions") {}
/**
 * Schema parse options for the query string: server decoding, client encoding,
 * and `HttpApiClient.urlBuilder`. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class QueryParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/QueryParseOptions") {}
/**
 * Schema parse options for request headers and the headers of `WithHeaders`
 * responses: server decoding/encoding and client encoding/decoding, including
 * buffered and streamed responses. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class HeadersParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/HeadersParseOptions") {}
/**
 * Schema parse options for request bodies, including multipart payloads:
 * server decoding and client encoding. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class PayloadParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/PayloadParseOptions") {}
/**
 * Schema parse options for success bodies, including streams, SSE events, and
 * the body of `WithHeaders` responses: server encoding and client decoding. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class SuccessParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/SuccessParseOptions") {}
/**
 * Schema parse options for error bodies: server encoding and client decoding. Falls back to `ParseOptions` when unset.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class ErrorParseOptions extends /*#__PURE__*/Context.Service()("effect/http-api/HttpApi/ErrorParseOptions") {}
/**
 * @internal
 */
export const getSlotParseOptions = annotations => {
  const fallback = Context.getOrUndefined(annotations, ParseOptions);
  return {
    params: Context.getOrUndefined(annotations, ParamsParseOptions) ?? fallback,
    query: Context.getOrUndefined(annotations, QueryParseOptions) ?? fallback,
    headers: Context.getOrUndefined(annotations, HeadersParseOptions) ?? fallback,
    payload: Context.getOrUndefined(annotations, PayloadParseOptions) ?? fallback,
    success: Context.getOrUndefined(annotations, SuccessParseOptions) ?? fallback,
    error: Context.getOrUndefined(annotations, ErrorParseOptions) ?? fallback
  };
};
//# sourceMappingURL=HttpApi.js.map