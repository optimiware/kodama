/**
 * Exact MCP v2024-11-05 wire schemas.
 *
 * Transport topology is intentionally not represented here. This module owns
 * the dated JSON-RPC method payloads and results only.
 *
 * @internal
 */
import * as Option from "../../../Option.js";
import * as Rpc from "../../../rpc/Rpc.js";
import * as RpcGroup from "../../../rpc/RpcGroup.js";
import * as Schema from "../../../Schema.js";
import * as SchemaGetter from "../../../SchemaGetter.js";
/**
 * @internal
 */
export const protocolVersion = "2024-11-05";
/**
 * @internal
 */
export const optional = schema => Schema.optionalKey(schema).pipe(Schema.decodeTo(Schema.toType(Schema.optional(schema)), {
  decode: SchemaGetter.passthrough(),
  encode: SchemaGetter.transformOptional(Option.flatMap(Option.fromUndefinedOr))
}));
const JsonObject = Schema.JsonObject;
/**
 * @internal
 */
export const RequestId = /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite]);
/**
 * @internal
 */
export const ProgressToken = /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite]);
/**
 * @internal
 */
export const Role = /*#__PURE__*/Schema.Literals(["user", "assistant"]);
/**
 * @internal
 */
export const LoggingLevel = /*#__PURE__*/Schema.Literals(["debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"]);
/**
 * @internal
 */
export const RequestMeta = /*#__PURE__*/Schema.Struct({
  _meta: /*#__PURE__*/optional(/*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
    progressToken: /*#__PURE__*/optional(ProgressToken)
  }), [Schema.JsonObject]))
});
/**
 * @internal
 */
export const NotificationMeta = /*#__PURE__*/Schema.Struct({
  _meta: /*#__PURE__*/optional(JsonObject)
});
/**
 * @internal
 */
export const ResultMeta = /*#__PURE__*/Schema.Struct({
  _meta: /*#__PURE__*/optional(JsonObject)
});
/**
 * @internal
 */
export const PaginatedRequest = /*#__PURE__*/Schema.Struct({
  ...RequestMeta.fields,
  cursor: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const PaginatedResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  nextCursor: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const Implementation = /*#__PURE__*/Schema.Struct({
  name: Schema.String,
  version: Schema.String
});
/**
 * @internal
 */
export const ClientCapabilities = /*#__PURE__*/Schema.Struct({
  extensions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(/*#__PURE__*/Schema.TemplateLiteral([Schema.String, "/", Schema.String]), Schema.Json)),
  experimental: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, JsonObject)),
  roots: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  sampling: /*#__PURE__*/optional(JsonObject)
});
/**
 * @internal
 */
export const ServerCapabilities = /*#__PURE__*/Schema.Struct({
  extensions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(/*#__PURE__*/Schema.TemplateLiteral([Schema.String, "/", Schema.String]), Schema.Json)),
  experimental: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, JsonObject)),
  logging: /*#__PURE__*/optional(JsonObject),
  prompts: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  resources: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    subscribe: /*#__PURE__*/optional(Schema.Boolean),
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  }))
});
/**
 * @internal
 */
export const McpError = /*#__PURE__*/Schema.Struct({
  code: Schema.Int,
  message: Schema.String,
  data: /*#__PURE__*/optional(Schema.Any)
});
/**
 * @internal
 */
export const Annotation = /*#__PURE__*/Schema.Struct({
  audience: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Role)),
  priority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  })))
});
/**
 * @internal
 */
export const TextResourceContents = /*#__PURE__*/Schema.Struct({
  uri: Schema.String,
  mimeType: /*#__PURE__*/optional(Schema.String),
  text: Schema.String
});
/**
 * @internal
 */
export const BlobResourceContents = /*#__PURE__*/Schema.Struct({
  uri: Schema.String,
  mimeType: /*#__PURE__*/optional(Schema.String),
  blob: Schema.String
});
/**
 * @internal
 */
