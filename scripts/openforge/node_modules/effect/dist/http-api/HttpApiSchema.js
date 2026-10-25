/**
 * Attaches HTTP API metadata to Effect Schema values.
 *
 * This module is the schema-side bridge for HttpApi endpoint builders,
 * generated clients, and OpenAPI support. It does not define routes or perform
 * IO. Instead, the helpers annotate schemas so the surrounding HTTP API tooling
 * can choose response status codes, content types, body codecs, multipart
 * handling, and no-body response behavior.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Sse from "../encoding/Sse.js";
import { constVoid } from "../Function.js";
import { hasBody } from "../http/HttpMethod.js";
import * as HttpStatus from "../http/HttpStatus.js";
import * as Predicate from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as SchemaAST from "../SchemaAST.js";
import * as SchemaTransformation from "../SchemaTransformation.js";
import * as Stream from "../Stream.js";
const StreamSchemaTypeId = "~effect/http-api/HttpApiSchema/Stream";
/**
 * @stability unstable
 */
export function status(code) {
  const statusCode = typeof code === "string" ? HttpStatus.fromLiteral(code) : code;
  return self => self.annotate({
    httpApiStatus: statusCode
  });
}
/**
 * Creates a void schema with the given HTTP status code.
 * This is used to represent empty responses with a specific status code.
 *
 * @see {@link NoContent} for the predefined 204 no content schema.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Empty = code => Schema.Void.pipe(status(code));
/**
 * Schema for empty HTTP responses with status code 204.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const NoContent = /*#__PURE__*/Empty(204);
/**
 * Schema for empty HTTP responses with status code 201.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Created = /*#__PURE__*/Empty(201);
/**
 * Schema for empty HTTP responses with status code 202.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Accepted = /*#__PURE__*/Empty(202);
/**
 * Marks a schema as a no-content response while preserving a decoded client value.
 *
 * **Details**
 *
 * The server encodes the response as `void`; generated clients call `decode` to
 * produce the schema's decoded value when the response has no body.
 *
 * @see {@link NoContent} for a void schema with the status code 204.
 * @see {@link Empty} for creating a void schema with a specific status code.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asNoContent(options) {
  return self => {
    return Schema.Void.pipe(Schema.decodeTo(Schema.toType(self), SchemaTransformation.transform({
      decode: options.decode,
      encode: constVoid
    })));
  };
}
const streamSchema = /*#__PURE__*/Schema.declare(Stream.isStream);
/**
 * Creates a Server-Sent Events streaming success response schema.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const StreamSse = options => {
  const events = options.events ?? (options.data === undefined ? undefined : Schema.Struct({
    ...Sse.EventEncoded.fields,
    data: Schema.fromJsonString(options.data)
  }));
  if (events === undefined) {
    throw new Error("StreamSse requires either an events schema or a data schema");
  }
  return Schema.make(streamSchema.ast, {
    [StreamSchemaTypeId]: StreamSchemaTypeId,
    _tag: "StreamSse",
    mode: "sse",
    sseMode: options.events === undefined ? "data" : "events",
    contentType: options.contentType ?? defaultStreamContentType("sse"),
    events,
    error: options.error ?? Schema.Never
  });
};
/**
 * Creates a streaming `Uint8Array` success response schema.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const StreamUint8Array = options => Schema.make(streamSchema.ast, {
  [StreamSchemaTypeId]: StreamSchemaTypeId,
  _tag: "StreamUint8Array",
  mode: "uint8array",
  contentType: options?.contentType ?? defaultStreamContentType("uint8array")
});
/**
 * @internal
 */
export const isStreamSchema = u => Schema.isSchema(u) && Predicate.hasProperty(u, StreamSchemaTypeId);
/**
 * @internal
 */
export const isStreamSse = u => isStreamSchema(u) && u._tag === "StreamSse";
/**
 * @internal
 */
export const isStreamUint8Array = u => isStreamSchema(u) && u._tag === "StreamUint8Array";
function defaultStreamContentType(mode) {
  switch (mode) {
    case "sse":
      return "text/event-stream";
    case "uint8array":
      return "application/octet-stream";
  }
}
/**
 * Runtime brand key used to mark `WithHeaders` response schemas.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const WithHeadersTypeId = "~effect/http-api/HttpApiSchema/WithHeaders";
/**
 * Runtime brand key used to mark `WithHeaders` response values.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const WithHeadersValueTypeId = "~effect/http-api/HttpApiSchema/WithHeadersValue";
/**
 * @internal
 */
export const isWithHeadersValue = u => Predicate.hasProperty(u, WithHeadersValueTypeId);
const withHeadersValueSchema = /*#__PURE__*/Schema.declare(isWithHeadersValue);
/**
 * @stability unstable
 */
