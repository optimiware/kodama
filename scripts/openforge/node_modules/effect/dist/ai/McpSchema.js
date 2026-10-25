/**
 * Defines schemas for Model Context Protocol messages.
 *
 * MCP clients and servers use these schemas to describe the JSON-RPC requests,
 * notifications, results, and errors that can cross the protocol boundary.
 * This is the stable public compatibility and authoring surface. It is not an
 * exact dated wire contract: MCP protocol adapters use frozen schemas under
 * `internal/mcpSchema` for decoding and encoding. This module groups the
 * current public message model for application authors and provides helpers
 * for optional fields and parameter metadata. Transport and server behavior
 * live in other modules.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.js";
import * as Data from "../Data.js";
import * as Effect from "../Effect.js";
import { constFalse, constTrue } from "../Function.js";
import * as Option from "../Option.js";
import * as Predicate from "../Predicate.js";
import * as Rpc from "../rpc/Rpc.js";
import * as RpcGroup from "../rpc/RpcGroup.js";
import * as RpcMiddleware from "../rpc/RpcMiddleware.js";
import * as Schema from "../Schema.js";
import * as SchemaGetter from "../SchemaGetter.js";
/**
 * Marks a struct field as optional and supplies `defaultValue` when the field
 * is absent.
 *
 * **Details**
 *
 * The default is used during decoding and as the constructor default for the
 * schema field.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const optionalWithDefault = (schema, defaultValue) => {
  const effect = Effect.sync(defaultValue);
  return Schema.optionalKey(schema).pipe(Schema.decode({
    decode: SchemaGetter.withDefault(effect),
    encode: SchemaGetter.passthrough()
  }), Schema.withConstructorDefault(effect));
};
/**
 * Creates an optional MCP struct-field schema from a required schema.
 *
 * **Details**
 *
 * The field may be absent, and explicit `undefined` values are omitted when
 * encoding.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const optional = schema => Schema.optionalKey(schema).pipe(Schema.decodeTo(Schema.toType(Schema.optional(schema)), {
  decode: SchemaGetter.passthrough(),
  encode: SchemaGetter.transformOptional(Option.flatMap(Option.fromUndefinedOr))
}));
// =============================================================================
// Common
// =============================================================================
/**
 * Schema for JSON-RPC request identifiers, allowing string or number ids.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const RequestId = /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite]);
/**
 * Schema for MCP progress tokens that associate progress notifications with the
 * original request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ProgressToken = /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite]);
/**
 * Schema for optional MCP request metadata.
 *
 * **Details**
 *
 * Request metadata may include a progress token that asks the receiver to send
 * out-of-band progress notifications for the request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class RequestMeta extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  _meta: /*#__PURE__*/optional(/*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
    /**
     * If specified, the caller is requesting out-of-band progress notifications
     * for this request (as represented by notifications/progress). The value of
     * this parameter is an opaque token that will be attached to any subsequent
     * notifications. The receiver is not obligated to provide these
     * notifications.
     */
    progressToken: /*#__PURE__*/optional(ProgressToken)
  }), [Schema.JsonObject]))
})) {}
/**
 * Schema for optional MCP result metadata.
 *
 * **Details**
 *
 * The `_meta` field is reserved for protocol, extension, or implementation
 * metadata attached to a result.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ResultMeta extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * This result property is reserved by the protocol to allow clients and
   * servers to attach additional metadata to their responses.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Schema for optional MCP notification metadata.
 *
 * **Details**
 *
 * The `_meta` field is reserved for protocol, extension, or implementation
 * metadata attached to a notification.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class NotificationMeta extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * This parameter name is reserved by MCP to allow clients and servers to
   * attach additional metadata to their notifications.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Schema for opaque cursor tokens used in pagination.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Cursor = Schema.String;
/**
 * Schema for MCP request metadata used by paginated requests.
 *
 * **Details**
 *
 * It includes the base request metadata fields plus an optional cursor
 * indicating where the server should continue listing results.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class PaginatedRequestMeta extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...RequestMeta.fields,
  /**
   * An opaque token representing the current pagination position.
   * If provided, the server should return results starting after this cursor.
   */
  cursor: /*#__PURE__*/optional(Cursor)
})) {}
/**
 * Schema for MCP result metadata returned by paginated operations.
 *
 * **Details**
 *
 * It includes the base result metadata fields plus an optional `nextCursor`,
 * which indicates that more results may be available.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class PaginatedResultMeta extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  /**
   * An opaque token representing the pagination position after the last returned result.
   * If present, there may be more results available.
   */
  nextCursor: /*#__PURE__*/optional(Cursor)
})) {}
/**
 * Schema for MCP conversation roles, allowing user and assistant.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Role = /*#__PURE__*/Schema.Literals(["user", "assistant"]);