export const ResourceContents = /*#__PURE__*/Schema.Union([TextResourceContents, BlobResourceContents]);
/**
 * @internal
 */
export const TextContent = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("text"),
  text: Schema.String,
  annotations: /*#__PURE__*/optional(Annotation)
});
/**
 * @internal
 */
export const ImageContent = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("image"),
  data: Schema.String,
  mimeType: Schema.String,
  annotations: /*#__PURE__*/optional(Annotation)
});
/**
 * @internal
 */
export const EmbeddedResource = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("resource"),
  resource: ResourceContents,
  annotations: /*#__PURE__*/optional(Annotation)
});
/**
 * @internal
 */
export const PromptOrToolContent = /*#__PURE__*/Schema.Union([TextContent, ImageContent, EmbeddedResource]);
/**
 * @internal
 */
export const SamplingContent = /*#__PURE__*/Schema.Union([TextContent, ImageContent]);
/**
 * @internal
 */
export const Resource = /*#__PURE__*/Schema.Struct({
  uri: Schema.String,
  name: Schema.String,
  description: /*#__PURE__*/optional(Schema.String),
  mimeType: /*#__PURE__*/optional(Schema.String),
  size: /*#__PURE__*/optional(Schema.Finite),
  annotations: /*#__PURE__*/optional(Annotation)
});
/**
 * @internal
 */
export const ResourceTemplate = /*#__PURE__*/Schema.Struct({
  uriTemplate: Schema.String,
  name: Schema.String,
  description: /*#__PURE__*/optional(Schema.String),
  mimeType: /*#__PURE__*/optional(Schema.String),
  annotations: /*#__PURE__*/optional(Annotation)
});
/**
 * @internal
 */
export const PromptArgument = /*#__PURE__*/Schema.Struct({
  name: Schema.String,
  description: /*#__PURE__*/optional(Schema.String),
  required: /*#__PURE__*/optional(Schema.Boolean)
});
/**
 * @internal
 */
export const Prompt = /*#__PURE__*/Schema.Struct({
  name: Schema.String,
  description: /*#__PURE__*/optional(Schema.String),
  arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(PromptArgument))
});
/**
 * @internal
 */
export const PromptMessage = /*#__PURE__*/Schema.Struct({
  role: Role,
  content: PromptOrToolContent
});
/**
 * @internal
 */
export const ToolInputSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("object"),
  properties: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, JsonObject)),
  required: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * @internal
 */
export const Tool = /*#__PURE__*/Schema.Struct({
  name: Schema.String,
  description: /*#__PURE__*/optional(Schema.String),
  inputSchema: ToolInputSchema
});
/**
 * @internal
 */
export const ModelHint = /*#__PURE__*/Schema.Struct({
  name: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const ModelPreferences = /*#__PURE__*/Schema.Struct({
  hints: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(ModelHint)),
  costPriority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  }))),
  speedPriority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  }))),
  intelligencePriority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  })))
});
/**
 * @internal
 */
export const SamplingMessage = /*#__PURE__*/Schema.Struct({
  role: Role,
  content: SamplingContent
});
/**
 * @internal
 */
export const ResourceReference = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("ref/resource"),
  uri: Schema.String
});
/**
 * @internal
 */
export const PromptReference = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("ref/prompt"),
  name: Schema.String
});
/**
 * @internal
 */
export const Root = /*#__PURE__*/Schema.Struct({
  uri: Schema.String,
  name: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const InitializeResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  protocolVersion: Schema.String,
  capabilities: ServerCapabilities,
  serverInfo: Implementation,
  instructions: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const ListResourcesResult = /*#__PURE__*/Schema.Struct({
  ...PaginatedResult.fields,
  resources: /*#__PURE__*/Schema.Array(Resource)
});
/**
 * @internal
 */
export const ListResourceTemplatesResult = /*#__PURE__*/Schema.Struct({
  ...PaginatedResult.fields,
  resourceTemplates: /*#__PURE__*/Schema.Array(ResourceTemplate)
});
/**
 * @internal
 */
export const ReadResourceResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  contents: /*#__PURE__*/Schema.Array(ResourceContents)
});
/**
 * @internal
 */