export function WithHeaders(schema, headers) {
  if (isWithHeaders(schema)) {
    throw new Error("WithHeaders schemas cannot be nested");
  }
  return Schema.make(withHeadersValueSchema.ast, {
    [WithHeadersTypeId]: WithHeadersTypeId,
    schema,
    headers: Schema.isSchema(headers) ? headers : Schema.Struct(headers)
  });
}
/**
 * Constructs a `WithHeaders` response value from a body and headers.
 *
 * **Details**
 *
 * The returned value is branded so servers and clients can detect it exactly,
 * including in mixed success unions. The same shape is used on both sides: a
 * value received from a client can be returned from another handler unchanged.
 *
 * See {@link WithHeaders} for an example that constructs a schema and its
 * corresponding response value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const withHeaders = options => ({
  [WithHeadersValueTypeId]: WithHeadersValueTypeId,
  body: options.body,
  headers: options.headers
});
/**
 * Returns `true` when a schema is a `WithHeaders` response schema.
 *
 * **Example** (Detecting a response headers schema)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApiSchema } from "effect/http-api"
 *
 * const schema = HttpApiSchema.WithHeaders(Schema.String, {
 *   "x-request-id": Schema.String
 * })
 *
 * HttpApiSchema.isWithHeaders(schema) // => true
 * HttpApiSchema.isWithHeaders(Schema.String) // => false
 * ```
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isWithHeaders = u => Schema.isSchema(u) && Predicate.hasProperty(u, WithHeadersTypeId);
/**
 * @internal
 */
export function rebuildWithHeaders(self, schema, headers) {
  return Schema.make(self.ast, {
    [WithHeadersTypeId]: WithHeadersTypeId,
    schema,
    headers
  });
}
/**
 * Encodes a schema as a `{ body, headers }` pair, folding response headers into
 * an opaque domain type such as an error class.
 *
 * **Details**
 *
 * The encoded side is the pair of the body schema and the headers fields; the
 * Type stays the source schema's Type. The body schema is authoritative for
 * everything wire-level: status, content type, and response encoding resolve
 * from the body schema's annotations.
 *
 * The mappings are pure total functions: validation lives in the body and
 * header schemas, the mappings only reshape valid data.
 *
 * Streams used as the body schema turn mid-stream transport errors into
 * defects on the client. Stream responses should use {@link WithHeaders},
 * which preserves the body stream's error channel in the generated client.
 *
 * **Example** (Encoding an error with headers)
 *
 * ```ts import.meta.vitest
 * import { Schema } from "effect"
 * import { HttpApiSchema } from "effect/http-api"
 *
 * class UserNotFound extends Schema.TaggedError<UserNotFound>()("UserNotFound", {
 *   userId: Schema.Int
 * }) {}
 *
 * const UserNotFoundWithHeaders = UserNotFound.pipe(
 *   HttpApiSchema.encodeToWithHeaders({
 *     body: HttpApiSchema.Empty(404),
 *     headers: {
 *       "x-user-id": Schema.Int
 *     }
 *   }, {
 *     decode: ({ headers }) => new UserNotFound({ userId: headers["x-user-id"] }),
 *     encode: (error) => ({
 *       headers: { "x-user-id": error.userId },
 *       body: undefined
 *     })
 *   })
 * )
 *
 * const encoded = Schema.encodeSync(UserNotFoundWithHeaders)(new UserNotFound({ userId: 123 }))
 * encoded // => { body: undefined, headers: { "x-user-id": 123 } }
 * ```
 *
 * @see {@link WithHeaders} for the structural wrapper recommended for success
 * responses, including streams.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function encodeToWithHeaders(options, transformation) {
  return self => {
    const body = options.body;
    const headers = Schema.Struct(options.headers);
    const status = resolveHttpApiStatus(body.ast);
    const encoding = resolveHttpApiEncoding(body.ast);
    return Schema.Struct({
      body,
      headers
    }).pipe(Schema.decodeTo(Schema.toType(self), SchemaTransformation.transform(transformation))).annotate({
      "~httpApiWithHeaders": {
        body,
        headers,
        bodyCodec: body,
        headersCodec: headers,
        transformation
      },
      httpApiStatus: status,
      "~httpApiEncoding": encoding
    });
  };
}
/**
 * Runtime brand key used to mark schemas as buffered multipart payloads.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const MultipartTypeId = "~effect/http-api/HttpApiSchema/Multipart";
/**
 * Marks a schema as a multipart payload.
 *
 * @see {@link asMultipartStream} for a multipart stream payload.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asMultipart(options) {
  return self => self.pipe(Schema.brand(MultipartTypeId)).annotate({
    "~httpApiEncoding": {
      _tag: "Multipart",
      mode: "buffered",
      contentType: defaultContentType("Multipart"),
      limits: options
    }
  });
}
/**
 * Runtime brand key used to mark schemas as streaming multipart payloads.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const MultipartStreamTypeId = "~effect/http-api/HttpApiSchema/MultipartStream";
/**
 * Marks a schema as a multipart stream payload.
 *
 * @see {@link asMultipart} for a buffered multipart payload.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asMultipartStream(options) {
  return self => self.pipe(Schema.brand(MultipartStreamTypeId)).annotate({
    "~httpApiEncoding": {
      _tag: "Multipart",
      mode: "stream",
      contentType: defaultContentType("Multipart"),
      limits: options
    }
  });
}
function asNonMultipartEncoding(self, options) {
  return self.annotate({
    "~httpApiEncoding": {
      _tag: options._tag,
      contentType: options.contentType ?? defaultContentType(options._tag)
    }
  });
}
function defaultContentType(_tag) {
  switch (_tag) {
    case "Multipart":
      return "multipart/form-data";
    case "Json":
      return "application/json";
    case "FormUrlEncoded":
      return "application/x-www-form-urlencoded";
    case "Uint8Array":
      return "application/octet-stream";
    case "Text":
      return "text/plain";
  }
}
/**
 * Marks a schema as a JSON payload / response.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asJson(options) {
  return self => asNonMultipartEncoding(self, {
    _tag: "Json",
    ...options
  });
}
/**
 * Marks a schema as an `application/x-www-form-urlencoded` payload or response.
 *
 * **Details**
 *
 * The schema's encoded side must be a record of strings.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asFormUrlEncoded(options) {
  return self => asNonMultipartEncoding(self, {
    _tag: "FormUrlEncoded",
    ...options
  });
}
/**
 * Marks a schema as a text payload / response.
 *
 * **Details**
 *
 * The schema encoded side must be a string.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asText(options) {
  return self => asNonMultipartEncoding(self, {
    _tag: "Text",
    ...options
  });
}
/**
 * Marks a schema as a binary payload / response.
 *
 * **Details**
 *
 * The schema encoded side must be a `Uint8Array`.
 *
 * @stability unstable
 * @category encoding
 * @since 4.0.0
 */
