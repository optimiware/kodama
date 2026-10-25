/**
 * MCP v2026-07-28 wire schemas.
 *
 * @internal
 */
import * as Rpc from "../../../rpc/Rpc.js";
import * as RpcGroup from "../../../rpc/RpcGroup.js";
import * as Schema from "../../../Schema.js";
import * as Previous from "./v2025_11_25.js";
export * from "./v2025_11_25.js";
/**
 * @internal
 */
export const protocolVersion = "2026-07-28";
const optional = Previous.optional;
const Meta = /*#__PURE__*/optional(Schema.JsonObject);
/**
 * @internal
 */
export const RequestId = /*#__PURE__*/Schema.Union([Schema.String, Schema.Int]);
/**
 * @internal
 */
export const ProgressToken = RequestId;
/**
 * @internal
 */
export const Role = Previous.Role;
/**
 * @internal
 */
export const LoggingLevel = Previous.LoggingLevel;
/**
 * @internal
 */
export const Implementation = Previous.Implementation;
/**
 * @internal
 */
export const ClientCapabilities = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  experimental: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.JsonObject)),
  roots: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({})),
  sampling: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    context: /*#__PURE__*/optional(Schema.JsonObject),
    tools: /*#__PURE__*/optional(Schema.JsonObject)
  })),
  elicitation: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    form: /*#__PURE__*/optional(Schema.JsonObject),
    url: /*#__PURE__*/optional(Schema.JsonObject)
  })),
  extensions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.JsonObject))
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const ServerCapabilities = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  experimental: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.JsonObject)),
  logging: /*#__PURE__*/optional(Schema.JsonObject),
  completions: /*#__PURE__*/optional(Schema.JsonObject),
  prompts: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  resources: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    subscribe: /*#__PURE__*/optional(Schema.Boolean),
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    listChanged: /*#__PURE__*/optional(Schema.Boolean)
  })),
  extensions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.JsonObject))
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const RequestMetaObject = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  progressToken: /*#__PURE__*/optional(ProgressToken),
  "io.modelcontextprotocol/protocolVersion": Schema.String,
  "io.modelcontextprotocol/clientInfo": /*#__PURE__*/optional(Implementation),
  "io.modelcontextprotocol/clientCapabilities": ClientCapabilities,
  "io.modelcontextprotocol/logLevel": /*#__PURE__*/optional(LoggingLevel)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const RequestParams = /*#__PURE__*/Schema.Struct({
  _meta: RequestMetaObject
});
/**
 * @internal
 */
export const NotificationMetaObject = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  "io.modelcontextprotocol/subscriptionId": /*#__PURE__*/optional(RequestId)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const NotificationParams = /*#__PURE__*/Schema.Struct({
  _meta: /*#__PURE__*/optional(NotificationMetaObject)
});
/**
 * @internal
 */
export const ResultMetaObject = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  "io.modelcontextprotocol/serverInfo": Implementation
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const ResultMeta = {
  _meta: ResultMetaObject,
  resultType: /*#__PURE__*/Schema.Literal("complete")
};
/**
 * @internal
 */
export const McpError = /*#__PURE__*/Schema.Struct({
  code: Schema.Int,
  message: Schema.String,
  data: /*#__PURE__*/optional(Schema.Json)
});
/**
 * @internal
 */
export const METHOD_NOT_FOUND = -32601;
/**
 * @internal
 */
export const INVALID_PARAMS = -32602;
/**
 * @internal
 */
export const MISSING_REQUIRED_CLIENT_CAPABILITY = -32021;
/**
 * @internal
 */
export const Annotations = Previous.Annotations;
/**
 * @internal
 */
export const Resource = /*#__PURE__*/Schema.Struct({
  ...Previous.Resource.fields,
  size: /*#__PURE__*/optional(Schema.Int),
  annotations: /*#__PURE__*/optional(Annotations)
});
/**
 * @internal
 */
export const ResourceTemplate = Previous.ResourceTemplate;
/**
 * @internal
 */
export const ResourceContents = Previous.ResourceContents;
/**
 * @internal
 */
export const TextContent = Previous.TextContent;
/**
 * @internal
 */
export const ImageContent = Previous.ImageContent;
/**
 * @internal
 */