/**
 * Schema for optional client-facing annotations on MCP objects.
 *
 * **When to use**
 *
 * Use to describe intended audience and priority metadata for objects shown or
 * processed by a client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Annotations extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * Describes who the intended customer of this object or data is.
   *
   * It can include multiple entries to indicate content useful for multiple
   * audiences (e.g., `["user", "assistant"]`).
   */
  audience: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Role)),
  /**
   * Describes how important this data is for operating the server.
   *
   * A value of 1 means "most important," and indicates that the data is
   * effectively required, while 0 means "least important," and indicates that
   * the data is entirely optional.
   */
  priority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  }))),
  /**
   * The last time the annotated object was modified, formatted as an ISO 8601
   * timestamp.
   */
  lastModified: /*#__PURE__*/optional(Schema.String)
})) {}
/**
 * Schema for an icon that an MCP client can display.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Icon extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/Icon")({
  /**
   * URI containing the icon image.
   */
  src: Schema.String,
  /**
   * MIME type of the icon, when known.
   */
  mimeType: /*#__PURE__*/optional(Schema.String),
  /**
   * Sizes supported by the icon, such as `"48x48"` or `"any"`.
   */
  sizes: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
  /**
   * Color theme for which the icon was designed.
   */
  theme: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["light", "dark"]))
}) {}
/**
 * Describes the name and version of an MCP implementation.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Implementation extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  version: Schema.String,
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon)),
  description: /*#__PURE__*/optional(Schema.String),
  websiteUrl: /*#__PURE__*/optional(Schema.String)
})) {}
/**
 * Describes capabilities advertised by an MCP client.
 *
 * **When to use**
 *
 * Use to describe which optional MCP features a client supports during
 * initialization.
 *
 * **Details**
 *
 * Known capabilities are represented by this schema, but the capability set is
 * open and clients may define additional capabilities.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ClientCapabilities extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ClientCapabilities")({
  /**
   * Experimental, non-standard capabilities that the client supports.
   */
  experimental: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Struct({}))),
  /**
   * Optional extensions capabilities advertised by the client.
   * Keys are extension identifiers following <vendor-prefix>/<extension-name> (e.g. "io.modelcontextprotocol/ui").
   */
  extensions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(/*#__PURE__*/Schema.TemplateLiteral([Schema.String, "/", Schema.String]), Schema.Json)),
  /**
   * Present if the client supports listing roots.
   */
  roots: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    /**
     * Whether the client supports notifications for changes to the roots list.
     */
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  /**
   * Present if the client supports sampling from an LLM.
   */
  sampling: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    /**
     * Present if the client supports context inclusion during sampling.
     */
    context: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({})),
    /**
     * Present if the client supports tool use during sampling.
     */
    tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({}))
  })),
  /**
   * Present if the client supports elicitation from the server.
   */
  elicitation: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    /**
     * Present if the client supports form-mode elicitation.
     */
    form: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({})),
    /**
     * Present if the client supports URL-mode elicitation.
     */
    url: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({}))
  }))
}) {}
/**
 * Describes capabilities advertised by an MCP server.
 *
 * **When to use**
 *
 * Use to describe which optional MCP features a server supports during
 * initialization.
 *
 * **Details**
 *
 * Known capabilities are represented by this schema, but the capability set is
 * open and servers may define additional capabilities.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ServerCapabilities extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * Experimental, non-standard capabilities that the server supports.
   */
  experimental: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Struct({}))),
  /**
   * Optional extensions capabilities advertised by the server.
   * Keys are extension identifiers following <vendor-prefix>/<extension-name> (e.g. "io.modelcontextprotocol/ui").
   */
  extensions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(/*#__PURE__*/Schema.TemplateLiteral([Schema.String, "/", Schema.String]), Schema.Json)),
  /**
   * Present if the server supports sending log messages to the client.
   */
  logging: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({})),
  /**
   * Present if the server supports argument autocompletion suggestions.
   */
  completions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({})),
  /**
   * Present if the server offers any prompt templates.
   */
  prompts: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    /**
     * Whether this server supports notifications for changes to the prompt list.
     */
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  /**
   * Present if the server offers any resources to read.
   */
  resources: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    /**
     * Whether this server supports subscribing to resource updates.
     */
    subscribe: /*#__PURE__*/optional(Schema.Boolean),
    /**
     * Whether this server supports notifications for changes to the resource list.
     */
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  /**
   * Present if the server offers any tools to call.
   */
  tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    /**
     * Whether this server supports notifications for changes to the tool list.
     */
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  }))
})) {}
// =============================================================================
// Errors
// =============================================================================
/**
 * Schema for MCP and JSON-RPC error objects.
 *
 * **Details**
 *
 * It contains the numeric error `code`, a concise `message`, and optional
 * sender-defined `data`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class McpErrorBase extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/McpErrorBase")({
  /**
   * The error type that occurred.
   */
  code: Schema.Int,
  /**
   * A short description of the error. The message SHOULD be limited to a
   * concise single sentence.
   */
  message: Schema.String,
  /**
   * Additional information about the error. The value of this member is
   * defined by the sender (e.g. detailed error information, nested errors etc.).
   */
  data: /*#__PURE__*/optional(Schema.Any)
}) {}
/**
 * Represents the JSON-RPC error code for requests that are not valid request objects.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for a syntactically parsed
 * request object that fails request-shape validation.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const INVALID_REQUEST_ERROR_CODE = -32600;
/**
 * Represents the JSON-RPC error code for requests whose method does not exist or is not
 * available.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for a request whose
 * `method` is unknown or unavailable.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const METHOD_NOT_FOUND_ERROR_CODE = -32601;
/**
 * Represents the JSON-RPC error code for invalid method parameters.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for decoded request
 * parameters that fail method-specific validation.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const INVALID_PARAMS_ERROR_CODE = -32602;
/**
 * Represents the JSON-RPC error code for internal server errors.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response for an unexpected
 * server-side failure.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const INTERNAL_ERROR_CODE = -32603;
/**
 * Represents the JSON-RPC error code for invalid JSON that could not be parsed.
 *
 * **When to use**
 *
 * Use when building an MCP/JSON-RPC error response before a request object is
 * available because the JSON payload could not be parsed.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const PARSE_ERROR_CODE = -32700;
/**
 * Represents the MCP error code for HTTP headers that do not match the
 * corresponding request values.
 *
 * **When to use**
 *
 * Use when building an MCP error response for missing, malformed, or
 * mismatched request-routing headers.
 *
 * @stability unstable
 * @category constants
 * @since 4.0.0
 */
export const HEADER_MISMATCH_ERROR_CODE = -32020;
/**
 * Represents an MCP/JSON-RPC error for invalid JSON that could not be parsed.
 *
 * **When to use**
 *
 * Use to report a JSON parse failure before a valid JSON-RPC request object is
 * available.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC parse error code `-32700`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class ParseError extends /*#__PURE__*/Schema.Error("effect/ai/McpSchema/ParseError")({
  ...McpErrorBase.fields,
  _tag: /*#__PURE__*/Schema.tag("ParseError"),
  code: /*#__PURE__*/Schema.tag(PARSE_ERROR_CODE)
}) {}
/**
 * Represents an MCP/JSON-RPC error for a request object that is not valid.
 *
 * **When to use**
 *
 * Use to report a syntactically parsed JSON-RPC request that is not a valid
 * request object.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC invalid request code `-32600`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class InvalidRequest extends /*#__PURE__*/Schema.Error("effect/ai/McpSchema/InvalidRequest")({
  ...McpErrorBase.fields,
  _tag: /*#__PURE__*/Schema.tag("InvalidRequest"),
  code: /*#__PURE__*/Schema.tag(INVALID_REQUEST_ERROR_CODE)
}) {}
/**
 * Represents an MCP/JSON-RPC error for an unavailable method.
 *
 * **When to use**
 *
 * Use to report a JSON-RPC method that does not exist or is not available.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC method-not-found code `-32601`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class MethodNotFound extends /*#__PURE__*/Schema.Error("effect/ai/McpSchema/MethodNotFound")({
  ...McpErrorBase.fields,
  _tag: /*#__PURE__*/Schema.tag("MethodNotFound"),
  code: /*#__PURE__*/Schema.tag(METHOD_NOT_FOUND_ERROR_CODE)
}) {}
/**
 * Represents an MCP/JSON-RPC error for invalid method parameters.
 *
 * **When to use**
 *
 * Use to report a request whose method parameters do not match the method
 * schema.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC invalid params code `-32602`.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class InvalidParams extends /*#__PURE__*/Schema.Error("effect/ai/McpSchema/InvalidParams")({
  ...McpErrorBase.fields,
  _tag: /*#__PURE__*/Schema.tag("InvalidParams"),
  code: /*#__PURE__*/Schema.tag(INVALID_PARAMS_ERROR_CODE)
}) {}
/**
 * Represents an MCP/JSON-RPC error for unexpected internal server failures.
 *
 * **When to use**
 *
 * Use to report an unexpected server-side failure while handling a valid
 * request.
 *
 * **Details**
 *
 * Uses the standard JSON-RPC internal error code `-32603` and includes
 * `InternalError.notImplemented` for unimplemented handlers.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class InternalError extends /*#__PURE__*/Schema.Error("effect/ai/McpSchema/InternalError")({
  ...McpErrorBase.fields,
  _tag: /*#__PURE__*/Schema.tag("InternalError"),
  code: /*#__PURE__*/Schema.tag(INTERNAL_ERROR_CODE)
}) {
  static notImplemented = /*#__PURE__*/new InternalError({
    message: "Not implemented"
  });
}
/**
 * Schema for MCP protocol errors returned in JSON-RPC failure responses,
 * including standard protocol errors and custom `McpErrorBase` values.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export const McpError = /*#__PURE__*/Schema.Union([ParseError, InvalidRequest, MethodNotFound, InvalidParams, InternalError, McpErrorBase]);