export function asUint8Array(options) {
  return self => asNonMultipartEncoding(self, {
    _tag: "Uint8Array",
    ...options
  });
}
/**
 * Returns `true` when a schema AST represents a no-content response.
 *
 * **Details**
 *
 * The check succeeds for direct `void` schemas and schemas whose encoded or
 * transformation target is `void`.
 *
 * @stability unstable
 * @category predicates
 * @since 4.0.0
 */
export const isNoContent = ast => {
  if (SchemaAST.isVoid(ast)) return true;
  const encoded = SchemaAST.toEncoded(ast);
  if (SchemaAST.isVoid(encoded)) return true;
  const target = ast.encoding?.[0].to;
  if (target === undefined) return false;
  return SchemaAST.isVoid(target);
};
const resolveHttpApiEncoding = /*#__PURE__*/SchemaAST.resolveAt("~httpApiEncoding");
/**
 * @internal
 */
export const getWithHeadersAnnotation = /*#__PURE__*/SchemaAST.resolveAt("~httpApiWithHeaders");
const resolveHttpApiStatus = /*#__PURE__*/SchemaAST.resolveAt("httpApiStatus");
const defaultJsonEncoding = {
  _tag: "Json",
  contentType: "application/json"
};
const defaultUrlEncodedEncoding = {
  _tag: "FormUrlEncoded",
  contentType: "application/x-www-form-urlencoded"
};
function getEncoding(ast) {
  return resolveHttpApiEncoding(ast) ?? defaultJsonEncoding;
}
/**
 * @internal
 */
export function getPayloadEncoding(ast, method) {
  const encoding = resolveHttpApiEncoding(ast);
  if (encoding) return encoding;
  return hasBody(method) ? defaultJsonEncoding : defaultUrlEncodedEncoding;
}
/**
 * @internal
 */
export function getResponseEncoding(ast) {
  const out = getEncoding(ast);
  if (out._tag === "Multipart") {
    throw new Error("Multipart is not supported in response");
  }
  return out;
}
/**
 * @internal
 */
export function getStatusSuccess(self) {
  return resolveHttpApiStatus(self) ?? 200;
}
/**
 * @internal
 */
export function getStatusSuccessSchema(schema) {
  if (isWithHeaders(schema)) {
    return resolveHttpApiStatus(schema.ast) ?? getStatusSuccess(schema.schema.ast);
  }
  return getStatusSuccess(schema.ast);
}
/**
 * @internal
 */
export function getResponseEncodingSchema(schema) {
  if (isWithHeaders(schema) && resolveHttpApiEncoding(schema.ast) === undefined) {
    return getResponseEncoding(schema.schema.ast);
  }
  return getResponseEncoding(schema.ast);
}
/**
 * @internal
 */
export function getStatusError(self) {
  return resolveHttpApiStatus(self) ?? 500;
}
/**
 * @internal
 */
export function getStatusErrorSchema(schema) {
  if (isWithHeaders(schema)) {
    return resolveHttpApiStatus(schema.ast) ?? getStatusError(schema.schema.ast);
  }
  return getStatusError(schema.ast);
}
//# sourceMappingURL=HttpApiSchema.js.map