export const AudioContent = Previous.AudioContent;
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
export const EmbeddedResource = Previous.EmbeddedResource;
/**
 * @internal
 */
export const ContentBlock = /*#__PURE__*/Schema.Union([TextContent, ImageContent, AudioContent, ResourceLink, EmbeddedResource]);
/**
 * @internal
 */
export const Prompt = Previous.Prompt;
/**
 * @internal
 */
export const PromptMessage = /*#__PURE__*/Schema.Struct({
  role: Role,
  content: ContentBlock
});
const ToolInputSchema = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  $schema: /*#__PURE__*/optional(Schema.String),
  type: /*#__PURE__*/Schema.Literal("object")
}), [Schema.JsonObject]);
const ToolOutputSchema = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  $schema: /*#__PURE__*/optional(Schema.String)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const ToolAnnotations = Previous.ToolAnnotations;
/**
 * @internal
 */
export const Tool = /*#__PURE__*/Schema.Struct({
  ...Previous.Tool.fields,
  inputSchema: ToolInputSchema,
  outputSchema: /*#__PURE__*/optional(ToolOutputSchema),
  annotations: /*#__PURE__*/optional(ToolAnnotations)
});
/**
 * @internal
 */
export const ToolUseContent = Previous.ToolUseContent;
/**
 * @internal
 */
export const ToolResultContent = /*#__PURE__*/Schema.Struct({
  type: /*#__PURE__*/Schema.Literal("tool_result"),
  toolUseId: Schema.String,
  content: /*#__PURE__*/Schema.Array(ContentBlock),
  structuredContent: /*#__PURE__*/optional(Schema.Json),
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
  role: Role,
  content: /*#__PURE__*/Schema.Union([SamplingMessageContentBlock, /*#__PURE__*/Schema.Array(SamplingMessageContentBlock)]),
  _meta: Meta
});
/**
 * @internal
 */
export const ModelHint = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  name: /*#__PURE__*/optional(Schema.String)
}), [Schema.JsonObject]);
const ModelPriority = /*#__PURE__*/Schema.Finite.check(/*#__PURE__*/Schema.isBetween({
  minimum: 0,
  maximum: 1
}));
/**
 * @internal
 */
export const ModelPreferences = /*#__PURE__*/Schema.Struct({
  ...Previous.ModelPreferences.fields,
  hints: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(ModelHint)),
  costPriority: /*#__PURE__*/optional(ModelPriority),
  speedPriority: /*#__PURE__*/optional(ModelPriority),
  intelligencePriority: /*#__PURE__*/optional(ModelPriority)
});
/**
 * @internal
 */
export const ToolChoice = Previous.ToolChoice;
/**
 * @internal
 */
export const CreateMessageRequest = /*#__PURE__*/Schema.Struct({
  method: /*#__PURE__*/Schema.Literal("sampling/createMessage"),
  params: /*#__PURE__*/Schema.Struct({
    messages: /*#__PURE__*/Schema.Array(SamplingMessage),
    modelPreferences: /*#__PURE__*/optional(ModelPreferences),
    systemPrompt: /*#__PURE__*/optional(Schema.String),
    includeContext: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literals(["none", "thisServer", "allServers"])),
    temperature: /*#__PURE__*/optional(Schema.Finite),
    maxTokens: Schema.Int,
    stopSequences: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String)),
    metadata: /*#__PURE__*/optional(Schema.JsonObject),
    tools: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Tool)),
    toolChoice: /*#__PURE__*/optional(ToolChoice)
  })
});
/**
 * @internal
 */
export const CreateMessageResult = /*#__PURE__*/Schema.Struct({
  ...SamplingMessage.fields,
  model: Schema.String,
  stopReason: /*#__PURE__*/optional(Schema.String)
});
/**
 * @internal
 */
export const Root = Previous.Root;
/**
 * @internal
 */
export const ListRootsRequest = /*#__PURE__*/Schema.Struct({
  method: /*#__PURE__*/Schema.Literal("roots/list"),
  params: /*#__PURE__*/optional(/*#__PURE__*/Schema.Struct({
    _meta: Meta
  }))
});
/**
 * @internal
 */
export const ListRootsResult = /*#__PURE__*/Schema.Struct({
  roots: /*#__PURE__*/Schema.Array(Root)
});
/**
 * @internal
 */