// =============================================================================
// Ping
// =============================================================================
/**
 * Represents an MCP ping request used to check whether the peer is still alive.
 *
 * **When to use**
 *
 * Use to implement client or server liveness checks.
 *
 * **Details**
 *
 * The receiver should respond promptly; otherwise the sender may disconnect.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class Ping extends /*#__PURE__*/Rpc.make("ping", {
  success: /*#__PURE__*/Schema.Struct({}),
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(RequestMeta)
}) {}
// =============================================================================
// Initialization
// =============================================================================
/**
 * Schema for the server's response to an initialize request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class InitializeResult extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  /**
   * The version of the Model Context Protocol that the server wants to use.
   * This may not match the version that the client requested. If the client
   * cannot support this version, it MUST disconnect.
   */
  protocolVersion: Schema.String,
  capabilities: ServerCapabilities,
  serverInfo: Implementation,
  /**
   * Instructions describing how to use the server and its features.
   *
   * This can be used by clients to improve the LLM's understanding of available
   * tools, resources, etc. It can be thought of like a "hint" to the model.
   * For example, this information MAY be added to the system prompt.
   */
  instructions: /*#__PURE__*/optional(Schema.String)
})) {}
/**
 * Sent from the client to the server when it first connects, asking it to begin
 * initialization.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class Initialize extends /*#__PURE__*/Rpc.make("initialize", {
  success: InitializeResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    /**
     * The latest version of the Model Context Protocol that the client
     * supports. The client MAY decide to support older versions as well.
     */
    protocolVersion: Schema.String,
    /**
     * Capabilities a client may support. Known capabilities are defined here,
     * in this schema, but this is not a closed set: any client can define its
     * own, additional capabilities.
     */
    capabilities: ClientCapabilities,
    /**
     * Describes the name and version of an MCP implementation.
     */
    clientInfo: Implementation
  }
}) {}
/**
 * Sent from the client to the server after initialization has finished.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class InitializedNotification extends /*#__PURE__*/Rpc.make("notifications/initialized", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
// =============================================================================
// Cancellation
// =============================================================================
/**
 * Sent from either peer to cancel a previously issued request in the same
 * direction.
 *
 * **Details**
 *
 * The payload identifies the request to cancel and may include a
 * human-readable reason.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class CancelledNotification extends /*#__PURE__*/Rpc.make("notifications/cancelled", {
  payload: {
    ...NotificationMeta.fields,
    /**
     * The ID of the request to cancel.
     *
     * This MUST correspond to the ID of a request previously issued in the
     * same direction.
     */
    requestId: RequestId,
    /**
     * An optional string describing the reason for the cancellation. This MAY
     * be logged or presented to the user.
     */
    reason: /*#__PURE__*/optional(Schema.String)
  }
}) {}
// =============================================================================
// Progress
// =============================================================================
/**
 * Sent from either peer to report progress for a long-running request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ProgressNotification extends /*#__PURE__*/Rpc.make("notifications/progress", {
  payload: {
    ...NotificationMeta.fields,
    /**
     * The progress token which was given in the initial request, used to
     * associate this notification with the request that is proceeding.
     */
    progressToken: ProgressToken,
    /**
     * The progress thus far. This should increase every time progress is made,
     * even if the total is unknown.
     */
    progress: Schema.Finite,
    /**
     * Total number of items to process (or total progress required), if known.
     */
    total: /*#__PURE__*/optional(Schema.Finite),
    /**
     * An optional message describing the current progress.
     */
    message: /*#__PURE__*/optional(Schema.String)
  }
}) {}
// =============================================================================
// Resources
// =============================================================================
/**
 * Schema for a known resource that the server is capable of reading.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Resource extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/Resource")({
  /**
   * The URI of this resource.
   */
  uri: Schema.String,
  /**
   * A human-readable name for this resource.
   *
   * This can be used by clients to populate UI elements.
   */
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  /**
   * A description of what this resource represents.
   *
   * This can be used by clients to improve the LLM's understanding of available
   * resources. It can be thought of like a "hint" to the model.
   */
  description: /*#__PURE__*/optional(Schema.String),
  /**
   * The MIME type of this resource, if known.
   */
  mimeType: /*#__PURE__*/optional(Schema.String),
  /**
   * Optional annotations for the client.
   */
  annotations: /*#__PURE__*/optional(Annotations),
  /**
   * Icons that clients can display for this resource.
   */
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon)),
  /**
   * The size of the raw resource content, in bytes (i.e., before base64
   * encoding or any tokenization), if known.
   *
   * This can be used by Hosts to display file sizes and estimate context
   * window usage.
   */
  size: /*#__PURE__*/optional(Schema.Int),
  /**
   * Optional additional metadata for the client.
   *
   * This parameter name is reserved by MCP to allow clients and servers to
   * attach additional metadata to resources.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Schema for a template description of resources available on the server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ResourceTemplate extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ResourceTemplate")({
  /**
   * A URI template (according to RFC 6570) that can be used to construct
   * resource URIs.
   */
  uriTemplate: Schema.String,
  /**
   * A human-readable name for the type of resource this template refers to.
   *
   * This can be used by clients to populate UI elements.
   */
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  /**
   * A description of what this template is for.
   *
   * This can be used by clients to improve the LLM's understanding of available
   * resources. It can be thought of like a "hint" to the model.
   */
  description: /*#__PURE__*/optional(Schema.String),
  /**
   * The MIME type for all resources that match this template. This should only
   * be included if all resources matching this template have the same type.
   */
  mimeType: /*#__PURE__*/optional(Schema.String),
  /**
   * Optional annotations for the client.
   */
  annotations: /*#__PURE__*/optional(Annotations),
  /**
   * Icons that clients can display for this resource template.
   */
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon)),
  /**
   * Optional additional metadata for the client.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Schema for the contents of a specific resource or sub-resource.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ResourceContents extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * The URI of this resource.
   */
  uri: Schema.String,
  /**
   * The MIME type of this resource, if known.
   */
  mimeType: /*#__PURE__*/optional(Schema.String),
  /**
   * Optional additional metadata for the client.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Schema for text resource contents represented as a string.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class TextResourceContents extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...ResourceContents.fields,
  /**
   * The text of the item. This must only be set if the item can actually be
   * represented as text (not binary data).
   */
  text: Schema.String
})) {}
/**
 * Schema for binary resource contents represented as a `Uint8Array`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class BlobResourceContents extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...ResourceContents.fields,
  /**
   * The binary data of the item decoded from a base64-encoded string.
   */
  blob: Schema.Uint8ArrayFromBase64
})) {}
/**
 * Schema for the server's response to a resources/list request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ListResourcesResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ListResourcesResult")({
  ...PaginatedResultMeta.fields,
  resources: /*#__PURE__*/Schema.Array(Resource)
}) {}
/**
 * Sent from the client to request a list of resources the server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ListResources extends /*#__PURE__*/Rpc.make("resources/list", {
  success: ListResourcesResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequestMeta)
}) {}
/**
 * Schema for the server's response to a resources/templates/list request from
 * the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ListResourceTemplatesResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ListResourceTemplatesResult")({
  ...PaginatedResultMeta.fields,
  resourceTemplates: /*#__PURE__*/Schema.Array(ResourceTemplate)
}) {}
/**
 * Sent from the client to request a list of resource templates the server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ListResourceTemplates extends /*#__PURE__*/Rpc.make("resources/templates/list", {
  success: ListResourceTemplatesResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequestMeta)
}) {}
/**
 * Schema for the server's response to a resources/read request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ReadResourceResult extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  contents: /*#__PURE__*/Schema.Array(/*#__PURE__*/Schema.Union([TextResourceContents, BlobResourceContents]))
})) {}
/**
 * Sent from the client to the server, to read a specific resource URI.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ReadResource extends /*#__PURE__*/Rpc.make("resources/read", {
  success: ReadResourceResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    /**
     * The URI of the resource to read. The URI can use any protocol; it is up
     * to the server how to interpret it.
     */
    uri: Schema.String
  }
}) {}
/**
 * Represents a notification that the server's resource list changed.
 *
 * **When to use**
 *
 * Use to notify clients that `resources/list` should be requested again.
 *
 * **Details**
 *
 * Servers may send this notification without a previous client subscription.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ResourceListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/resources/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
/**
 * Sent from the client to request resources/updated notifications from the
 * server whenever a particular resource changes.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class Subscribe extends /*#__PURE__*/Rpc.make("resources/subscribe", {
  success: /*#__PURE__*/Schema.Struct({}),
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String
  }
}) {}
/**
 * Sent from the client to request cancellation of resources/updated
 * notifications from the server. This should follow a previous
 * resources/subscribe request.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class Unsubscribe extends /*#__PURE__*/Rpc.make("resources/unsubscribe", {
  success: /*#__PURE__*/Schema.Struct({}),
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    /**
     * The URI of the resource to subscribe to. The URI can use any protocol;
     * it is up to the server how to interpret it.
     */
    uri: Schema.String
  }
}) {}
/**
 * Sent from the server when a subscribed resource URI has changed.
 *
 * **Details**
 *
 * The URI may identify a sub-resource of the resource that the client
 * originally subscribed to.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ResourceUpdatedNotification extends /*#__PURE__*/Rpc.make("notifications/resources/updated", {
  payload: {
    ...NotificationMeta.fields,
    /**
     * The URI of the resource that has been updated. This might be a sub-resource of the one that the client actually subscribed to.
     */
    uri: Schema.String
  }
}) {}
// =============================================================================
// Prompts
// =============================================================================
/**
 * Describes an argument that a prompt can accept.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class PromptArgument extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * The name of the argument.
   */
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  /**
   * A human-readable description of the argument.
   */
  description: /*#__PURE__*/optional(Schema.String),
  /**
   * Whether this argument must be provided.
   */
  required: /*#__PURE__*/optional(Schema.Boolean)
})) {}
/**
 * Represents a prompt or prompt template that the server offers.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Prompt extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/Prompt")({
  /**
   * The name of the prompt or prompt template.
   */
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  /**
   * An optional description of what this prompt provides
   */
  description: /*#__PURE__*/optional(Schema.String),
  /**
   * A list of arguments to use for templating the prompt.
   */
  arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(PromptArgument)),
  /**
   * Icons that clients can display for this prompt.
   */
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon)),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Represents text content provided to or from an LLM.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class TextContent extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.tag("text"),
  /**
   * The text content of the message.
   */
  text: Schema.String,
  /**
   * Optional annotations for the client.
   */
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Represents image content provided to or from an LLM.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ImageContent extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.tag("image"),
  /**
   * The image data.
   */
  data: Schema.Uint8ArrayFromBase64,
  /**
   * The MIME type of the image. Different providers may support different
   * image types.
   */
  mimeType: Schema.String,
  /**
   * Optional annotations for the client.
   */
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Represents audio content provided to or from an LLM.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class AudioContent extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.tag("audio"),
  /**
   * The audio data.
   */
  data: Schema.Uint8ArrayFromBase64,
  /**
   * The MIME type of the audio. Different providers may support different
   * audio types.
   */
  mimeType: Schema.String,
  /**
   * Optional annotations for the client.
   */
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Represents resource contents embedded into a prompt or tool call result.
 *
 * **Details**
 *
 * It is up to the client how best to render embedded resources for the benefit
 * of the LLM and/or the user.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class EmbeddedResource extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.tag("resource"),
  resource: /*#__PURE__*/Schema.Union([TextResourceContents, BlobResourceContents]),
  /**
   * Optional annotations for the client.
   */
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Represents a readable resource included in a prompt or tool call result.
 *
 * **Gotchas**
 *
 * Resource links returned by tools are not guaranteed to appear in the results
 * of `resources/list` requests.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ResourceLink extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...Resource.fields,
  type: /*#__PURE__*/Schema.tag("resource_link")
})) {}
/**
 * Schema for MCP content blocks that can appear in prompt messages or tool
 * results.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ContentBlock = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent, EmbeddedResource, ResourceLink]);
/**
 * Describes a message returned as part of a prompt.
 *
 * **Details**
 *
 * This is similar to `SamplingMessage`, but also supports the embedding of
 * resources from the MCP server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class PromptMessage extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  role: Role,
  content: ContentBlock
})) {}
/**
 * Represents the server response to a prompts/list request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ListPromptsResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ListPromptsResult")({
  ...PaginatedResultMeta.fields,
  prompts: /*#__PURE__*/Schema.Array(Prompt)
}) {}
/**
 * Sent from the client to request a list of prompts and prompt templates the
 * server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ListPrompts extends /*#__PURE__*/Rpc.make("prompts/list", {
  success: ListPromptsResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequestMeta)
}) {}
/**
 * Represents the server response to a prompts/get request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class GetPromptResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/GetPromptResult")({
  ...ResultMeta.fields,
  messages: /*#__PURE__*/Schema.Array(PromptMessage),
  /**
   * An optional description for the prompt.
   */
  description: /*#__PURE__*/optional(Schema.String)
}) {}
/**
 * Sent from the client to get a prompt provided by the server.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class GetPrompt extends /*#__PURE__*/Rpc.make("prompts/get", {
  success: GetPromptResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    /**
     * The name of the prompt or prompt template.
     */
    name: Schema.String,
    title: /*#__PURE__*/optional(Schema.String),
    /**
     * Arguments to use for templating the prompt.
     */
    arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.String))
  }
}) {}
/**
 * Represents a notification that the server's prompt list changed.
 *
 * **When to use**
 *
 * Use to notify clients that `prompts/list` should be requested again.
 *
 * **Details**
 *
 * Servers may send this notification without a previous client subscription.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class PromptListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/prompts/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
// =============================================================================
// Tools
// =============================================================================
/**
 * Schema for additional properties describing a tool to clients.
 *
 * **Details**
 *
 * NOTE: all properties in ToolAnnotations are **hints**. They are not
 * guaranteed to provide a faithful description of tool behavior (including
 * descriptive properties like `title`).
 *
 * **Gotchas**
 *
 * Clients should never make tool use decisions based on ToolAnnotations
 * received from untrusted servers.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ToolAnnotations extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * A human-readable title for the tool.
   */
  title: /*#__PURE__*/optional(Schema.String),
  /**
   * If true, the tool does not modify its environment.
   *
   * Default: `false`
   */
  readOnlyHint: /*#__PURE__*/optionalWithDefault(Schema.Boolean, constFalse),
  /**
   * If true, the tool may perform destructive updates to its environment.
   * If false, the tool performs only additive updates.
   *
   * (This property is meaningful only when `readOnlyHint == false`)
   *
   * Default: `true`
   */
  destructiveHint: /*#__PURE__*/optionalWithDefault(Schema.Boolean, constTrue),
  /**
   * If true, calling the tool repeatedly with the same arguments
   * will have no additional effect on the its environment.
   *
   * (This property is meaningful only when `readOnlyHint == false`)
   *
   * Default: `false`
   */
  idempotentHint: /*#__PURE__*/optionalWithDefault(Schema.Boolean, constFalse),
  /**
   * If true, this tool may interact with an "open world" of external
   * entities. If false, the tool's domain of interaction is closed.
   * For example, the world of a web search tool is open, whereas that
   * of a memory tool is not.
   *
   * Default: `true`
   */
  openWorldHint: /*#__PURE__*/optionalWithDefault(Schema.Boolean, constTrue)
})) {}
/**
 * Schema for {@link ToolJson}.
 *
 * @stability unstable
 * @category tools
 * @since 4.0.0
 */