export const ListPromptsResult = /*#__PURE__*/Schema.Struct({
  ...PaginatedResult.fields,
  prompts: /*#__PURE__*/Schema.Array(Prompt)
});
/**
 * @internal
 */
export const GetPromptResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  description: /*#__PURE__*/optional(Schema.String),
  messages: /*#__PURE__*/Schema.Array(PromptMessage)
});
/**
 * @internal
 */
export const ListToolsResult = /*#__PURE__*/Schema.Struct({
  ...PaginatedResult.fields,
  tools: /*#__PURE__*/Schema.Array(Tool)
});
/**
 * @internal
 */
export const CallToolResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  content: /*#__PURE__*/Schema.Array(PromptOrToolContent),
  isError: /*#__PURE__*/optional(Schema.Boolean)
});
/**
 * @internal
 */
export const CreateMessageResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  role: Role,
  content: SamplingContent,
  model: Schema.String,
  stopReason: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const CompleteResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  completion: /*#__PURE__*/Schema.Struct({
    values: /*#__PURE__*/Schema.Array(Schema.String),
    total: /*#__PURE__*/optional(Schema.Finite),
    hasMore: /*#__PURE__*/optional(Schema.Boolean)
  })
});
/**
 * @internal
 */
export const ListRootsResult = /*#__PURE__*/Schema.Struct({
  ...ResultMeta.fields,
  roots: /*#__PURE__*/Schema.Array(Root)
});
/**
 * @internal
 */
export class Ping extends /*#__PURE__*/Rpc.make("ping", {
  success: ResultMeta,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(RequestMeta)
}) {}
/**
 * @internal
 */
export class Initialize extends /*#__PURE__*/Rpc.make("initialize", {
  success: InitializeResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    protocolVersion: Schema.String,
    capabilities: ClientCapabilities,
    clientInfo: Implementation
  }
}) {}
/**
 * @internal
 */
export class Complete extends /*#__PURE__*/Rpc.make("completion/complete", {
  success: CompleteResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    ref: /*#__PURE__*/Schema.Union([PromptReference, ResourceReference]),
    argument: /*#__PURE__*/Schema.Struct({
      name: Schema.String,
      value: Schema.String
    })
  }
}) {}
/**
 * @internal
 */
export class SetLevel extends /*#__PURE__*/Rpc.make("logging/setLevel", {
  success: ResultMeta,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    level: LoggingLevel
  }
}) {}
/**
 * @internal
 */
export class GetPrompt extends /*#__PURE__*/Rpc.make("prompts/get", {
  success: GetPromptResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    name: Schema.String,
    arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.String))
  }
}) {}
/**
 * @internal
 */
export class ListPrompts extends /*#__PURE__*/Rpc.make("prompts/list", {
  success: ListPromptsResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequest)
}) {}
/**
 * @internal
 */
export class ListResources extends /*#__PURE__*/Rpc.make("resources/list", {
  success: ListResourcesResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequest)
}) {}
/**
 * @internal
 */
export class ListResourceTemplates extends /*#__PURE__*/Rpc.make("resources/templates/list", {
  success: ListResourceTemplatesResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequest)
}) {}
/**
 * @internal
 */