export const StringSchema = Previous.StringSchema;
/**
 * @internal
 */
export const NumberSchema = Previous.NumberSchema;
/**
 * @internal
 */
export const BooleanSchema = Previous.BooleanSchema;
/**
 * @internal
 */
export const SingleSelectEnumSchema = Previous.SingleSelectEnumSchema;
/**
 * @internal
 */
export const MultiSelectEnumSchema = Previous.MultiSelectEnumSchema;
/**
 * @internal
 */
export const LegacyTitledEnumSchema = Previous.LegacyTitledEnumSchema;
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
  mode: /*#__PURE__*/optional(/*#__PURE__*/Schema.Literal("form")),
  message: Schema.String,
  requestedSchema: RequestedSchema
});
/**
 * @internal
 */
export const ElicitRequestURLParams = /*#__PURE__*/Schema.Struct({
  mode: /*#__PURE__*/Schema.Literal("url"),
  message: Schema.String,
  url: Schema.String
});
/**
 * @internal
 */
export const ElicitRequestParams = /*#__PURE__*/Schema.Union([ElicitRequestFormParams, ElicitRequestURLParams]);
/**
 * @internal
 */
export const ElicitRequest = /*#__PURE__*/Schema.Struct({
  method: /*#__PURE__*/Schema.Literal("elicitation/create"),
  params: ElicitRequestParams
});
/**
 * @internal
 */
export const ElicitResult = /*#__PURE__*/Schema.Struct({
  action: /*#__PURE__*/Schema.Literals(["accept", "decline", "cancel"]),
  content: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.Union([Schema.String, Schema.Finite, Schema.Boolean, /*#__PURE__*/Schema.Array(Schema.String)])))
});
/**
 * @internal
 */
export const InputRequest = /*#__PURE__*/Schema.Union([CreateMessageRequest, ListRootsRequest, ElicitRequest]);
/**
 * @internal
 */
export const InputResponse = /*#__PURE__*/Schema.Union([CreateMessageResult, ListRootsResult, ElicitResult]);
/**
 * @internal
 */
export const InputRequests = /*#__PURE__*/Schema.Record(Schema.String, InputRequest);
/**
 * @internal
 */
export const InputResponses = /*#__PURE__*/Schema.Record(Schema.String, InputResponse);
const InputRequiredResultMeta = {
  _meta: ResultMetaObject,
  resultType: /*#__PURE__*/Schema.Literal("input_required")
};
/**
 * @internal
 */
export const InputRequiredResult = /*#__PURE__*/Schema.Union([/*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...InputRequiredResultMeta,
  inputRequests: InputRequests,
  requestState: /*#__PURE__*/optional(Schema.String)
}), [Schema.JsonObject]), /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...InputRequiredResultMeta,
  inputRequests: /*#__PURE__*/optional(InputRequests),
  requestState: Schema.String
}), [Schema.JsonObject])]);
/**
 * @internal
 */
export const InputResponseRequestParams = {
  ...RequestParams.fields,
  inputResponses: /*#__PURE__*/optional(InputResponses),
  requestState: /*#__PURE__*/optional(Schema.String)
};
/**
 * @internal
 */
export const CacheableResult = {
  ...ResultMeta,
  ttlMs: /*#__PURE__*/Schema.Int.check(/*#__PURE__*/Schema.isGreaterThanOrEqualTo(0)),
  cacheScope: /*#__PURE__*/Schema.Literals(["public", "private"])
};
/**
 * @internal
 */
export const PaginatedRequestParams = {
  ...RequestParams.fields,
  cursor: /*#__PURE__*/optional(Schema.String)
};
/**
 * @internal
 */
export const PaginatedResult = {
  ...ResultMeta,
  nextCursor: /*#__PURE__*/optional(Schema.String)
};
/**
 * @internal
 */
export const DiscoverResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...CacheableResult,
  supportedVersions: /*#__PURE__*/Schema.Array(Schema.String),
  capabilities: ServerCapabilities,
  instructions: /*#__PURE__*/optional(Schema.String)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class Discover extends /*#__PURE__*/Rpc.make("server/discover", {
  success: DiscoverResult,
  error: McpError,
  payload: RequestParams
}) {}
/**
 * @internal
 */