export const ToolJson = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("object"),
  properties: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.JsonObject)),
  required: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
}), [Schema.JsonObject]);
/**
 * Schema for {@link ToolOutputJson}.
 *
 * @stability unstable
 * @category tools
 * @since 4.0.0
 */
export const ToolOutputJson = Schema.JsonObject;
/**
 * Schema for the definition of a tool the client can call.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Tool extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/Tool")({
  /**
   * The name of the tool.
   */
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  /**
   * A human-readable description of the tool.
   *
   * This can be used by clients to improve the LLM's understanding of available tools. It can be thought of like a "hint" to the model.
   */
  description: /*#__PURE__*/optional(Schema.String),
  /**
   * A JSON Schema object defining the expected parameters for the tool.
   */
  inputSchema: ToolJson,
  /**
   * An optional JSON Schema object defining the structure of the tool output.
   */
  outputSchema: /*#__PURE__*/optional(ToolOutputJson),
  /**
   * Optional additional tool information.
   */
  annotations: /*#__PURE__*/optional(ToolAnnotations),
  /**
   * Icons that clients can display for this tool.
   */
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon)),
  /**
   * Optional additional metadata for the client.
   *
   * This parameter name is reserved by MCP to allow clients and servers to
   * attach additional metadata to resources.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Schema for the server's response to a tools/list request from the client.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ListToolsResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ListToolsResult")({
  ...PaginatedResultMeta.fields,
  tools: /*#__PURE__*/Schema.Array(Tool)
}) {}
/**
 * Sent from the client to request a list of tools the server has.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ListTools extends /*#__PURE__*/Rpc.make("tools/list", {
  success: ListToolsResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequestMeta)
}) {}
/**
 * Schema for the server's response to a tool call.
 *
 * **Details**
 *
 * Any errors that originate from the tool SHOULD be reported inside the result
 * object, with `isError` set to true, _not_ as an MCP protocol-level error
 * response. Otherwise, the LLM would not be able to see that an error occurred
 * and self-correct. However, any errors in _finding_ the tool, an error
 * indicating that the server does not support tool calls, or any other
 * exceptional conditions, should be reported as an MCP error response.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class CallToolResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/CallToolResult")({
  ...ResultMeta.fields,
  content: /*#__PURE__*/Schema.Array(ContentBlock),
  /**
   * An optional JSON value containing the structured result of the tool call.
   */
  structuredContent: /*#__PURE__*/optional(Schema.Json),
  /**
   * Whether the tool call ended in an error.
   *
   * If not set, this is assumed to be false (the call was successful).
   */
  isError: /*#__PURE__*/optional(Schema.Boolean)
}) {}
/**
 * Represents a client request to invoke a tool provided by the server.
 *
 * **When to use**
 *
 * Use when you need to represent a client request that already knows the tool
 * name and asks the server to execute it with argument values.
 *
 * @see {@link ListTools} for discovering available tools before calling one
 * @see {@link CallToolResult} for the successful tool-call result shape
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class CallTool extends /*#__PURE__*/Rpc.make("tools/call", {
  success: CallToolResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    name: Schema.String,
    arguments: /*#__PURE__*/optionalWithDefault(/*#__PURE__*/Schema.Record(Schema.String, Schema.Any), () => ({}))
  }
}) {}
/**
 * Represents a notification that the server's tool list changed.
 *
 * **When to use**
 *
 * Use to notify clients that `tools/list` should be requested again.
 *
 * **Details**
 *
 * Servers may send this notification without a previous client subscription.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ToolListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/tools/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
// =============================================================================
// Logging
// =============================================================================
/**
 * Schema for log message severity levels, mapped to syslog message severities
 * as specified in RFC 5424 section 6.2.1:
 * https://datatracker.ietf.org/doc/html/rfc5424#section-6.2.1.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export const LoggingLevel = /*#__PURE__*/Schema.Literals(["debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"]);
