/**
 * Defines endpoint declarations used inside an HTTP API group.
 *
 * An endpoint records a stable identifier, HTTP method, router path, request schemas,
 * response schemas, declared errors, middleware, and annotations. Endpoint
 * values are declarations, not handlers: builders use them to decode requests,
 * type handler input, encode responses, generate OpenAPI metadata, and derive
 * generated-client call signatures. This module also includes HTTP method
 * constructors, payload and response schema helpers, and type utilities used by
 * builders and generated clients.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Context from "../Context.js";
import { identity, memoize } from "../Function.js";
import * as HttpRouter from "../http/HttpRouter.js";
import { pipeArguments } from "../Pipeable.js";
import * as Predicate from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as AST from "../SchemaAST.js";
import * as HttpApiSchema from "./HttpApiSchema.js";
import * as MediaType from "./internal/mediaType.js";
const TypeId = "~effect/http-api/HttpApiEndpoint";
/**
 * Returns `true` when a value is an `HttpApiEndpoint`, narrowing the value to the
 * endpoint interface.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isHttpApiEndpoint = u => Predicate.hasProperty(u, TypeId);
/**
 * @internal
 */
export function getPayloadSchemas(endpoint) {
  const result = [];
  for (const {
    schemas
  } of endpoint.payload.values()) {
    result.push(...schemas);
  }
  return result;
}
/**
 * @internal
 */
export function getSuccessSchemas(endpoint) {
  const schemas = Array.from(endpoint.success);
  return Arr.isArrayNonEmpty(schemas) ? schemas : [HttpApiSchema.NoContent];
}
/**
 * @internal
 */