export const ListResourcesResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...PaginatedResult,
  ...CacheableResult,
  resources: /*#__PURE__*/Schema.Array(Resource)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class ListResources extends /*#__PURE__*/Rpc.make("resources/list", {
  success: ListResourcesResult,
  error: McpError,
  payload: PaginatedRequestParams
}) {}
/**
 * @internal
 */
export const ListResourceTemplatesResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...PaginatedResult,
  ...CacheableResult,
  resourceTemplates: /*#__PURE__*/Schema.Array(ResourceTemplate)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class ListResourceTemplates extends /*#__PURE__*/Rpc.make("resources/templates/list", {
  success: ListResourceTemplatesResult,
  error: McpError,
  payload: PaginatedRequestParams
}) {}
/**
 * @internal
 */
export const ReadResourceResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...CacheableResult,
  contents: /*#__PURE__*/Schema.Array(ResourceContents)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class ReadResource extends /*#__PURE__*/Rpc.make("resources/read", {
  success: /*#__PURE__*/Schema.Union([ReadResourceResult, InputRequiredResult]),
  error: McpError,
  payload: {
    ...InputResponseRequestParams,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export const ListPromptsResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...PaginatedResult,
  ...CacheableResult,
  prompts: /*#__PURE__*/Schema.Array(Prompt)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class ListPrompts extends /*#__PURE__*/Rpc.make("prompts/list", {
  success: ListPromptsResult,
  error: McpError,
  payload: PaginatedRequestParams
}) {}
/**
 * @internal
 */
export const GetPromptResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...ResultMeta,
  description: /*#__PURE__*/optional(Schema.String),
  messages: /*#__PURE__*/Schema.Array(PromptMessage)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class GetPrompt extends /*#__PURE__*/Rpc.make("prompts/get", {
  success: /*#__PURE__*/Schema.Union([GetPromptResult, InputRequiredResult]),
  error: McpError,
  payload: {
    ...InputResponseRequestParams,
    name: Schema.String,
    arguments: /*#__PURE__*/optional(/*#__PURE__*/Schema.Record(Schema.String, Schema.String))
  }
}) {}
/**
 * @internal
 */
export const ListToolsResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...PaginatedResult,
  ...CacheableResult,
  tools: /*#__PURE__*/Schema.Array(Tool)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class ListTools extends /*#__PURE__*/Rpc.make("tools/list", {
  success: ListToolsResult,
  error: McpError,
  payload: PaginatedRequestParams
}) {}
/**
 * @internal
 */
export const CallToolResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...ResultMeta,
  content: /*#__PURE__*/Schema.Array(ContentBlock),
  structuredContent: /*#__PURE__*/optional(Schema.Json),
  isError: /*#__PURE__*/optional(Schema.Boolean)
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class CallTool extends /*#__PURE__*/Rpc.make("tools/call", {
  success: /*#__PURE__*/Schema.Union([CallToolResult, InputRequiredResult]),
  error: McpError,
  payload: {
    ...InputResponseRequestParams,
    name: Schema.String,
    arguments: /*#__PURE__*/optional(Schema.JsonObject)
  }
}) {}
/**
 * @internal
 */
export const PromptReference = Previous.PromptReference;
/**
 * @internal
 */
export const ResourceTemplateReference = Previous.ResourceTemplateReference;
/**
 * @internal
 */
export const CompleteResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  ...ResultMeta,
  completion: /*#__PURE__*/Schema.Struct({
    values: /*#__PURE__*/Schema.Array(Schema.String).check(/*#__PURE__*/Schema.isMaxLength(100)),
    total: /*#__PURE__*/optional(Schema.Int),
    hasMore: /*#__PURE__*/optional(Schema.Boolean)
  })
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class Complete extends /*#__PURE__*/Rpc.make("completion/complete", {
  success: CompleteResult,
  error: McpError,
  payload: {
    ...RequestParams.fields,
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
export const SubscriptionFilter = /*#__PURE__*/Schema.Struct({
  toolsListChanged: /*#__PURE__*/optional(Schema.Boolean),
  promptsListChanged: /*#__PURE__*/optional(Schema.Boolean),
  resourcesListChanged: /*#__PURE__*/optional(Schema.Boolean),
  resourceSubscriptions: /*#__PURE__*/optional(/*#__PURE__*/Schema.Array(Schema.String))
});
/**
 * @internal
 */
export const SubscriptionsListenResultMetaObject = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  "io.modelcontextprotocol/serverInfo": Implementation,
  "io.modelcontextprotocol/subscriptionId": RequestId
}), [Schema.JsonObject]);
/**
 * @internal
 */
export const SubscriptionsListenResult = /*#__PURE__*/Schema.StructWithRest(/*#__PURE__*/Schema.Struct({
  _meta: SubscriptionsListenResultMetaObject,
  resultType: /*#__PURE__*/Schema.Literal("complete")
}), [Schema.JsonObject]);
/**
 * @internal
 */
export class SubscriptionsListen extends /*#__PURE__*/Rpc.make("subscriptions/listen", {
  success: SubscriptionsListenResult,
  error: McpError,
  payload: {
    ...RequestParams.fields,
    notifications: SubscriptionFilter
  }
}) {}
/**
 * @internal
 */
export class CancelledNotification extends /*#__PURE__*/Rpc.make("notifications/cancelled", {
  payload: {
    ...NotificationParams.fields,
    requestId: RequestId,
    reason: /*#__PURE__*/optional(Schema.String)
  }
}) {}
/**
 * @internal
 */
export class ProgressNotification extends /*#__PURE__*/Rpc.make("notifications/progress", {
  payload: {
    ...NotificationParams.fields,
    progressToken: ProgressToken,
    progress: Schema.Finite,
    total: /*#__PURE__*/optional(Schema.Finite),
    message: /*#__PURE__*/optional(Schema.String)
  }
}) {}
/**
 * @internal
 */
export class LoggingMessageNotification extends /*#__PURE__*/Rpc.make("notifications/message", {
  payload: {
    ...NotificationParams.fields,
    level: LoggingLevel,
    logger: /*#__PURE__*/optional(Schema.String),
    data: Schema.Json
  }
}) {}
/**
 * @internal
 */
export class ResourceUpdatedNotification extends /*#__PURE__*/Rpc.make("notifications/resources/updated", {
  payload: {
    ...NotificationParams.fields,
    uri: Schema.String
  }
}) {}
/**
 * @internal
 */
export class ResourceListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/resources/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationParams)
}) {}
/**
 * @internal
 */