/**
 * Sent from the client to the server to enable or adjust logging.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export class SetLevel extends /*#__PURE__*/Rpc.make("logging/setLevel", {
  payload: {
    ...RequestMeta.fields,
    /**
     * The level of logging that the client wants to receive from the server.
     * The server should send all logs at this level and higher (i.e., more
     * severe) to the client as notifications/message.
     */
    level: LoggingLevel
  },
  success: /*#__PURE__*/Schema.Struct({}),
  error: McpError
}) {}
/**
 * Sent from the server to the client carrying a log message.
 *
 * **Details**
 *
 * The notification includes the severity level, optional logger name, and
 * JSON-serializable log data.
 *
 * @stability unstable
 * @category logging
 * @since 4.0.0
 */
export class LoggingMessageNotification extends /*#__PURE__*/Rpc.make("notifications/message", {
  payload: /*#__PURE__*/Schema.Struct({
    ...NotificationMeta.fields,
    /**
     * The severity of this log message.
     */
    level: LoggingLevel,
    /**
     * An optional name of the logger issuing this message.
     */
    logger: /*#__PURE__*/optional(Schema.String),
    /**
     * The data to be logged, such as a string message or an object. Any JSON
     * serializable type is allowed here.
     */
    data: Schema.Any
  })
}) {}
// =============================================================================
// Sampling
// =============================================================================
/**
 * Schema for a tool-use request produced during MCP sampling.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export class ToolUseContent extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ToolUseContent")({
  type: /*#__PURE__*/Schema.tag("tool_use"),
  /**
   * Identifier used to associate a later tool result with this request.
   */
  id: Schema.String,
  /**
   * Name of the tool to invoke.
   */
  name: Schema.String,
  /**
   * Arguments supplied to the tool.
   */
  input: /*#__PURE__*/Schema.Record(Schema.String, Schema.Unknown),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Schema for the result of a tool use supplied in a sampling message.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export class ToolResultContent extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ToolResultContent")({
  type: /*#__PURE__*/Schema.tag("tool_result"),
  /**
   * Identifier of the tool-use request that produced this result.
   */
  toolUseId: Schema.String,
  /**
   * Content returned by the tool.
   */
  content: /*#__PURE__*/Schema.Array(ContentBlock),
  /**
   * Optional structured result returned by the tool.
   */
  structuredContent: /*#__PURE__*/optional(Schema.Json),
  /**
   * Whether tool execution ended in an error.
   */
  isError: /*#__PURE__*/optional(Schema.Boolean),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Schema for content blocks accepted in MCP sampling messages.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export const SamplingMessageContentBlock = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent, ToolUseContent, ToolResultContent]);
