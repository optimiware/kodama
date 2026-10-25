/**
 * Exact MCP v2025-06-18 wire schemas.
 *
 * This module is independent of the public compatibility-oriented McpSchema
 * surface and is frozen as a dated delta from v2025-03-26.
 *
 * @internal
 */
import * as Rpc from "../../../rpc/Rpc.js";
import * as RpcGroup from "../../../rpc/RpcGroup.js";
import * as Schema from "../../../Schema.js";
import * as Previous from "./v2025_03_26.js";
export * from "./v2025_03_26.js";
/**
 * @internal
 */
export const protocolVersion = "2025-06-18";
const optional = Previous.optional;
const JsonObject = Schema.JsonObject;
const Meta = /*#__PURE__*/optional(JsonObject);
/**
 * @internal
 */
export const Implementation = /*#__PURE__*/Schema.Struct({
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  version: Schema.String
});
/**
 * @internal
 */
export const ClientCapabilities = /*#__PURE__*/Schema.Struct({
  ...Previous.ClientCapabilities.fields,
  elicitation: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({}))
});
/**
 * @internal
 */
export const Annotations = /*#__PURE__*/Schema.Struct({
  audience: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Previous.Role)),
  priority: /*#__PURE__*/optional(/*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
    minimum: 0,
    maximum: 1
  })))
});
/**
 * @internal
 */
export const Resource = /*#__PURE__*/Schema.Struct({
  ...Previous.Resource.fields,
  title: /*#__PURE__*/optional(Schema.String),
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: Meta
});
/**
 * @internal
 */
export const ResourceTemplate = /*#__PURE__*/Schema.Struct({
  ...Previous.ResourceTemplate.fields,
  title: /*#__PURE__*/optional(Schema.String),
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: Meta
});
/**
 * @internal
 */
export const TextResourceContents = /*#__PURE__*/Schema.Struct({
  ...Previous.TextResourceContents.fields,
  _meta: Meta
});
/**
 * @internal
 */
export const BlobResourceContents = /*#__PURE__*/Schema.Struct({
  ...Previous.BlobResourceContents.fields,
  _meta: Meta
});
/**
 * @internal
 */
export const ResourceContents = /*#__PURE__*/Schema.Union([TextResourceContents, BlobResourceContents]);
/**
 * @internal
 */
export const PromptArgument = /*#__PURE__*/Schema.Struct({
  ...Previous.PromptArgument.fields,
  title: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const Prompt = /*#__PURE__*/Schema.Struct({
  ...Previous.Prompt.fields,
  title: /*#__PURE__*/optional(Schema.String),
  arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(PromptArgument)),
  _meta: Meta
});
/**
 * @internal
 */
export const EmbeddedResource = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("resource"),
  resource: ResourceContents,
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: Meta
});
/**
 * @internal
 */
export const ResourceLink = /*#__PURE__*/Schema.Struct({
  ...Resource.fields,
  type: /*#__PURE__*/Schema.Literal("resource_link")
});
/**
 * @internal
 */
export const TextContent = /*#__PURE__*/Schema.Struct({
  ...Previous.TextContent.fields,
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: Meta
});
/**
 * @internal
 */
export const ImageContent = /*#__PURE__*/Schema.Struct({
  ...Previous.ImageContent.fields,
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: Meta
});
/**
 * @internal
 */
export const AudioContent = /*#__PURE__*/Schema.Struct({
  ...Previous.AudioContent.fields,
  annotations: /*#__PURE__*/optional(Annotations),
  _meta: Meta
});
/**
 * @internal
 */
export const ContentBlock = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent, EmbeddedResource, ResourceLink]);
/**
 * @internal
 */
export const SamplingContent = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent]);
/**
 * @internal
 */
export const PromptMessage = /*#__PURE__*/Schema.Struct({
  role: Previous.Role,
  content: ContentBlock
});
/**
 * @internal
 */
export const SamplingMessage = /*#__PURE__*/Schema.Struct({
  role: Previous.Role,
  content: SamplingContent
});
/**
 * @internal
 */
export const ServerCapabilities = /*#__PURE__*/Schema.Struct({
  ...Previous.ServerCapabilities.fields
});
/**
 * @internal
 */
export const InitializeResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  protocolVersion: Schema.String,
  capabilities: ServerCapabilities,
  serverInfo: Implementation,
  instructions: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export class Initialize extends /*#__PURE__*/Rpc.make("initialize", {
  success: InitializeResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    protocolVersion: Schema.String,
    capabilities: ClientCapabilities,
    clientInfo: Implementation
  }
}) {}
const ToolJsonSchema = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("object"),
  properties: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, JsonObject)),
  required: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const Tool = /*#__PURE__*/Schema.Struct({
  name: Schema.String,
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  inputSchema: ToolJsonSchema,
  outputSchema: /*#__PURE__*/optional(ToolJsonSchema),
  annotations: /*#__PURE__*/optional(Previous.ToolAnnotations),
  _meta: Meta
});
/**
 * @internal
 */
export const CallToolResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  content: /*#__PURE__*/Schema.Array(ContentBlock),
  structuredContent: /*#__PURE__*/optional(JsonObject),
  isError: /*#__PURE__*/optional(Schema.Boolean)
});
/**
 * @internal
 */
