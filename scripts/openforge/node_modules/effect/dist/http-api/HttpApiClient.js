/**
 * Builds HTTP clients from `HttpApi` declarations.
 *
 * The client methods are derived from the groups and endpoints in an `HttpApi`
 * and run through an `HttpClient`. They use the same schema-driven contract as
 * the server: request parts are encoded from endpoint schemas, client
 * middleware is applied, the HTTP request is executed, and declared success or
 * error responses are decoded. This module also includes helpers for building a
 * client for only one group, one endpoint, or only the encoded URL.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Cause from "../Cause.js";
import * as Effect from "../Effect.js";
import * as Sse from "../encoding/Sse.js";
import { identity } from "../Function.js";
import * as HttpBody from "../http/HttpBody.js";
import * as HttpClient from "../http/HttpClient.js";
import * as HttpClientError from "../http/HttpClientError.js";
import * as HttpClientRequest from "../http/HttpClientRequest.js";
import * as HttpMethod from "../http/HttpMethod.js";
import * as UrlParams from "../http/UrlParams.js";
import * as InternalRecord from "../internal/record.js";
import * as Predicate from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as SchemaAST from "../SchemaAST.js";
import * as SchemaIssue from "../SchemaIssue.js";
import * as SchemaTransformation from "../SchemaTransformation.js";
import * as Stream from "../Stream.js";
import * as UndefinedOr from "../UndefinedOr.js";
import * as HttpApi from "./HttpApi.js";
import * as HttpApiEndpoint from "./HttpApiEndpoint.js";
import * as HttpApiSchema from "./HttpApiSchema.js";
import * as MediaType from "./internal/mediaType.js";
import * as HttpApiPath from "./internal/path.js";
/**
 * @internal
 */