/**
 * Describes a message issued to or received from an LLM API.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class SamplingMessage extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  role: Role,
  content: /*#__PURE__*/Schema.Union([SamplingMessageContentBlock, /*#__PURE__*/Schema.Array(SamplingMessageContentBlock)]),
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
})) {}
/**
 * Schema for controlling tool selection during MCP sampling.
 *
 * @stability unstable
 * @category sampling
 * @since 4.0.0
 */
export class ToolChoice extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ToolChoice")({
  /**
   * Tool-selection mode requested from the client.
   */
  mode: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["auto", "required", "none"]))
}) {}
/**
 * Schema for model selection hints.
 *
 * **Details**
 *
 * Keys not declared here are currently left unspecified by the spec and are up
 * to the client to interpret.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ModelHint extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  /**
   * A hint for a model name.
   *
   * The client SHOULD treat this as a substring of a model name; for example:
   *  - `claude-3-5-sonnet` should match `claude-3-5-sonnet-20241022`
   *  - `sonnet` should match `claude-3-5-sonnet-20241022`, `claude-3-sonnet-20240229`, etc.
   *  - `claude` should match any Claude model
   *
   * The client MAY also map the string to a different provider's model name or
   * a different model family, as long as it fills a similar niche; for example:
   *  - `gemini-1.5-flash` could match `claude-3-haiku-20240307`
   */
  name: /*#__PURE__*/optional(Schema.String)
})) {}
/**
 * Schema for the server's model selection preferences requested of the client
 * during sampling.
 *
 * **Details**
 *
 * Because LLMs can vary along multiple dimensions, choosing the "best" model is
 * rarely straightforward. Different models excel in different areas, some are
 * faster but less capable, others are more capable but more expensive, and so
 * on. This interface allows servers to express their priorities across multiple
 * dimensions to help clients make an appropriate selection for their use case.
 *
 * **Gotchas**
 *
 * These preferences are always advisory. The client MAY ignore them. It is also
 * up to the client to decide how to interpret these preferences and how to
 * balance them against other considerations.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ModelPreferences extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ModelPreferences")({
  /**
   * Optional hints to use for model selection.
   *
   * If multiple hints are specified, the client MUST evaluate them in order
   * (such that the first match is taken).
   *
   * The client SHOULD prioritize these hints over the numeric priorities, but
   * MAY still use the priorities to select from ambiguous matches.
   */
  hints: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(ModelHint)),
  /**
   * How much to prioritize cost when selecting a model. A value of 0 means cost
   * is not important, while a value of 1 means cost is the most important
   * factor.
   */
  costPriority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  }))),
  /**
   * How much to prioritize sampling speed (latency) when selecting a model. A
   * value of 0 means speed is not important, while a value of 1 means speed is
   * the most important factor.
   */
  speedPriority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  }))),
  /**
   * How much to prioritize intelligence and capabilities when selecting a
   * model. A value of 0 means intelligence is not important, while a value of 1
   * means intelligence is the most important factor.
   */
  intelligencePriority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  })))
}) {}
/**
 * Represents a client response to an MCP sampling request.
 *
 * **When to use**
 *
 * Use to return the message produced by client-side model sampling.
 *
 * **Details**
 *
 * The client should let the user inspect the sampled message before returning
 * it to the server.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class CreateMessageResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/CreateMessageResult")({
  role: Role,
  content: /*#__PURE__*/Schema.Union([SamplingMessageContentBlock, /*#__PURE__*/Schema.Array(SamplingMessageContentBlock)]),
  _meta: /*#__PURE__*/optional(Schema.JsonObject),
  /**
   * The name of the model that generated the message.
   */
  model: Schema.String,
  /**
   * The reason why sampling stopped, if known.
   */
  stopReason: /*#__PURE__*/optional(Schema.String)
}) {}
/**
 * Represents a server request for the client to sample an LLM.
 *
 * **When to use**
 *
 * Use when you need to request model sampling from an MCP client on behalf of a
 * server.
 *
 * **Details**
 *
 * The client chooses the model and should ask the user to approve the sampling
 * request before it begins.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class CreateMessage extends /*#__PURE__*/Rpc.make("sampling/createMessage", {
  success: CreateMessageResult,
  error: McpError,
  payload: {
    messages: /*#__PURE__*/Schema.Array(SamplingMessage),
    /**
     * The server's preferences for which model to select. The client MAY ignore
     * these preferences.
     */
    modelPreferences: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct(ModelPreferences.fields)),
    /**
     * An optional system prompt the server wants to use for sampling. The
     * client MAY modify or omit this prompt.
     */
    systemPrompt: /*#__PURE__*/optional(Schema.String),
    /**
     * A request to include context from one or more MCP servers (including the
     * caller), to be attached to the prompt. The client MAY ignore this request.
     */
    includeContext: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["none", "thisServer", "allServers"])),
    temperature: /*#__PURE__*/optional(Schema.Finite),
    /**
     * The maximum number of tokens to sample, as requested by the server. The
     * client MAY choose to sample fewer tokens than requested.
     */
    maxTokens: Schema.Int,
    stopSequences: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
    /**
     * Optional metadata to pass through to the LLM provider. The format of
     * this metadata is provider-specific.
     */
    metadata: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.Unknown)),
    /**
     * Tools that the model may call while producing the response.
     */
    tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(/*#__PURE__*/Schema.Struct(Tool.fields))),
    /**
     * Controls whether the model may or must call a tool.
     */
    toolChoice: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct(ToolChoice.fields))
  }
}) {}
// =============================================================================
// Autocomplete
// =============================================================================
/**
 * Schema for a reference to a resource or resource template definition.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ResourceReference extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.tag("ref/resource"),
  /**
   * The URI or URI template of the resource.
   */
  uri: Schema.String
})) {}
/**
 * Schema for a prompt reference used in autocomplete requests.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class PromptReference extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.tag("ref/prompt"),
  /**
   * The name of the prompt or prompt template
   */
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String)
})) {}
/**
 * Schema for the server's response to a completion/complete request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class CompleteResult extends /*#__PURE__*/Schema.Opaque()(/*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  completion: /*#__PURE__*/Schema.Struct({
    /**
     * An array of completion values. Must not exceed 100 items.
     */
    values: /*#__PURE__*/Schema.Array(Schema.String),
    /**
     * The total number of completion options available. This can exceed the
     * number of values actually sent in the response.
     */
    total: /*#__PURE__*/optional(Schema.Int),
    /**
     * Indicates whether there are additional completion options beyond those
     * provided in the current response, even if the exact total is unknown.
     */
    hasMore: /*#__PURE__*/optional(Schema.Boolean)
  })
})) {
  /**
   * Empty completion result used when a completion request has no values.
   *
   * @stability unstable
   * @since 4.0.0
   */
  static empty = /*#__PURE__*/CompleteResult.make({
    completion: {
      values: [],
      total: 0,
      hasMore: false
    }
  });
}
/**
 * Sent from the client to the server to ask for completion options.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class Complete extends /*#__PURE__*/Rpc.make("completion/complete", {
  success: CompleteResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.Struct({
    ref: /*#__PURE__*/Schema.Union([PromptReference, ResourceReference]),
    /**
     * The argument's information
     */
    argument: /*#__PURE__*/Schema.Struct({
      /**
       * The name of the argument
       */
      name: Schema.String,
      /**
       * The value of the argument to use for completion matching.
       */
      value: Schema.String
    }),
    /**
     * Additional, optional context for completions
     */
    context: /*#__PURE__*/optionalWithDefault(/*#__PURE__*/Schema.Struct({
      /**
       * Previously-resolved variables in a URI template or prompt.
       */
      arguments: /*#__PURE__*/optionalWithDefault(/*#__PURE__*/Schema.Record(Schema.String, Schema.String), () => ({}))
    }), () => ({
      arguments: {}
    }))
  })
}) {}
// =============================================================================
// Roots
// =============================================================================
/**
 * Represents a root directory or file that the server can operate on.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class Root extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/Root")({
  /**
   * The URI identifying the root. This *must* start with file:// for now.
   * This restriction may be relaxed in future versions of the protocol to allow
   * other URI schemes.
   */
  uri: Schema.String,
  /**
   * An optional name for the root. This can be used to provide a human-readable
   * identifier for the root, which may be useful for display purposes or for
   * referencing the root in other parts of the application.
   */
  name: /*#__PURE__*/optional(Schema.String),
  /**
   * Optional additional metadata associated with the root.
   */
  _meta: /*#__PURE__*/optional(Schema.JsonObject)
}) {}
/**
 * Represents a client response containing the roots available to the server.
 *
 * **When to use**
 *
 * Use to return the directories or files that an MCP server may operate on.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ListRootsResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ListRootsResult")({
  roots: /*#__PURE__*/Schema.Array(Root)
}) {}
/**
 * Sent from the server to request a list of root URIs from the client. Roots
 * allow servers to ask for specific directories or files to operate on. A
 * common example for roots is providing a set of repositories or directories a
 * server should operate on.
 *
 * **Details**
 *
 * This request is typically used when the server needs to understand the file
 * system structure or access specific locations that the client has permission
 * to read from.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ListRoots extends /*#__PURE__*/Rpc.make("roots/list", {
  success: ListRootsResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(RequestMeta)
}) {}
/**
 * Represents a notification that the client's root list changed.
 *
 * **When to use**
 *
 * Use to tell the server that it should request an updated roots list.
 *
 * **Details**
 *
 * Send this when the client adds, removes, or modifies a root.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class RootsListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/roots/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
// =============================================================================
// Elicitation
// =============================================================================
/**
 * Schema for a string field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class ElicitationString extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitationString")({
  type: /*#__PURE__*/Schema.tag("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minLength: /*#__PURE__*/optional(Schema.Int),
  maxLength: /*#__PURE__*/optional(Schema.Int),
  format: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["email", "uri", "date", "date-time"])),
  default: /*#__PURE__*/optional(Schema.String)
}) {}
/**
 * Schema for a numeric field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class ElicitationNumber extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitationNumber")({
  type: /*#__PURE__*/Schema.Literals(["number", "integer"]),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minimum: /*#__PURE__*/optional(Schema.Finite),
  maximum: /*#__PURE__*/optional(Schema.Finite),
  default: /*#__PURE__*/optional(Schema.Finite)
}) {}
/**
 * Schema for a boolean field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class ElicitationBoolean extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitationBoolean")({
  type: /*#__PURE__*/Schema.tag("boolean"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  default: /*#__PURE__*/optional(Schema.Boolean)
}) {}
/**
 * Schema for an untitled single-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class UntitledSingleSelectEnum extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/UntitledSingleSelectEnum")({
  type: /*#__PURE__*/Schema.tag("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  enum: /*#__PURE__*/Schema.Array(Schema.String),
  default: /*#__PURE__*/optional(Schema.String)
}) {}
/**
 * Schema for a titled single-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class TitledSingleSelectEnum extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/TitledSingleSelectEnum")({
  type: /*#__PURE__*/Schema.tag("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  oneOf: /*#__PURE__*/Schema.Array(/*#__PURE__*/Schema.Struct({
    const: Schema.String,
    title: Schema.String
  })),
  default: /*#__PURE__*/optional(Schema.String)
}) {}
/**
 * Schema for every single-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export const SingleSelectEnum = /*#__PURE__*/Schema.Union([UntitledSingleSelectEnum, TitledSingleSelectEnum]);
