/**
 * Supported non-Task MCP v2025-11-25 wire schemas.
 *
 * This module is a dated delta from v2025-06-18. Experimental Tasks are not
 * part of the supported vocabulary for this adapter.
 *
 * @internal
 */
import * as Rpc from "../../../rpc/Rpc.js";
import * as RpcGroup from "../../../rpc/RpcGroup.js";
import * as Schema from "../../../Schema.js";
import * as Previous from "./v2025_06_18.js";
export * from "./v2025_06_18.js";
/**
 * @internal
 */
export const protocolVersion = "2025-11-25";
const optional = Previous.optional;
const JsonObject = Schema.JsonObject;
const Meta = /*#__PURE__*/optional(JsonObject);
/**
 * @internal
 */
export const Icon = /*#__PURE__*/Schema.Struct({
  src: Schema.String,
  mimeType: /*#__PURE__*/optional(Schema.String),
  sizes: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
  theme: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["light", "dark"]))
});
/**
 * @internal
 */
export const Implementation = /*#__PURE__*/Schema.Struct({
  ...Previous.Implementation.fields,
  description: /*#__PURE__*/optional(Schema.String),
  websiteUrl: /*#__PURE__*/optional(Schema.String),
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon))
});
/**
 * @internal
 */
export const ClientCapabilities = /*#__PURE__*/Schema.Struct({
  ...Previous.ClientCapabilities.fields,
  sampling: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    context: /*#__PURE__*/optional(JsonObject),
    tools: /*#__PURE__*/optional(JsonObject)
  })),
  elicitation: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    form: /*#__PURE__*/optional(JsonObject),
    url: /*#__PURE__*/optional(JsonObject)
  }))
});
/**
 * @internal
 */
export const Annotations = /*#__PURE__*/Schema.Struct({
  ...Previous.Annotations.fields,
  lastModified: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const Resource = /*#__PURE__*/Schema.Struct({
  ...Previous.Resource.fields,
  annotations: /*#__PURE__*/optional(Annotations),
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon))
});
/**
 * @internal
 */
export const ResourceTemplate = /*#__PURE__*/Schema.Struct({
  ...Previous.ResourceTemplate.fields,
  annotations: /*#__PURE__*/optional(Annotations),
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon))
});
/**
 * @internal
 */
export const Prompt = /*#__PURE__*/Schema.Struct({
  ...Previous.Prompt.fields,
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon))
});
/**
 * @internal
 */
export const TextContent = /*#__PURE__*/Schema.Struct({
  ...Previous.TextContent.fields,
  annotations: /*#__PURE__*/optional(Annotations)
});
/**
 * @internal
 */
export const ImageContent = /*#__PURE__*/Schema.Struct({
  ...Previous.ImageContent.fields,
  annotations: /*#__PURE__*/optional(Annotations)
});
/**
 * @internal
 */
export const AudioContent = /*#__PURE__*/Schema.Struct({
  ...Previous.AudioContent.fields,
  annotations: /*#__PURE__*/optional(Annotations)
});
/**
 * @internal
 */
export const EmbeddedResource = /*#__PURE__*/Schema.Struct({
  ...Previous.EmbeddedResource.fields,
  annotations: /*#__PURE__*/optional(Annotations)
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
export const ContentBlock = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent, ResourceLink, EmbeddedResource]);
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
export const Tool = /*#__PURE__*/Schema.Struct({
  ...Previous.Tool.fields,
  icons: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Icon))
});
/**
 * @internal
 */
export const CallToolResult = /*#__PURE__*/Schema.Struct({
  ...Previous.CallToolResult.fields,
  content: /*#__PURE__*/Schema.Array(ContentBlock)
});
/**
 * @internal
 */
export class CallTool extends /*#__PURE__*/Rpc.make("tools/call", {
  success: CallToolResult,
  error: Previous.McpError,
  payload: Previous.CallTool.payloadSchema
}) {}
/**
 * @internal
 */
export const ToolUseContent = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("tool_use"),
  id: Schema.String,
  name: Schema.String,
  input: JsonObject,
  _meta: Meta
});
/**
 * @internal
 */