export const makeClient = (api, options) => Effect.gen(function* () {
  const services = yield* Effect.context();
  const httpClient = options.httpClient.pipe(options?.baseUrl === undefined ? identity : HttpClient.mapRequest(HttpClientRequest.prependUrl(options.baseUrl.toString())));
  function executeMiddleware(group, endpoint, request, middlewareKeys, index) {
    if (index === -1) {
      return httpClient.execute(request);
    }
    const middleware = services.mapUnsafe.get(middlewareKeys[index]);
    if (middleware === undefined) {
      return executeMiddleware(group, endpoint, request, middlewareKeys, index - 1);
    }
    return middleware({
      endpoint,
      group,
      request,
      next(request) {
        return executeMiddleware(group, endpoint, request, middlewareKeys, index - 1);
      }
    });
  }
  HttpApi.reflect(api, {
    predicate: options?.predicate,
    onGroup(onGroupOptions) {
      options.onGroup?.(onGroupOptions);
    },
    onEndpoint(onEndpointOptions) {
      const {
        group,
        endpoint,
        errors,
        successes,
        mergedAnnotations
      } = onEndpointOptions;
      const parseOptions = HttpApi.getSlotParseOptions(mergedAnnotations);
      const makeUrl = compilePath(endpoint.path, endpoint.params);
      const decodeMap = {
        orElse: statusOrElse
      };
      const errorAlternatives = new Map();
      for (const [status, schemas] of errors.entries()) {
        const grouped = groupSchemasByContentType(schemas);
        for (const [contentType, schemas] of grouped.entries()) {
          addResponseAlternative(errorAlternatives, status, contentType, schemasToResponse(schemas, parseOptions.error, parseOptions.headers));
        }
      }
      for (const [status, alternatives] of errorAlternatives.entries()) {
        const decode = makeResponseDecoder(alternatives);
        decodeMap[status] = response => Effect.flatMap(Effect.catchCause(decode(response), cause => Effect.failCause(Cause.combine(Cause.fail(new HttpClientError.HttpClientError({
          reason: new HttpClientError.StatusCodeError({
            request: response.request,
            response
          })
        })), cause))), Effect.fail);
      }
      const successAlternatives = new Map();
      for (const [status, schemas] of successes.entries()) {
        const grouped = groupSchemasByContentType(schemas);
        for (const [contentType, schemas] of grouped.entries()) {
          addResponseAlternative(successAlternatives, status, contentType, schemasToResponse(schemas, parseOptions.success, parseOptions.headers));
        }
      }
      for (const streamSuccess of getStreamSuccessSchemas(endpoint)) {
        const streamSchema = isWithHeadersStreamSuccess(streamSuccess) ? streamSuccess.schema : streamSuccess;
        addResponseAlternative(successAlternatives, HttpApiSchema.getStatusSuccessSchema(streamSuccess), streamSchema.contentType, streamToResponse(streamSuccess, parseOptions.success, parseOptions.headers));
      }
      for (const [status, alternatives] of successAlternatives.entries()) {
        decodeMap[status] = makeResponseDecoder(alternatives);
      }
      const encodeParams = UndefinedOr.map(endpoint.params, schema => Schema.encodeUnknownEffect(schema, parseOptions.params));
      const payloadSchemas = HttpApiEndpoint.getPayloadSchemas(endpoint);
      const encodePayload = Arr.isArrayNonEmpty(payloadSchemas) ? Schema.encodeUnknownEffect(HttpMethod.hasBody(endpoint.method) ? getEncodePayloadSchema(payloadSchemas, endpoint.method) : Schema.Union(payloadSchemas), parseOptions.payload) : undefined;
      const encodeHeaders = UndefinedOr.map(endpoint.headers, schema => Schema.encodeUnknownEffect(schema, parseOptions.headers));
      const encodeQuery = UndefinedOr.map(endpoint.query, schema => Schema.encodeUnknownEffect(schema, parseOptions.query));
      const middlewareKeys = Array.from(onEndpointOptions.middleware, tag => `${tag.key}/Client`);
      const endpointFn = Effect.fnUntraced(function* (request) {
        let httpRequest = HttpClientRequest.make(endpoint.method)(endpoint.path);
        if (request !== undefined) {
          // params
          if (encodeParams !== undefined) {
            const params = yield* encodeParams(request.params);
            httpRequest = HttpClientRequest.setUrl(httpRequest, makeUrl(params));
          }
          // payload
          if (encodePayload !== undefined) {
            if (HttpMethod.hasBody(endpoint.method)) {
              if (request.payload instanceof FormData) {
                httpRequest = HttpClientRequest.bodyFormData(httpRequest, request.payload);
              } else {
                const body = yield* encodePayload(request.payload);
                httpRequest = HttpClientRequest.setBody(httpRequest, body);
              }
            } else {
              const urlParams = yield* encodePayload(request.payload);
              httpRequest = HttpClientRequest.appendUrlParams(httpRequest, urlParams);
            }
          }
          // headers
          if (encodeHeaders !== undefined) {
            const headers = yield* encodeHeaders(request.headers);
            httpRequest = HttpClientRequest.setHeaders(httpRequest, headers);
          }
          // query
          if (encodeQuery !== undefined) {
            const query = yield* encodeQuery(request.query);
            httpRequest = HttpClientRequest.appendUrlParams(httpRequest, query);
          }
        }
        const response = yield* executeMiddleware(group, endpoint, httpRequest, middlewareKeys, middlewareKeys.length - 1);
        if (request?.responseMode === "response-only") {
          return response;
        }
        const decoded = (decodeMap[response.status] ?? decodeMap.orElse)(response, request?.sseOptions);
        const value = yield* options.transformResponse === undefined ? decoded : options.transformResponse(decoded);
        return request?.responseMode === "decoded-and-response" ? [value, response] : value;
      });
      options.onEndpoint({
        ...onEndpointOptions,
        endpointFn
      });
    }
  });
});
/**
 * Constructs a type-safe client for an HTTP API using the `HttpClient` service,
 * endpoint schemas, middleware, and optional client or response transformations.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (api, options) => Effect.flatMap(HttpClient.HttpClient, httpClient => makeWith(api, {
  ...options,
  httpClient: options?.transformClient ? options.transformClient(httpClient) : httpClient
}));
/**
 * Constructs a type-safe client for an HTTP API from the supplied `HttpClient`,
 * using the API metadata to encode requests, execute middleware, and decode
 * responses.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeWith = (api, options) => {
  const client = {};
  return makeClient(api, {
    ...options,
    onGroup({
      group
    }) {
      if (group.topLevel) return;
      InternalRecord.assignProperty(client, group.identifier, {});
    },
    onEndpoint({
      endpoint,
      endpointFn,
      group
    }) {
      InternalRecord.assignProperty(group.topLevel ? client : client[group.identifier], endpoint.identifier, endpointFn);
    }
  }).pipe(Effect.as(client));
};
/**
 * Builds a typed client object for a single API group from the supplied
 * `HttpClient`, filtering the API to that group.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const group = (api, options) => {
  const client = {};
  return makeClient(api, {
    ...options,
    predicate: ({
      group
    }) => group.identifier === options.group,
    onEndpoint({
      endpoint,
      endpointFn
    }) {
      InternalRecord.assignProperty(client, endpoint.identifier, endpointFn);
    }
  }).pipe(Effect.map(() => client));
};
/**
 * Builds the typed client method for one endpoint in one API group, using the
 * supplied `HttpClient` and endpoint metadata.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const endpoint = (api, options) => {
  let client = undefined;
  return makeClient(api, {
    ...options,
    httpClient: options.transformClient ? options.transformClient(options.httpClient) : options.httpClient,
    predicate: ({
      endpoint,
      group
    }) => group.identifier === options.group && endpoint.identifier === options.endpoint,
    onEndpoint({
      endpointFn
    }) {
      client = endpointFn;
    }
  }).pipe(Effect.map(() => client));
};
/**
 * Creates a type-safe URL builder that mirrors `HttpApiClient.make`.
 *
 * **Example** (Building typed URLs)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApi, HttpApiClient, HttpApiEndpoint, HttpApiGroup } from "effect/http-api"
 *
 * const Api = HttpApi.make("Api").add(
 *   HttpApiGroup.make("users").add(
 *     HttpApiEndpoint.get("getUser", "/users/:id", {
 *       params: { id: Schema.String }
 *     })
 *   )
 * )
 *
 * const buildUrl = HttpApiClient.urlBuilder(Api, {
 *   baseUrl: "https://api.example.com"
 * })
 *
 * buildUrl.users.getUser({
 *   params: { id: "123" }
 * }) // => "https://api.example.com/users/123"
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const urlBuilder = (api, options) => {
  const builder = {};
  HttpApi.reflect(api, {
    onGroup({
      group
    }) {
      if (group.topLevel) return;
      InternalRecord.assignProperty(builder, group.identifier, {});
    },
    onEndpoint({
      group,
      endpoint,
      mergedAnnotations
    }) {
      const parseOptions = HttpApi.getSlotParseOptions(mergedAnnotations);
      const makeUrl = compilePath(endpoint.path, endpoint.params);
      const encodeParams = endpoint.params === undefined ? undefined : Schema.encodeSync(endpoint.params, parseOptions.params);
      const encodeQuery = endpoint.query === undefined ? undefined : Schema.encodeSync(endpoint.query, parseOptions.query);
      const endpointBuilder = request => {
        const params = request?.params;
        const path = params === undefined ? endpoint.path : makeUrl(encodeParams === undefined ? params : encodeParams(params));
        const queryInput = request?.query === undefined ? undefined : encodeQuery === undefined ? request.query : encodeQuery(request.query);
        const urlParams = queryInput === undefined ? UrlParams.empty : UrlParams.fromInput(queryInput);
        if (options?.baseUrl === undefined) {
          const query = UrlParams.toString(urlParams);
          return query === "" ? path : `${path}?${query}`;
        }
        const url = new URL(HttpClientRequest.prependUrl(HttpClientRequest.get(path), options.baseUrl.toString()).url);
        for (const [key, value] of urlParams.params) {
          if (value !== undefined) {
            url.searchParams.append(key, value);
          }
        }
        return url.toString();
      };
      InternalRecord.assignProperty(group.topLevel ? builder : builder[group.identifier], endpoint.identifier, endpointBuilder);
    }
  });
  return builder;
};
// ----------------------------------------------------------------------------
const paramsRegExp = /(\/?):(\w+)(\?)?/g;
const compilePath = (path, schema) => {
  if (schema === undefined || !path.includes(":")) {
    return _ => path;
  }
  const paramNames = HttpApiPath.getParamNames(schema);
  return params => {
    return path.replace(paramsRegExp, (match, slash, key, optional) => {
      if (paramNames !== undefined && !paramNames.has(key)) {
        return match;
      }
      const value = params[key];
      if (value === undefined) {
        if (optional !== undefined) {
          return "";
        }
        throw new Error(`Missing path parameter: ${key}`);
      }
      return `${slash}${encodeURIComponent(value)}`;
    });
  };
};
function schemasToResponse(schemas, options, headersOptions) {
  const hasWithHeaders = schemas.some(schema => HttpApiSchema.isWithHeaders(schema) || HttpApiSchema.getWithHeadersAnnotation(schema.ast) !== undefined);
  const codec = hasWithHeaders ? Schema.Union(schemas.map(schema => toCodecArrayBufferWithHeaders(schema, options, headersOptions))) : toCodecArrayBuffer(schemas);
  const decode = Schema.decodeUnknownEffect(codec, options);
  return response => Effect.flatMap(response.arrayBuffer, hasWithHeaders ? body => decode({
    body,
    headers: response.headers
  }) : decode);
}
const ResponsePair = /*#__PURE__*/Schema.Struct({
  body: Schema.Unknown,
  headers: Schema.Unknown
});
// Decodes the body with `options` and the headers with `headersOptions`, so
// they are not applied from the outer decode.
function toCodecArrayBufferWithHeaders(schema, options, headersOptions) {
  const annotation = HttpApiSchema.getWithHeadersAnnotation(schema.ast);
  const parts = annotation !== undefined ? {
    body: annotation.bodyCodec,
    headers: annotation.headersCodec,
    toValue: annotation.transformation.decode
  } : HttpApiSchema.isWithHeaders(schema) ? {
    body: schema.schema,
    headers: schema.headers,
    toValue: HttpApiSchema.withHeaders
  } : {
    body: schema,
    headers: Schema.Unknown,
    toValue: pair => pair.body
  };
  const decodeBody = Schema.decodeUnknownEffect(fromArrayBuffer(parts.body).pipe(Schema.decodeTo(parts.body)), options);
  const decodeHeaders = Schema.decodeUnknownEffect(parts.headers, headersOptions);
  return ResponsePair.pipe(Schema.decodeTo(HttpApiSchema.isWithHeaders(schema) ? schema : Schema.toType(schema), SchemaTransformation.transformEffect({
    decode: pair => Effect.all({
      body: decodeBody(pair.body),
      headers: decodeHeaders(pair.headers)
    }).pipe(Effect.map(parts.toValue), Effect.mapError(error => error.issue)),
    encode: (input, options) => Effect.fail(new SchemaIssue.Forbidden({
      message: "Decode only schema"
    }, input, options))
  })));
}
function addResponseAlternative(map, status, contentType, decode) {
  const normalizedContentType = MediaType.normalize(contentType);
  const alternatives = map.get(status);
  if (alternatives === undefined) {
    map.set(status, [{
      contentType: normalizedContentType,
      decode
    }]);
  } else {
    alternatives.push({
      contentType: normalizedContentType,
      decode
    });
  }
}
function makeResponseDecoder(alternatives) {
  const first = alternatives[0];
  if (alternatives.length === 1 && first !== undefined) {
    return first.decode;
  }
  return (response, sseOptions) => {
    const contentType = MediaType.normalize(response.headers["content-type"] ?? "");
    const alternative = alternatives.find(alternative => alternative.contentType === contentType);
    return alternative === undefined ? failUnsupportedContentType(response, contentType, alternatives) : alternative.decode(response, sseOptions);
  };
}
function groupSchemasByContentType(schemas) {
  const grouped = new Map();
  for (const schema of schemas) {
    const body = HttpApiSchema.isWithHeaders(schema) ? schema.schema : schema;
    const contentType = HttpApiSchema.isNoContent(body.ast) ? "" : MediaType.normalize(HttpApiSchema.getResponseEncodingSchema(schema).contentType);
    const existing = grouped.get(contentType);
    if (existing === undefined) {
      grouped.set(contentType, [schema]);
    } else {
      existing.push(schema);
    }
  }
  return grouped;
}
function failUnsupportedContentType(response, contentType, alternatives) {
  const expected = Array.from(new Set(alternatives.map(alternative => alternative.contentType))).join(", ");
  return Effect.fail(new HttpClientError.HttpClientError({
    reason: new HttpClientError.DecodeError({
      request: response.request,
      response,
      description: `Unsupported response content-type for status ${response.status}: ${contentType || "<missing>"}. Expected one of: ${expected}`
    })
  }));
}
const reservedStreamFailureEvent = "effect/http-api/stream/failure";
const isWithHeadersStreamSuccess = schema => HttpApiSchema.isWithHeaders(schema);
function getStreamSuccessSchemas(endpoint) {
  const schemas = [];
  for (const schema of endpoint.success) {
    const body = HttpApiSchema.isWithHeaders(schema) ? schema.schema : schema;
    if (HttpApiSchema.isStreamSchema(body)) {
      schemas.push(schema);
    }
  }
  return schemas;
}
function streamToResponse(successSchema, options, headersOptions) {
  const isWithHeaders = isWithHeadersStreamSuccess(successSchema);
  const streamSchema = isWithHeaders ? successSchema.schema : successSchema;
  const sse = HttpApiSchema.isStreamUint8Array(streamSchema) ? undefined : {
    declaration: streamSchema,
    decoder: makeSseDecoder(streamSchema, options)
  };
  const toStream = (response, sseOptions) => Effect.map(Effect.context(), context => Stream.provideContext(sse === undefined ? response.stream : decodeSseStream(response.stream, sse.declaration, sse.decoder(sseOptions)), context));
  if (!isWithHeaders) return toStream;
  const decodeHeaders = Schema.decodeUnknownEffect(successSchema.headers, headersOptions);
  return (response, sseOptions) => Effect.flatMap(decodeHeaders(response.headers), headers => Effect.map(toStream(response, sseOptions), body => HttpApiSchema.withHeaders({
    body,
    headers
  })));
}
function makeSseDecoder(declaration, parseOptions) {
  const Event = Schema.Union([Schema.Struct({
    id: Schema.optional(Schema.String),
    event: Schema.Literal(reservedStreamFailureEvent),
    data: Schema.fromJsonString(Schema.toCodecJson(Schema.Cause(declaration.error, Schema.Defect())))
  }), declaration.events]);
  const defaultDecoder = Sse.decodeSchema(Event, undefined, parseOptions);
  return options => options === undefined ? defaultDecoder : Sse.decodeSchema(Event, options, parseOptions);
}
function decodeSseStream(stream, declaration, decoder) {
  const events = Stream.transformPull(stream.pipe(Stream.decodeText, Stream.pipeThroughChannel(decoder)), pull => Effect.sync(() => {
    let pendingFailureCause = undefined;
    return Effect.suspend(() => {
      if (pendingFailureCause !== undefined) {
        return Effect.failCause(pendingFailureCause);
      }
      return Effect.flatMap(pull, events => {
        for (let i = 0; i < events.length; i++) {
          const event = events[i];
          if (event.event === reservedStreamFailureEvent && Cause.isCause(event.data)) {
            if (i === 0) {
              return Effect.failCause(event.data);
            }
            pendingFailureCause = event.data;
            events = events.slice(0, i);
            break;
          }
        }
        return Effect.succeed(events);
      });
    });
  }));
  if (declaration.sseMode === "data") {
    return Stream.map(events, event => event.data);
  }
  return events;
}
const ArrayBuffer = /*#__PURE__*/Schema.instanceOf(globalThis.ArrayBuffer, {
  expected: "ArrayBuffer"
});
// _tag: Uint8Array
const Uint8ArrayFromArrayBuffer = /*#__PURE__*/ArrayBuffer.pipe(/*#__PURE__*/Schema.decodeTo(Schema.Uint8Array, /*#__PURE__*/SchemaTransformation.transform({
  decode(fromA) {
    return new Uint8Array(fromA);
  },
  encode(arr) {
    return arr.byteLength === arr.buffer.byteLength ? arr.buffer : arr.buffer.slice(arr.byteOffset, arr.byteOffset + arr.byteLength);
  }
})));
// _tag: Text
const StringFromArrayBuffer = /*#__PURE__*/ArrayBuffer.pipe(/*#__PURE__*/Schema.decodeTo(Schema.String, /*#__PURE__*/SchemaTransformation.transform({
  decode(fromA) {
    return new TextDecoder().decode(fromA);
  },
  encode(toI) {
    const arr = new TextEncoder().encode(toI);
    return arr.byteLength === arr.buffer.byteLength ? arr.buffer : arr.buffer.slice(arr.byteOffset, arr.byteOffset + arr.byteLength);
  }
})));
// _tag: Json
const UnknownFromArrayBuffer = /*#__PURE__*/StringFromArrayBuffer.pipe(/*#__PURE__*/Schema.decodeTo(/*#__PURE__*/Schema.Union([
/*#__PURE__*/
// Handle No Content
Schema.Literal("").pipe(/*#__PURE__*/Schema.decodeTo(Schema.Undefined, /*#__PURE__*/SchemaTransformation.transform({
  decode: () => undefined,
  encode: () => ""
}))), Schema.UnknownFromJsonString])));
function toCodecArrayBuffer(schemas) {
  return Schema.Union(schemas.map(onSchema));
  function onSchema(schema) {
    return fromArrayBuffer(schema).pipe(Schema.decodeTo(schema));
  }
}
function fromArrayBuffer(schema) {
  const encoding = HttpApiSchema.getResponseEncoding(schema.ast);
  switch (encoding._tag) {
    case "Json":
      {
        // handle json codecs that transform void schemas to null
        const encodedIsNull = SchemaAST.isNull(SchemaAST.toEncoded(schema.ast));
        return encodedIsNull ? UnknownFromArrayBuffer.pipe(Schema.decodeTo(Schema.Unknown, SchemaTransformation.transform({
          decode: a => a === undefined ? null : a,
          encode: a => a === null ? undefined : a
        }))) : UnknownFromArrayBuffer;
      }
    case "FormUrlEncoded":
      return StringFromArrayBuffer.pipe(Schema.decodeTo(Schema.RecordFromUrlParams, SchemaTransformation.transform({
        decode: text => UrlParams.fromInput(new URLSearchParams(text)),
        encode: UrlParams.toString
      })));
    case "Uint8Array":
      return Uint8ArrayFromArrayBuffer;
    case "Text":
      return StringFromArrayBuffer;
  }
}
const statusOrElse = response => Effect.fail(new HttpClientError.HttpClientError({
  reason: new HttpClientError.DecodeError({
    request: response.request,
    response
  })
}));
const $HttpBody = /*#__PURE__*/Schema.declare(HttpBody.isHttpBody);
function getEncodePayloadSchema(schemas, method) {
  return Schema.Union(schemas.map(s => getEncodePayloadSchemaFromBody(s, method)));
}
const bodyFromPayloadCache = /*#__PURE__*/new WeakMap();
function getEncodePayloadSchemaFromBody(schema, method) {
  const ast = schema.ast;
  const cached = bodyFromPayloadCache.get(ast);
  if (cached !== undefined) {
    return cached;
  }
  const encoding = HttpApiSchema.getPayloadEncoding(ast, method);
  const out = $HttpBody.pipe(Schema.decodeTo(schema, SchemaTransformation.transformEffect({
    decode(input, options) {
      return Effect.fail(new SchemaIssue.Forbidden({
        message: "Encode only schema"
      }, input, options));
    },
    encode(t, options) {
      switch (encoding._tag) {
        case "Multipart":
          return Effect.fail(new SchemaIssue.Forbidden({
            message: "Payload must be a FormData"
          }, t, options));
        case "Json":
          {
            try {
              const body = JSON.stringify(t);
              return Effect.succeed(HttpBody.text(body, encoding.contentType));
            } catch {
              return Effect.fail(new SchemaIssue.InvalidValue({
                expected: "a JSON-serializable request body"
              }, t, options));
            }
          }
        case "Text":
          {
            if (typeof t !== "string") {
              return Effect.fail(new SchemaIssue.InvalidValue({
                message: "Expected a string"
              }, t, options));
            }
            return Effect.succeed(HttpBody.text(t, encoding.contentType));
          }
        case "FormUrlEncoded":
          {
            if (!Predicate.isObject(t)) {
              return Effect.fail(new SchemaIssue.InvalidValue({
                message: "Expected a record"
              }, t, options));
            }
            return Effect.succeed(HttpBody.urlParams(UrlParams.fromInput(t), encoding.contentType));
          }
        case "Uint8Array":
          {
            if (!(t instanceof Uint8Array)) {
              return Effect.fail(new SchemaIssue.InvalidValue({
                message: "Expected a Uint8Array"
              }, t, options));
            }
            return Effect.succeed(HttpBody.uint8Array(t, encoding.contentType));
          }
      }
    }
  })));
  bodyFromPayloadCache.set(ast, out);
  return out;
}
//# sourceMappingURL=HttpApiClient.js.map