/**
 * Schema for an untitled multi-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class UntitledMultiSelectEnum extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/UntitledMultiSelectEnum")({
  type: /*#__PURE__*/Schema.tag("array"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minItems: /*#__PURE__*/optional(Schema.Int),
  maxItems: /*#__PURE__*/optional(Schema.Int),
  items: /*#__PURE__*/Schema.Struct({
    type: /*#__PURE__*/Schema.tag("string"),
    enum: /*#__PURE__*/Schema.Array(Schema.String)
  }),
  default: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
}) {}
/**
 * Schema for a titled multi-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class TitledMultiSelectEnum extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/TitledMultiSelectEnum")({
  type: /*#__PURE__*/Schema.tag("array"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minItems: /*#__PURE__*/optional(Schema.Int),
  maxItems: /*#__PURE__*/optional(Schema.Int),
  items: /*#__PURE__*/Schema.Struct({
    anyOf: /*#__PURE__*/Schema.Array(/*#__PURE__*/Schema.Struct({
      const: Schema.String,
      title: Schema.String
    }))
  }),
  default: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
}) {}
/**
 * Schema for every multi-select field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export const MultiSelectEnum = /*#__PURE__*/Schema.Union([UntitledMultiSelectEnum, TitledMultiSelectEnum]);
/**
 * Schema for the legacy titled single-select elicitation field.
 *
 * @deprecated Use {@link TitledSingleSelectEnum} instead.
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class LegacyTitledEnum extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/LegacyTitledEnum")({
  type: /*#__PURE__*/Schema.tag("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  enum: /*#__PURE__*/Schema.Array(Schema.String),
  enumNames: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
  default: /*#__PURE__*/optional(Schema.String)
}) {}
/**
 * Schema for every enumeration field in an MCP elicitation form.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export const ElicitationEnum = /*#__PURE__*/Schema.Union([LegacyTitledEnum, SingleSelectEnum, MultiSelectEnum]);
/**
 * Schema for primitive field definitions accepted by MCP elicitation forms.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export const PrimitiveSchemaDefinition = /*#__PURE__*/Schema.Union([ElicitationEnum, ElicitationString, ElicitationNumber, ElicitationBoolean]);
const ElicitationForm = /*#__PURE__*/Schema.Struct({
  $schema: /*#__PURE__*/optional(Schema.String),
  type: /*#__PURE__*/Schema.tag("object"),
  properties: /*#__PURE__*/Schema.Record(Schema.String, PrimitiveSchemaDefinition),
  required: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * Schema for form-mode MCP elicitation requests.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class ElicitRequestFormParams extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitRequestFormParams")({
  ...RequestMeta.fields,
  mode: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literal("form")),
  message: Schema.String,
  requestedSchema: ElicitationForm
}) {}
/**
 * Schema for URL-mode MCP elicitation requests.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class ElicitRequestURLParams extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitRequestURLParams")({
  ...RequestMeta.fields,
  mode: /*#__PURE__*/Schema.Literal("url"),
  message: Schema.String,
  elicitationId: Schema.String,
  url: Schema.String
}) {}
/**
 * Schema for every MCP elicitation request mode.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export const ElicitRequestParams = /*#__PURE__*/Schema.Union([ElicitRequestFormParams, ElicitRequestURLParams]);