export class ReadResource extends /*#__PURE__*/Rpc.make("resources/read", {
  success: ReadResourceResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export class Subscribe extends /*#__PURE__*/Rpc.make("resources/subscribe", {
  success: ResultMeta,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export class Unsubscribe extends /*#__PURE__*/Rpc.make("resources/unsubscribe", {
  success: ResultMeta,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export class CallTool extends /*#__PURE__*/Rpc.make("tools/call", {
  success: CallToolResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    name: Schema.String,
    arguments: /*#__PURE__*/optional(JsonObject)
  }
}) {}
/**
 * @internal
 */
export class ListTools extends /*#__PURE__*/Rpc.make("tools/list", {
  success: ListToolsResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(PaginatedRequest)
}) {}
/**
 * @internal
 */
export class CreateMessage extends /*#__PURE__*/Rpc.make("sampling/createMessage", {
  success: CreateMessageResult,
  error: McpError,
  payload: {
    ...RequestMeta.fields,
    messages: /*#__PURE__*/Schema.Array(SamplingMessage),
    modelPreferences: /*#__PURE__*/optional(ModelPreferences),
    systemPrompt: /*#__PURE__*/optional(Schema.String),
    includeContext: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["none", "thisServer", "allServers"])),
    temperature: /*#__PURE__*/optional(Schema.Finite),
    maxTokens: Schema.Finite,
    stopSequences: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
    metadata: /*#__PURE__*/optional(JsonObject)
  }
}) {}
/**
 * @internal
 */
export class ListRoots extends /*#__PURE__*/Rpc.make("roots/list", {
  success: ListRootsResult,
  error: McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(RequestMeta)
}) {}
/**
 * @internal
 */
export class CancelledNotification extends /*#__PURE__*/Rpc.make("notifications/cancelled", {
  payload: {
    ...NotificationMeta.fields,
    requestId: RequestId,
    reason: /*#__PURE__*/optional(Schema.String)
  }
}) {}
/**
 * @internal
 */
export class ProgressNotification extends /*#__PURE__*/Rpc.make("notifications/progress", {
  payload: {
    ...NotificationMeta.fields,
    progressToken: ProgressToken,
    progress: Schema.Finite,
    total: /*#__PURE__*/optional(Schema.Finite)
  }
}) {}
/**
 * @internal
 */
export class InitializedNotification extends /*#__PURE__*/Rpc.make("notifications/initialized", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
/**
 * @internal
 */
export class RootsListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/roots/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
/**
 * @internal
 */
export class LoggingMessageNotification extends /*#__PURE__*/Rpc.make("notifications/message", {
  payload: {
    ...NotificationMeta.fields,
    level: LoggingLevel,
    logger: /*#__PURE__*/optional(Schema.String),
    data: Schema.Any
  }
}) {}
/**
 * @internal
 */
export class ResourceUpdatedNotification extends /*#__PURE__*/Rpc.make("notifications/resources/updated", {
  payload: {
    ...NotificationMeta.fields,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export class ResourceListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/resources/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
/**
 * @internal
 */
export class ToolListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/tools/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
/**
 * @internal
 */
export class PromptListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/prompts/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationMeta)
}) {}
/**
 * @internal
 */
export class ClientRequestRpcs extends /*#__PURE__*/RpcGroup.make(Ping, Initialize, Complete, SetLevel, GetPrompt, ListPrompts, ListResources, ListResourceTemplates, ReadResource, Subscribe, Unsubscribe, CallTool, ListTools) {}
/**
 * @internal
 */
export class ClientNotificationRpcs extends /*#__PURE__*/RpcGroup.make(CancelledNotification, ProgressNotification, InitializedNotification, RootsListChangedNotification) {}
/**
 * @internal
 */
export class ServerRequestRpcs extends /*#__PURE__*/RpcGroup.make(Ping, CreateMessage, ListRoots) {}
/**
 * @internal
 */
export class ServerNotificationRpcs extends /*#__PURE__*/RpcGroup.make(CancelledNotification, ProgressNotification, LoggingMessageNotification, ResourceUpdatedNotification, ResourceListChangedNotification, ToolListChangedNotification, PromptListChangedNotification) {}
//# sourceMappingURL=v2024_11_05.js.map