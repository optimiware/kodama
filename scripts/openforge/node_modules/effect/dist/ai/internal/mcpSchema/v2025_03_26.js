/**
 * Exact MCP v2025-03-26 wire schemas.
 *
 * This revision is expressed as a frozen delta from the exact 2024-11-05
 * schemas. Transport envelopes (including JSON-RPC batches) are owned by the
 * transport codec rather than Effect RPC payload schemas.
 *
 * @internal
 */
import * as Rpc from "../../../rpc/Rpc.js";
import * as RpcGroup from "../../../rpc/RpcGroup.js";
import * as Schema from "../../../Schema.js";
import * as Previous from "./v2024_11_05.js";
export * from "./v2024_11_05.js";
/**
 * @internal
 */
export const protocolVersion = "2025-03-26";
const optional = Previous.optional;
/**
 * @internal
 */
export const ServerCapabilities = /*#__PURE__*/Schema.Struct({
  ...Previous.ServerCapabilities.fields,
  completions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({}))
});
/**
 * @internal
 */
export const AudioContent = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("audio"),
  data: Schema.String,
  mimeType: Schema.String,
  annotations: /*#__PURE__*/optional(Previous.Annotation)
});
/**
 * @internal
 */
export const PromptOrToolContent = /*#__PURE__*/Schema.Union([Previous.TextContent, Previous.ImageContent, AudioContent, Previous.EmbeddedResource]);
/**
 * @internal
 */
export const SamplingContent = /*#__PURE__*/Schema.Union([Previous.TextContent, Previous.ImageContent, AudioContent]);
/**
 * @internal
 */
export const PromptMessage = /*#__PURE__*/Schema.Struct({
  role: Previous.Role,
  content: PromptOrToolContent
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
export const ToolAnnotations = /*#__PURE__*/Schema.Struct({
  title: /*#__PURE__*/optional(Schema.String),
  readOnlyHint: /*#__PURE__*/optional(Schema.Boolean),
  destructiveHint: /*#__PURE__*/optional(Schema.Boolean),
  idempotentHint: /*#__PURE__*/optional(Schema.Boolean),
  openWorldHint: /*#__PURE__*/optional(Schema.Boolean)
});
/**
 * @internal
 */
export const Tool = /*#__PURE__*/Schema.Struct({
  ...Previous.Tool.fields,
  annotations: /*#__PURE__*/optional(ToolAnnotations)
});
/**
 * @internal
 */
export const InitializeResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  protocolVersion: Schema.String,
  capabilities: ServerCapabilities,
  serverInfo: Previous.Implementation,
  instructions: /*#__PURE__*/optional(Schema.String)
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
export const CallToolResult = /*#__PURE__*/Schema.Struct({
  ...Previous.ResultMeta.fields,
  content: /*#__PURE__*/Schema.Array(PromptOrToolContent),
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
export class Initialize extends /*#__PURE__*/Rpc.make("initialize", {
  success: InitializeResult,
  error: Previous.McpError,
  payload: {
    ...Previous.RequestMeta.fields,
    protocolVersion: Schema.String,
    capabilities: Previous.ClientCapabilities,
    clientInfo: Previous.Implementation
  }
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
    arguments: /*#__PURE__*/optional(Schema.JsonObject)
  }
}) {}
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
    metadata: /*#__PURE__*/optional(Schema.JsonObject)
  }
}) {}
/**
 * @internal
 */
export class ProgressNotification extends /*#__PURE__*/Rpc.make("notifications/progress", {
  payload: {
    ...Previous.NotificationMeta.fields,
    progressToken: Previous.ProgressToken,
    progress: Schema.Finite,
    total: /*#__PURE__*/optional(Schema.Finite),
    message: /*#__PURE__*/optional(Schema.String)
  }
}) {}
/**
 * @internal
 */
export class ClientRequestRpcs extends /*#__PURE__*/RpcGroup.make(Previous.Ping, Initialize, Previous.Complete, Previous.SetLevel, GetPrompt, Previous.ListPrompts, Previous.ListResources, Previous.ListResourceTemplates, Previous.ReadResource, Previous.Subscribe, Previous.Unsubscribe, CallTool, ListTools) {}
/**
 * @internal
 */
export class ClientNotificationRpcs extends /*#__PURE__*/RpcGroup.make(Previous.CancelledNotification, ProgressNotification, Previous.InitializedNotification, Previous.RootsListChangedNotification) {}
/**
 * @internal
 */
export class ServerRequestRpcs extends /*#__PURE__*/RpcGroup.make(Previous.Ping, CreateMessage, Previous.ListRoots) {}
/**
 * @internal
 */
export class ServerNotificationRpcs extends /*#__PURE__*/RpcGroup.make(Previous.CancelledNotification, ProgressNotification, Previous.LoggingMessageNotification, Previous.ResourceUpdatedNotification, Previous.ResourceListChangedNotification, Previous.ToolListChangedNotification, Previous.PromptListChangedNotification) {}
//# sourceMappingURL=v2025_03_26.js.map