export function getErrorSchemas(endpoint) {
  const schemas = new Set(endpoint.error);
  const transform = endpoint.disableCodecs ? identity : transformResponseSchema;
  for (const middleware of endpoint.middlewares) {
    const key = middleware;
    for (const schema of key.error) {
      schemas.add(transform(schema));
    }
  }
  return Array.from(schemas);
}
const Proto = {
  [TypeId]: TypeId,
  pipe() {
    return pipeArguments(this, arguments);
  },
  prefix(prefix) {
    return makeProto({
      ...optionsFromEndpoint(this),
      path: HttpRouter.prefixPath(this.path, prefix)
    });
  },
  middleware(middleware) {
    return makeProto({
      ...optionsFromEndpoint(this),
      middlewares: new Set([...this.middlewares, middleware])
    });
  },
  annotate(key, value) {
    return makeProto({
      ...optionsFromEndpoint(this),
      annotations: Context.add(this.annotations, key, value)
    });
  },
  annotateMerge(annotations) {
    return makeProto({
      ...optionsFromEndpoint(this),
      annotations: Context.merge(this.annotations, annotations)
    });
  }
};
const optionsFromEndpoint = endpoint => ({
  identifier: endpoint.identifier,
  path: endpoint.path,
  method: endpoint.method,
  params: endpoint.params,
  query: endpoint.query,
  headers: endpoint.headers,
  payload: endpoint.payload,
  success: endpoint.success,
  error: endpoint.error,
  annotations: endpoint.annotations,
  middlewares: endpoint.middlewares,
  disableCodecs: endpoint.disableCodecs
});
function makeProto(options) {
  function HttpApiEndpoint() {}
  Object.setPrototypeOf(HttpApiEndpoint, Proto);
  return Object.assign(HttpApiEndpoint, options);
}
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
export const make = method => (identifier, path, options) => {
  const disableCodecs = options?.disableCodecs ?? false;
  const transformStringTree = disableCodecs ? identity : Schema.toCodecStringTree;
  return makeProto({
    identifier,
    path,
    method,
    params: ensureStruct(options?.params, transformStringTree),
    query: ensureStruct(options?.query, transformStringTree),
    headers: ensureStruct(options?.headers, transformStringTree),
    payload: getPayload(options?.payload, method, disableCodecs),
    success: getSuccessResponse(options?.success, method, disableCodecs),
    error: getErrorResponse(options?.error, disableCodecs),
    annotations: Context.empty(),
    middlewares: new Set(),
    disableCodecs
  });
};
function ensureStruct(params, transform) {
  if (params === undefined) return undefined;
  if (Schema.isSchema(params)) return transform(params);
  return transform(Schema.Struct(params));
}
function getPayload(payload, method, disableCodecs) {
  const result = new Map();
  if (payload === undefined) return result;
  const schemas = Array.isArray(payload) ? payload : Schema.isSchema(payload) ? [payload] : [Schema.Struct(payload).pipe(HttpApiSchema.asFormUrlEncoded())];
  const transform = disableCodecs ? identity : transformPayload;
  for (const schema of schemas) {
    const encoding = HttpApiSchema.getPayloadEncoding(schema.ast, method);
    const contentType = MediaType.normalize(encoding.contentType);
    const existing = result.get(contentType);
    if (existing) {
      if (existing.encoding._tag !== encoding._tag) {
        throw new Error(`Multiple payload encodings for content-type: ${encoding.contentType}`);
      }
      if (existing.encoding._tag === "Multipart") {
        throw new Error(`Multiple multipart payloads for content-type: ${encoding.contentType}`);
      }
      existing.schemas.push(transform(schema, method));
    } else {
      result.set(contentType, {
        encoding,
        schemas: [transform(schema, method)]
      });
    }
  }
  return result;
}
const reservedStreamFailureEvent = "effect/http-api/stream/failure";
function getSuccessResponse(success, method, disableCodecs) {
  if (success === undefined) return new Set();
  const schemas = Arr.ensure(success);
  validateSuccessResponse(schemas, method);
  return new Set(disableCodecs ? schemas : schemas.map(transformResponseSchema));
}
const transformResponseSchema = /*#__PURE__*/memoize(schema => {
  if (HttpApiSchema.isStreamSchema(schema)) return schema;
  if (HttpApiSchema.isWithHeaders(schema)) {
    const inner = HttpApiSchema.isStreamSchema(schema.schema) ? schema.schema : applyResponseEncoding(schema.schema, HttpApiSchema.getResponseEncodingSchema(schema));
    return HttpApiSchema.rebuildWithHeaders(schema, inner, Schema.toCodecStringTree(schema.headers));
  }
  return transformResponse(schema);
});
function getErrorResponse(error, disableCodecs) {
  if (error === undefined) return new Set();
  const schemas = Arr.ensure(error);
  for (const schema of schemas) {
    const body = HttpApiSchema.isWithHeaders(schema) ? schema.schema : schema;
    if (HttpApiSchema.isStreamSchema(body)) {
      throw new Error("Streaming schemas are not supported in error responses");
    }
  }
  validateResponseExclusivity(schemas, HttpApiSchema.getStatusErrorSchema);
  return new Set(disableCodecs ? schemas : schemas.map(transformResponseSchema));
}
function validateSuccessResponse(schemas, method) {
  let hasStream = false;
  const statuses = new Map();
  for (const schema of schemas) {
    const inner = HttpApiSchema.isWithHeaders(schema) ? schema.schema : schema;
    const status = HttpApiSchema.getStatusSuccessSchema(schema);
    if (HttpApiSchema.isStreamSchema(inner)) {
      validateStreamSuccess(inner, method);
      if (hasStream) {
        throw new Error("Multiple streaming success responses are not supported");
      }
      hasStream = true;
      const entry = getStatusEntry(statuses, status);
      if (entry.noContent) {
        throw new Error(`Cannot combine no-content and streaming success responses for status: ${status}`);
      }
      if (entry.bufferedContentTypes.has(MediaType.normalize(inner.contentType))) {
        throw new Error(`Cannot combine buffered and streaming success responses for status ${status} and content-type: ${inner.contentType}`);
      }
      statuses.set(status, {
        ...entry,
        stream: inner
      });
    } else {
      const entry = getStatusEntry(statuses, status);
      const noContent = HttpApiSchema.isNoContent(inner.ast);
      if (entry.stream !== undefined) {
        if (noContent) {
          throw new Error(`Cannot combine no-content and streaming success responses for status: ${status}`);
        }
        const encoding = HttpApiSchema.getResponseEncodingSchema(schema);
        if (MediaType.normalize(encoding.contentType) === MediaType.normalize(entry.stream.contentType)) {
          throw new Error(`Cannot combine buffered and streaming success responses for status ${status} and content-type: ${encoding.contentType}`);
        }
      }
      if (!noContent) {
        entry.bufferedContentTypes.add(MediaType.normalize(HttpApiSchema.getResponseEncodingSchema(schema).contentType));
      }
      entry.noContent = entry.noContent || noContent;
    }
  }
  validateResponseExclusivity(schemas, HttpApiSchema.getStatusSuccessSchema);
}
function getStatusEntry(statuses, status) {
  let entry = statuses.get(status);
  if (entry === undefined) {
    entry = {
      bufferedContentTypes: new Set(),
      noContent: false
    };
    statuses.set(status, entry);
  }
  return entry;
}
function validateResponseExclusivity(schemas, getStatus) {
  const statuses = new Map();
  for (const schema of schemas) {
    const status = getStatus(schema);
    const withHeadersAnnotation = HttpApiSchema.getWithHeadersAnnotation(schema.ast);
    const body = HttpApiSchema.isWithHeaders(schema) ? schema.schema : withHeadersAnnotation?.body ?? schema;
    const contentType = HttpApiSchema.isNoContent(body.ast) ? "" : MediaType.normalize(HttpApiSchema.isStreamSchema(body) ? body.contentType : HttpApiSchema.getResponseEncodingSchema(schema).contentType);
    let entry = statuses.get(status);
    if (entry === undefined) {
      entry = {
        headerContentType: undefined,
        plainContentTypes: new Set()
      };
      statuses.set(status, entry);
    }
    const combineError = () => new Error(`Cannot combine a response with headers with another response for status ${status} and content-type: ${contentType || "<no content>"}`);
    if (HttpApiSchema.isWithHeaders(schema) || withHeadersAnnotation !== undefined) {
      if (entry.headerContentType !== undefined) {
        throw new Error(`Cannot declare multiple responses with headers for status ${status}`);
      }
      if (entry.plainContentTypes.has(contentType)) throw combineError();
      entry.headerContentType = contentType;
    } else {
      if (entry.headerContentType === contentType) throw combineError();
      entry.plainContentTypes.add(contentType);
    }
  }
}
function validateStreamSuccess(schema, method) {
  if (method === "HEAD") {
    throw new Error("HEAD endpoints cannot declare streaming success responses");
  }
  if (HttpApiSchema.isStreamSse(schema) && hasReservedSseEventName(schema.events.ast)) {
    throw new Error(`SSE event name is reserved: ${reservedStreamFailureEvent}`);
  }
}
function hasReservedSseEventName(ast) {
  return hasReservedEventName(AST.toEncoded(ast), new Set());
}
function hasReservedEventName(ast, seen) {
  if (seen.has(ast)) return false;
  seen.add(ast);
  if (AST.isUnion(ast)) {
    return ast.types.some(type => hasReservedEventName(type, seen));
  }
  if (AST.isSuspend(ast)) {
    return hasReservedEventName(ast.thunk(), seen);
  }
  if (!AST.isObjects(ast)) return false;
  const event = ast.propertySignatures.find(ps => ps.name === "event");
  return event !== undefined && hasReservedEventLiteral(event.type, seen);
}
function hasReservedEventLiteral(ast, seen) {
  if (seen.has(ast)) return false;
  seen.add(ast);
  const encoded = AST.toEncoded(ast);
  if (encoded !== ast) {
    return hasReservedEventLiteral(encoded, seen);
  }
  if (AST.isLiteral(ast)) {
    return ast.literal === reservedStreamFailureEvent;
  }
  if (AST.isUnion(ast)) {
    return ast.types.some(type => hasReservedEventLiteral(type, seen));
  }
  if (AST.isSuspend(ast)) {
    return hasReservedEventLiteral(ast.thunk(), seen);
  }
  return false;
}
function transformResponse(schema) {
  const encoding = HttpApiSchema.getResponseEncoding(schema.ast);
  const withHeaders = HttpApiSchema.getWithHeadersAnnotation(schema.ast);
  if (withHeaders === undefined) {
    return applyResponseEncoding(schema, encoding);
  }
  return schema.annotate({
    "~httpApiWithHeaders": {
      ...withHeaders,
      bodyCodec: applyResponseEncoding(withHeaders.body, encoding),
      headersCodec: Schema.toCodecStringTree(withHeaders.headers)
    }
  });
}
function applyResponseEncoding(schema, encoding) {
  switch (encoding._tag) {
    case "Json":
      return Schema.toCodecJson(schema);
    case "FormUrlEncoded":
      return Schema.toCodecStringTree(schema);
    case "Text":
    case "Uint8Array":
      return schema;
  }
}
function transformPayload(schema, method) {
  const encoding = HttpApiSchema.getPayloadEncoding(schema.ast, method);
  switch (encoding._tag) {
    case "Json":
      return Schema.toCodecJson(schema);
    case "FormUrlEncoded":
      return Schema.toCodecStringTree(schema);
    case "Text":
    case "Uint8Array":
    case "Multipart":
      return schema;
  }
}
/**
 * Creates a `GET` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const get = /*#__PURE__*/make("GET");
/**
 * Creates a `QUERY` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const query = /*#__PURE__*/make("QUERY");
/**
 * Creates a `POST` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const post = /*#__PURE__*/make("POST");
/**
 * Creates a `PUT` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const put = /*#__PURE__*/make("PUT");
/**
 * Creates a `PATCH` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const patch = /*#__PURE__*/make("PATCH");
const del = /*#__PURE__*/make("DELETE");
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
export const head = /*#__PURE__*/make("HEAD");
/**
 * Creates an `OPTIONS` endpoint declaration.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const options = /*#__PURE__*/make("OPTIONS");
//# sourceMappingURL=HttpApiEndpoint.js.map