/**
 * Schema for an accepted client response to an elicitation request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ElicitAcceptResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitAcceptResult")({
  ...ResultMeta.fields,
  /**
   * The user action in response to the elicitation.
   * - "accept": User submitted the form/confirmed the action
   * - "decline": User explicitly declined the action
   * - "cancel": User dismissed without making an explicit choice
   */
  action: /*#__PURE__*/Schema.Literal("accept"),
  /**
   * The submitted form data, only present when action is "accept".
   * Contains values matching the requested schema.
   */
  content: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite, Schema.Boolean, /*#__PURE__*/Schema.Array(Schema.String)])))
}) {}
/**
 * Schema for a declined or canceled client response to an elicitation request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ElicitDeclineResult extends /*#__PURE__*/Schema.Class("@effect/ai/McpSchema/ElicitDeclineResult")({
  ...ResultMeta.fields,
  /**
   * The user action in response to the elicitation.
   * - "accept": User submitted the form/confirmed the action
   * - "decline": User explicitly declined the action
   * - "cancel": User dismissed without making an explicit choice
   */
  action: /*#__PURE__*/Schema.Literals(["cancel", "decline"])
}) {}
/**
 * Schema for every client response to an elicitation request.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ElicitResult = /*#__PURE__*/Schema.Union([ElicitAcceptResult, ElicitDeclineResult]);
/**
 * Indicates that an MCP operation requires additional keyed input from the client.
 *
 * **When to use**
 *
 * Use when a handler cannot complete until the client supplies roots, sampling,
 * or elicitation input in a later request.
 *
 * **Details**
 *
 * `requestState` is opaque to the client and is returned unchanged with the
 * keyed input responses.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export class InputRequired extends /*#__PURE__*/Data.TaggedClass("InputRequired") {
  // The overload preserves the requirement that at least one of inputRequests or requestState is present.
  // oxlint-disable-next-line no-useless-constructor
  constructor(fields) {
    super(fields);
  }
}
/**
 * Sent from the server asking the client to collect structured input from the
 * user.
 *
 * **Details**
 *
 * The client responds with accepted content, an explicit decline, or a
 * cancellation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class Elicit extends /*#__PURE__*/Rpc.make("elicitation/create", {
  success: ElicitResult,
  error: McpError,
  payload: ElicitRequestParams
}) {}
/**
 * Notifies a client that a URL-mode elicitation completed.
 *
 * @stability unstable
 * @category elicitation
 * @since 4.0.0
 */
export class ElicitationCompleteNotification extends /*#__PURE__*/Rpc.make("notifications/elicitation/complete", {
  payload: {
    elicitationId: Schema.String
  }
}) {}
/**
 * Error raised when an MCP elicitation request is declined or fails before
 * accepted content is returned.
 *
 * **Details**
 *
 * The error stores the original elicitation request and, when available, the
 * underlying cause.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export class ElicitationDeclined extends /*#__PURE__*/Schema.Error("@effect/ai/McpSchema/ElicitationDeclined")({
  _tag: /*#__PURE__*/Schema.tag("ElicitationDeclined"),
  request: Elicit.payloadSchema,
  cause: /*#__PURE__*/optional(/*#__PURE__*/Schema.Defect())
}) {}
// =============================================================================
// McpServerClient
// =============================================================================
/**
 * Raised when the negotiated MCP revision or client capabilities do not
 * support a server-initiated operation.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class McpReverseOperationUnsupported extends /*#__PURE__*/Data.TaggedError("McpReverseOperationUnsupported") {}
/**
 * A reverse MCP operation failed while being sent or projected through a
 * version adapter.
 *
 * @stability unstable
 * @category errors
 * @since 4.0.0
 */
export class McpReverseOperationError extends /*#__PURE__*/Data.TaggedError("McpReverseOperationError") {}
/**
 * Protocol-neutral context available while handling an MCP request.
 *
 * **Details**
 *
 * Unlike `McpServerClient`, this service does not imply an initialized session
 * or support for server-initiated requests.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class McpRequestContext extends /*#__PURE__*/Context.Service()("effect/ai/McpSchema/McpRequestContext") {}
/**
 * Service available while handling an MCP client request.
 *
 * **Details**
 *
 * It exposes the current client id, normalized initialization data, and a
 * scoped version-neutral facade for server-initiated requests.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class McpServerClient extends /*#__PURE__*/Context.Service()("effect/ai/McpSchema/McpServerClient") {}
/**
 * RPC middleware that provides `McpServerClient` to handlers for initialized
 * MCP clients.
 *
 * @stability unstable
 * @category middleware
 * @since 4.0.0
 */
export class McpServerClientMiddleware extends /*#__PURE__*/RpcMiddleware.Service()("effect/ai/McpSchema/McpServerClientMiddleware") {}
/**
 * RPC group for requests that MCP clients send to the server.
 *
 * **Details**
 *
 * The group includes initialization, resource, prompt, tool, logging,
 * completion, and ping requests, and installs `McpServerClientMiddleware` for
 * handlers.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ClientRequestRpcs extends /*#__PURE__*/RpcGroup.make(Ping, Initialize, Complete, SetLevel, GetPrompt, ListPrompts, ListResources, ListResourceTemplates, ReadResource, Subscribe, Unsubscribe, CallTool, ListTools).middleware(McpServerClientMiddleware) {}
/**
 * RPC group for notifications that MCP clients send to the server, such as
 * cancellation, progress, initialization completion, and roots list changes.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ClientNotificationRpcs extends /*#__PURE__*/RpcGroup.make(CancelledNotification, ProgressNotification, InitializedNotification, RootsListChangedNotification) {}
/**
 * RPC group combining all client-to-server MCP requests and notifications.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ClientRpcs extends /*#__PURE__*/ClientRequestRpcs.merge(ClientNotificationRpcs) {}
/**
 * RPC group for requests that an MCP server can send to a client, including
 * ping, sampling, roots listing, and elicitation.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ServerRequestRpcs extends /*#__PURE__*/RpcGroup.make(Ping, CreateMessage, ListRoots, Elicit) {}
/**
 * RPC group for notifications that an MCP server can send to a client,
 * including cancellation, progress, logging, and list or resource update
 * notifications.
 *
 * @stability unstable
 * @category protocols
 * @since 4.0.0
 */
export class ServerNotificationRpcs extends /*#__PURE__*/RpcGroup.make(CancelledNotification, ProgressNotification, LoggingMessageNotification, ResourceUpdatedNotification, ResourceListChangedNotification, ToolListChangedNotification, PromptListChangedNotification, ElicitationCompleteNotification) {}
const ParamSchemaTypeId = "~effect/ai/McpSchema/ParamSchema";
/**
 * Returns `true` when a schema was created with `param` and therefore carries
 * a resource URI template parameter name.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export function isParam(schema) {
  return Predicate.hasProperty(schema, ParamSchemaTypeId);
}
/**
 * Creates a parameter for a resource URI template.
 *
 * @stability unstable
 * @category parameters
 * @since 4.0.0
 */
export function param(name, schema) {
  return Schema.make(schema.ast, {
    [ParamSchemaTypeId]: ParamSchemaTypeId,
    name,
    schema
  });
}
/**
 * Annotation to conditionally enable or disable tools based on client
 * information.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export class EnabledWhen extends /*#__PURE__*/Context.Service()("effect/ai/McpSchema/EnabledWhen") {}
//# sourceMappingURL=McpSchema.js.map