export class ToolListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/tools/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationParams)
}) {}
/**
 * @internal
 */
export class PromptListChangedNotification extends /*#__PURE__*/Rpc.make("notifications/prompts/list_changed", {
  payload: /*#__PURE__*/Schema.UndefinedOr(NotificationParams)
}) {}
/**
 * @internal
 */
export class SubscriptionsAcknowledgedNotification extends /*#__PURE__*/Rpc.make("notifications/subscriptions/acknowledged", {
  payload: {
    ...NotificationParams.fields,
    notifications: SubscriptionFilter
  }
}) {}
/**
 * @internal
 */
export class ClientRequestRpcs extends /*#__PURE__*/RpcGroup.make(Discover, Complete, GetPrompt, ListPrompts, ListResources, ListResourceTemplates, ReadResource, SubscriptionsListen, CallTool, ListTools) {}
/**
 * @internal
 */
export class ClientNotificationRpcs extends /*#__PURE__*/RpcGroup.make(CancelledNotification) {}
/**
 * @internal
 */
export class ClientRpcs extends /*#__PURE__*/ClientRequestRpcs.merge(ClientNotificationRpcs) {}
// In v2026-07-28 these requests are embedded in InputRequiredResult rather than
// being sent as independent JSON-RPC requests.
/**
 * @internal
 */
export class ServerRequestRpcs extends /*#__PURE__*/RpcGroup.make() {}
/**
 * @internal
 */
export class ServerNotificationRpcs extends /*#__PURE__*/RpcGroup.make(CancelledNotification, ProgressNotification, LoggingMessageNotification, ResourceUpdatedNotification, ResourceListChangedNotification, ToolListChangedNotification, PromptListChangedNotification, SubscriptionsAcknowledgedNotification) {}
//# sourceMappingURL=v2026_07_28.js.map