export const CreateMessageResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  role: Previous.Role,
  content: SamplingContent,
  model: Schema.String,
  stopReason: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export class CreateMessage extends /*#__PURE__*/Rpc.make("sampling/createMessage", {
  success: CreateMessageResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    messages: /*#__PURE__*/Schema.Array(SamplingMessage),
    modelPreferences: /*#__PURE__*/optional(Previous.ModelPreferences),
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
export const PromptReference = /*#__PURE__*/Schema.Struct({
  ...Previous.PromptReference.fields,
  title: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const ResourceTemplateReference = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("ref/resource"),
  uri: Schema.String
});
/**
 * @internal
 */
export const CompleteResult = Previous.CompleteResult;
/**
 * @internal
 */
export class Complete extends /*#__PURE__*/Rpc.make("completion/complete", {
  success: CompleteResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    ref: /*#__PURE__*/Schema.Union([PromptReference, ResourceTemplateReference]),
    argument: /*#__PURE__*/Schema.Struct({
      name: Schema.String,
      value: Schema.String
    }),
    context: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
      arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.String))
    }))
  }
}) {}
/**
 * @internal
 */
export const ListResourcesResult = /*#__PURE__*/Schema.Struct({
  ...Previous.PaginatedResult.fields,
  resources: /*#__PURE__*/Schema.Array(Resource)
});
/**
 * @internal
 */
export const ListResourceTemplatesResult = /*#__PURE__*/Schema.Struct({
  ...Previous.PaginatedResult.fields,
  resourceTemplates: /*#__PURE__*/Schema.Array(ResourceTemplate)
});
/**
 * @internal
 */
export const ReadResourceResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  contents: /*#__PURE__*/Schema.Array(ResourceContents)
});
/**
 * @internal
 */
export const ListPromptsResult = /*#__PURE__*/Schema.Struct({
  ...Previous.PaginatedResult.fields,
  prompts: /*#__PURE__*/Schema.Array(Prompt)
});
/**
 * @internal
 */
export const GetPromptResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  description: /*#__PURE__*/optional(Schema.String),
  messages: /*#__PURE__*/Schema.Array(PromptMessage)
});
/**
 * @internal
 */
export const ListToolsResult = /*#__PURE__*/Schema.Struct({
  ...Previous.PaginatedResult.fields,
  tools: /*#__PURE__*/Schema.Array(Tool)
});
/**
 * @internal
 */
export class ListResources extends /*#__PURE__*/Rpc.make("resources/list", {
  success: ListResourcesResult,
  error: Previous.McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(Previous.PaginatedRequest)
}) {}
/**
 * @internal
 */
export class ListResourceTemplates extends /*#__PURE__*/Rpc.make("resources/templates/list", {
  success: ListResourceTemplatesResult,
  error: Previous.McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(Previous.PaginatedRequest)
}) {}
/**
 * @internal
 */
export class ReadResource extends /*#__PURE__*/Rpc.make("resources/read", {
  success: ReadResourceResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export class ListPrompts extends /*#__PURE__*/Rpc.make("prompts/list", {
  success: ListPromptsResult,
  error: Previous.McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(Previous.PaginatedRequest)
}) {}
/**
 * @internal
 */
export class GetPrompt extends /*#__PURE__*/Rpc.make("prompts/get", {
  success: GetPromptResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    name: Schema.String,
    arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.String))
  }
}) {}
/**
 * @internal
 */
export class ListTools extends /*#__PURE__*/Rpc.make("tools/list", {
  success: ListToolsResult,
  error: Previous.McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(Previous.PaginatedRequest)
}) {}
/**
 * @internal
 */
export class CallTool extends /*#__PURE__*/Rpc.make("tools/call", {
  success: CallToolResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    name: Schema.String,
    arguments: /*#__PURE__*/optional(JsonObject)
  }
}) {}
/**
 * @internal
 */
export const ElicitResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  action: /*#__PURE__*/Schema.Literals(["accept", "decline", "cancel"]),
  content: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite, Schema.Boolean])))
});
const StringSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minLength: /*#__PURE__*/optional(Schema.Int),
  maxLength: /*#__PURE__*/optional(Schema.Int),
  format: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["email", "uri", "date", "date-time"]))
});
const NumberSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literals(["number", "integer"]),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minimum: /*#__PURE__*/optional(Schema.Finite),
  maximum: /*#__PURE__*/optional(Schema.Finite)
});
const BooleanSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("boolean"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  default: /*#__PURE__*/optional(Schema.Boolean)
});
const EnumSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  enum: /*#__PURE__*/Schema.Array(Schema.String),
  enumNames: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
const RequestedSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("object"),
  properties: /*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Union([StringSchema, NumberSchema, BooleanSchema, EnumSchema])),
  required: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * @internal
 */
export class Elicit extends /*#__PURE__*/Rpc.make("elicitation/create", {
  success: ElicitResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    message: Schema.String,
    requestedSchema: RequestedSchema
  }
}) {}
/**
 * @internal
 */
export class ClientRequestRpcs extends /*#__PURE__*/RpcGroup.make(Previous.Ping, Initialize, Complete, Previous.SetLevel, GetPrompt, ListPrompts, ListResources, ListResourceTemplates, ReadResource, Previous.Subscribe, Previous.Unsubscribe, CallTool, ListTools) {}
/**
 * @internal
 */
export class ClientNotificationRpcs extends /*#__PURE__*/RpcGroup.make(Previous.CancelledNotification, Previous.ProgressNotification, Previous.InitializedNotification, Previous.RootsListChangedNotification) {}
/**
 * @internal
 */
export class ServerRequestRpcs extends /*#__PURE__*/RpcGroup.make(Previous.Ping, CreateMessage, Previous.ListRoots, Elicit) {}
/**
 * @internal
 */
export class ServerNotificationRpcs extends /*#__PURE__*/RpcGroup.make(Previous.CancelledNotification, Previous.ProgressNotification, Previous.LoggingMessageNotification, Previous.ResourceUpdatedNotification, Previous.ResourceListChangedNotification, Previous.ToolListChangedNotification, Previous.PromptListChangedNotification) {}
//# sourceMappingURL=v2025_06_18.js.map