export const ToolResultContent = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("tool_result"),
  toolUseId: Schema.String,
  content: /*#__PURE__*/Schema.Array(ContentBlock),
  structuredContent: /*#__PURE__*/optional(JsonObject),
  isError: /*#__PURE__*/optional(Schema.Boolean),
  _meta: Meta
});
/**
 * @internal
 */
export const SamplingMessageContentBlock = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent, ToolUseContent, ToolResultContent]);
/**
 * @internal
 */
export const SamplingMessage = /*#__PURE__*/Schema.Struct({
  role: Previous.Role,
  content: /*#__PURE__*/Schema.Union([SamplingMessageContentBlock, /*#__PURE__*/Schema.Array(SamplingMessageContentBlock)]),
  _meta: Meta
});
/**
 * @internal
 */
export const ToolChoice = /*#__PURE__*/Schema.Struct({
  mode: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["auto", "required", "none"]))
});
/**
 * @internal
 */
export const CreateMessageResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  ...SamplingMessage.fields,
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
    ...Previous.CreateMessage.payloadSchema.fields,
    messages: /*#__PURE__*/Schema.Array(SamplingMessage),
    maxTokens: Schema.Int,
    tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Tool)),
    toolChoice: /*#__PURE__*/optional(ToolChoice)
  }
}) {}
/**
 * @internal
 */
export const Root = /*#__PURE__*/Schema.Struct({
  ...Previous.Root.fields,
  _meta: Meta
});
/**
 * @internal
 */
export const ListRootsResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  roots: /*#__PURE__*/Schema.Array(Root)
});
/**
 * @internal
 */
export class ListRoots extends /*#__PURE__*/Rpc.make("roots/list", {
  success: ListRootsResult,
  error: Previous.McpError,
  payload: /*#__PURE__*/Schema.UndefinedOr(Previous.RequestMeta)
}) {}
/**
 * @internal
 */
export const StringSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minLength: /*#__PURE__*/optional(Schema.Int),
  maxLength: /*#__PURE__*/optional(Schema.Int),
  format: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["email", "uri", "date", "date-time"])),
  default: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const NumberSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literals(["number", "integer"]),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minimum: /*#__PURE__*/optional(Schema.Finite),
  maximum: /*#__PURE__*/optional(Schema.Finite),
  default: /*#__PURE__*/optional(Schema.Finite)
});
/**
 * @internal
 */
export const BooleanSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("boolean"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  default: /*#__PURE__*/optional(Schema.Boolean)
});
const EnumOption = /*#__PURE__*/Schema.Struct({
  const: Schema.String,
  title: Schema.String
});
/**
 * @internal
 */
export const UntitledSingleSelectEnumSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  enum: /*#__PURE__*/Schema.Array(Schema.String),
  default: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const TitledSingleSelectEnumSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  oneOf: /*#__PURE__*/Schema.Array(EnumOption),
  default: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const SingleSelectEnumSchema = /*#__PURE__*/Schema.Union([UntitledSingleSelectEnumSchema, TitledSingleSelectEnumSchema]);
/**
 * @internal
 */
export const UntitledMultiSelectEnumSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("array"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minItems: /*#__PURE__*/optional(Schema.Int),
  maxItems: /*#__PURE__*/optional(Schema.Int),
  items: /*#__PURE__*/Schema.Struct({
    type: /*#__PURE__*/Schema.Literal("string"),
    enum: /*#__PURE__*/Schema.Array(Schema.String)
  }),
  default: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * @internal
 */
export const TitledMultiSelectEnumSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("array"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  minItems: /*#__PURE__*/optional(Schema.Int),
  maxItems: /*#__PURE__*/optional(Schema.Int),
  items: /*#__PURE__*/Schema.Struct({
    anyOf: /*#__PURE__*/Schema.Array(EnumOption)
  }),
  default: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * @internal
 */
export const MultiSelectEnumSchema = /*#__PURE__*/Schema.Union([UntitledMultiSelectEnumSchema, TitledMultiSelectEnumSchema]);
/**
 * @internal
 */
export const LegacyTitledEnumSchema = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("string"),
  title: /*#__PURE__*/optional(Schema.String),
  description: /*#__PURE__*/optional(Schema.String),
  enum: /*#__PURE__*/Schema.Array(Schema.String),
  enumNames: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
  default: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const EnumSchema = /*#__PURE__*/Schema.Union([LegacyTitledEnumSchema, SingleSelectEnumSchema, MultiSelectEnumSchema]);
/**
 * @internal
 */
export const PrimitiveSchemaDefinition = /*#__PURE__*/Schema.Union([EnumSchema, StringSchema, NumberSchema, BooleanSchema]);
/**
 * @internal
 */
export const RequestedSchema = /*#__PURE__*/Schema.Struct({
  $schema: /*#__PURE__*/optional(Schema.String),
  type: /*#__PURE__*/Schema.Literal("object"),
  properties: /*#__PURE__*/Schema.Record(Schema.String, PrimitiveSchemaDefinition),
  required: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * @internal
 */
export const ElicitRequestFormParams = /*#__PURE__*/Schema.Struct({
  ...Previous.RequestMeta.fields,
  mode: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literal("form")),
  message: Schema.String,
  requestedSchema: RequestedSchema
});
/**
 * @internal
 */
export const ElicitRequestURLParams = /*#__PURE__*/Schema.Struct({
  ...Previous.RequestMeta.fields,
  mode: /*#__PURE__*/Schema.Literal("url"),
  message: Schema.String,
  elicitationId: Schema.String,
  url: Schema.String
});
/**
 * @internal
 */
export const ElicitRequestParams = /*#__PURE__*/Schema.Union([ElicitRequestFormParams, ElicitRequestURLParams]);
/**
 * @internal
 */
export const ElicitResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  action: /*#__PURE__*/Schema.Literals(["accept", "decline", "cancel"]),
  content: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite, Schema.Boolean, /*#__PURE__*/Schema.Array(Schema.String)])))
});
/**
 * @internal
 */
export class Elicit extends /*#__PURE__*/Rpc.make("elicitation/create", {
  success: ElicitResult,
  error: Previous.McpError,
  payload: ElicitRequestParams
}) {}
/**
 * @internal
 */
export class ElicitationCompleteNotification extends /*#__PURE__*/Rpc.make("notifications/elicitation/complete", {
  payload: {
    elicitationId: Schema.String
  }
}) {}
/**
 * @internal
 */
export const InitializeResult = /*#__PURE__*/Schema.Struct({
  ...Previous.InitializeResult.fields,
  serverInfo: Implementation
});
/**
 * @internal
 */
export class Initialize extends /*#__PURE__*/Rpc.make("initialize", {
  success: InitializeResult,
  error: Previous.McpError,
  payload: {
    ...Previous.Initialize.payloadSchema.fields,
    capabilities: ClientCapabilities,
    clientInfo: Implementation
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
export const ListPromptsResult = /*#__PURE__*/Schema.Struct({
  ...Previous.PaginatedResult.fields,
  prompts: /*#__PURE__*/Schema.Array(Prompt)
});
/**
 * @internal
 */
export const GetPromptResult = /*#__PURE__*/Schema.Struct({
  ...Previous.GetPromptResult.fields,
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
export class ClientRequestRpcs extends /*#__PURE__*/RpcGroup.make(Previous.Ping, Initialize, Previous.Complete, Previous.SetLevel, GetPrompt, ListPrompts, ListResources, ListResourceTemplates, Previous.ReadResource, Previous.Subscribe, Previous.Unsubscribe, CallTool, ListTools) {}
/**
 * @internal
 */
export class ClientNotificationRpcs extends /*#__PURE__*/RpcGroup.make(Previous.CancelledNotification, Previous.ProgressNotification, Previous.InitializedNotification, Previous.RootsListChangedNotification) {}
/**
 * @internal
 */
export class ServerRequestRpcs extends /*#__PURE__*/RpcGroup.make(Previous.Ping, CreateMessage, ListRoots, Elicit) {}
/**
 * @internal
 */
export class ServerNotificationRpcs extends /*#__PURE__*/RpcGroup.make(Previous.CancelledNotification, Previous.ProgressNotification, Previous.LoggingMessageNotification, Previous.ResourceUpdatedNotification, Previous.ResourceListChangedNotification, Previous.ToolListChangedNotification, Previous.PromptListChangedNotification, ElicitationCompleteNotification) {}
//# sourceMappingURL=v2025_11_25.js.map