import * as Data from "../../Data.js";
import * as Effect from "../../Effect.js";
import * as Base64 from "../../encoding/Base64.js";
import * as Match from "../../Match.js";
import * as Predicate from "../../Predicate.js";
import * as Result from "../../Result.js";
import * as RpcClient from "../../rpc/RpcClient.js";
import * as Schema from "../../Schema.js";
import * as PublicMcpSchema from "../McpSchema.js";
import * as McpCore from "./mcpCore.js";
const LEGACY_RESOURCE_NOT_FOUND_ERROR_CODE = -32002;
const BASE64_SENTINEL_PREFIX = "=?base64?";
const BASE64_SENTINEL_SUFFIX = "?=";
// A leading U+FEFF belongs to the header value and must survive comparison with the JSON body.
const routingHeaderDecoder = /*#__PURE__*/new TextDecoder("utf-8", {
  fatal: true,
  ignoreBOM: true
});
/**
 * @internal
 */
export const mcpLogLevels = {
  debug: {
    effect: "Debug",
    order: 0
  },
  info: {
    effect: "Info",
    order: 1
  },
  notice: {
    effect: "Info",
    order: 2
  },
  warning: {
    effect: "Warn",
    order: 3
  },
  error: {
    effect: "Error",
    order: 4
  },
  critical: {
    effect: "Fatal",
    order: 5
  },
  alert: {
    effect: "Fatal",
    order: 6
  },
  emergency: {
    effect: "Fatal",
    order: 7
  }
};
/**
 * @internal
 */
export const decodeRoutingHeader = value => {
  const startsWithSentinel = value.startsWith(BASE64_SENTINEL_PREFIX);
  const endsWithSentinel = value.endsWith(BASE64_SENTINEL_SUFFIX);
  if (!startsWithSentinel || !endsWithSentinel) {
    return /^[\t\x20-\x7e]*$/.test(value) ? value : undefined;
  }
  const decoded = Base64.decode(value.slice(BASE64_SENTINEL_PREFIX.length, -BASE64_SENTINEL_SUFFIX.length));
  if (Result.isFailure(decoded)) return undefined;
  try {
    return routingHeaderDecoder.decode(decoded.success);
  } catch {
    return undefined;
  }
};
/**
 * @internal
 */
export const profileFromClient = request => ({
  protocolVersion: request.protocolVersion,
  clientCapabilities: request.clientCapabilities,
  clientInfo: request.clientInfo,
  requestMetadata: request.initializePayload._meta
});
/**
 * @internal
 */
export const invocationFromClient = request => ({
  clientId: request.clientId,
  protocol: profileFromClient(request),
  requestContext: PublicMcpSchema.McpRequestContext.of({
    clientId: request.clientId,
    protocolVersion: request.protocolVersion,
    clientCapabilities: request.clientCapabilities,
    clientInfo: request.clientInfo,
    requestMetadata: request.requestMetadata
  }),
  serverClient: request
});
/**
 * @internal
 */
export const invocationFromRequestContext = request => ({
  clientId: request.clientId,
  protocol: {
    protocolVersion: request.protocolVersion,
    clientCapabilities: request.clientCapabilities,
    clientInfo: request.clientInfo,
    requestMetadata: request.requestMetadata
  },
  requestContext: request
});
/**
 * @internal
 */
export const requireCompleteOperation = (protocolVersion, outcome) => outcome._tag === "Complete" ? Effect.succeed(outcome.value) : Effect.fail(new McpCore.UnsupportedByProtocol({
  protocolVersion,
  feature: "Client input"
}));
/**
 * Unquotes an exact text mirror when the protocol drops string `structuredContent`.
 *
 * @internal
 */
export const unwrapStringStructuredContent = result => {
  const {
    content,
    structuredContent
  } = result;
  if (typeof structuredContent !== "string" || content.length !== 1) return content;
  const [block] = content;
  return block.type === "text" && block.text === JSON.stringify(structuredContent) ? [{
    ...block,
    text: structuredContent
  }] : content;
};
const isSamplingToolContent = content => Predicate.isReadonlyObject(content) && (content.type === "tool_use" || content.type === "tool_result");
/**
 * @internal
 */
export const samplingRequestRequiresTools = request => {
  if (!Predicate.isReadonlyObject(request)) return false;
  if (request.tools !== undefined || request.toolChoice !== undefined) return true;
  if (!Array.isArray(request.messages)) return false;
  return request.messages.some(message => {
    if (!Predicate.isReadonlyObject(message)) return false;
    const content = message.content;
    return Array.isArray(content) ? content.some(isSamplingToolContent) : isSamplingToolContent(content);
  });
};
/**
 * @internal
 */
export class ProtocolError extends /*#__PURE__*/Data.TaggedError("ProtocolError") {
  static fromTool(error) {
    const message = Match.value(error).pipe(Match.tag("ToolNotFound", error => `Tool '${error.name}' not found`), Match.tag("UnsupportedByProtocol", error => `${error.feature} is not supported by MCP ${error.protocolVersion}`), Match.tags({
      InvalidToolInput: error => error.message,
      InvalidToolContinuation: error => error.message,
      ToolExecutionError: error => error.message
    }), Match.exhaustive);
    return new ProtocolError({
      code: error._tag === "ToolExecutionError" ? PublicMcpSchema.INTERNAL_ERROR_CODE : PublicMcpSchema.INVALID_PARAMS_ERROR_CODE,
      message
    });
  }
  static fromFeature(error) {
    if (Predicate.hasProperty(error, "_tag")) {
      if (error._tag === "PromptNotFound" && Predicate.hasProperty(error, "name") && Predicate.isString(error.name)) {
        return new ProtocolError({
          code: PublicMcpSchema.INVALID_PARAMS_ERROR_CODE,
          message: `Prompt '${error.name}' not found`
        });
      }
      if (error._tag === "ResourceNotFound" && Predicate.hasProperty(error, "uri") && Predicate.isString(error.uri)) {
        return new ProtocolError({
          code: LEGACY_RESOURCE_NOT_FOUND_ERROR_CODE,
          message: `Resource '${error.uri}' not found`
        });
      }
    }
    const decoded = decodeProtocolErrorFields(error);
    return Result.isSuccess(decoded) ? new ProtocolError(decoded.success) : new ProtocolError({
      code: PublicMcpSchema.INTERNAL_ERROR_CODE,
      message: "MCP feature handler failed"
    });
  }
}
const ProtocolErrorFields = /*#__PURE__*/Schema.Struct({
  code: Schema.Finite,
  message: Schema.String,
  data: /*#__PURE__*/Schema.optionalKey(Schema.Unknown)
});
const decodeProtocolErrorFields = /*#__PURE__*/Schema.decodeUnknownResult(ProtocolErrorFields);
const isReverseOperationUnsupported = cause => Predicate.isTagged(cause, "McpReverseOperationUnsupported");
/**
 * @internal
 */
export const reverseError = operation => cause => isReverseOperationUnsupported(cause) ? cause : new PublicMcpSchema.McpReverseOperationError({
  operation,
  cause
});
/**
 * @internal
 */
export const transcode = (from, to, input) => Schema.encodeEffect(from)(input).pipe(Effect.flatMap(Schema.decodeUnknownEffect(to)));
/**
 * @internal
 */
export const transcodeStrict = (from, to, input) => Schema.encodeEffect(from)(input).pipe(Effect.flatMap(Schema.decodeUnknownEffect(to, {
  onExcessProperty: "error"
})));
/**
 * @internal
 */
export const makeNotificationProjector = /*#__PURE__*/Effect.fn(function* (options, notification) {
  return McpCore.ServerNotification.$match(notification, {
    Cancelled: notification => ({
      tag: PublicMcpSchema.CancelledNotification._tag,
      payload: PublicMcpSchema.CancelledNotification.payloadSchema.make({
        _meta: notification.metadata,
        requestId: notification.requestId,
        reason: notification.reason
      })
    }),
    Progress: notification => ({
      tag: PublicMcpSchema.ProgressNotification._tag,
      payload: PublicMcpSchema.ProgressNotification.payloadSchema.make({
        _meta: notification.metadata,
        progressToken: notification.progressToken,
        progress: notification.progress,
        total: notification.total,
        message: options.supportsProgressMessage ? notification.message : undefined
      })
    }),
    LoggingMessage: notification => ({
      tag: PublicMcpSchema.LoggingMessageNotification._tag,
      payload: PublicMcpSchema.LoggingMessageNotification.payloadSchema.make({
        _meta: notification.metadata,
        level: notification.level,
        logger: notification.logger,
        data: notification.data
      })
    }),
    ResourceUpdated: notification => ({
      tag: PublicMcpSchema.ResourceUpdatedNotification._tag,
      payload: PublicMcpSchema.ResourceUpdatedNotification.payloadSchema.make({
        _meta: notification.metadata,
        uri: notification.uri
      })
    }),
    ResourcesChanged: notification => ({
      tag: PublicMcpSchema.ResourceListChangedNotification._tag,
      payload: PublicMcpSchema.ResourceListChangedNotification.payloadSchema.make({
        _meta: notification.metadata
      })
    }),
    ToolsChanged: notification => ({
      tag: PublicMcpSchema.ToolListChangedNotification._tag,
      payload: PublicMcpSchema.ToolListChangedNotification.payloadSchema.make({
        _meta: notification.metadata
      })
    }),
    PromptsChanged: notification => ({
      tag: PublicMcpSchema.PromptListChangedNotification._tag,
      payload: PublicMcpSchema.PromptListChangedNotification.payloadSchema.make({
        _meta: notification.metadata
      })
    }),
    ElicitationComplete: () => undefined
  });
});
/**
 * @internal
 */
export const isSubscriptionServerNotification = notification => {
  switch (notification._tag) {
    case "ToolsChanged":
    case "PromptsChanged":
    case "ResourcesChanged":
    case "ResourceUpdated":
      return true;
    default:
      return false;
  }
};
/**
 * @internal
 */
export const make = options => {
  const payloadCodecsCache = new WeakMap();
  const payloadCodecs = rpc => {
    let codecs = payloadCodecsCache.get(rpc);
    if (codecs === undefined) {
      const schema = Schema.toCodecJson(rpc.payloadSchema);
      codecs = {
        decode: Schema.decodeUnknownEffect(schema),
        encode: Schema.encodeUnknownEffect(schema)
      };
      payloadCodecsCache.set(rpc, codecs);
    }
    return codecs;
  };
  const installHandlers = (core, lifecycle, target) => options.handlerRpcs === undefined || options.makeHandlers === undefined ? Effect.void : lifecycle === undefined && options.runtime._tag === "Stateful" ? Effect.die("MCP sessionful handler installation requires a lifecycle runtime") : target.install(options, options.handlerRpcs, options.makeHandlers(core,
  // TypeScript cannot narrow HandlerLifecycle<Runtime> from the generic runtime tag; the preceding guard
  // enforces the stateful lifecycle invariant.
  options.runtime._tag === "Stateful" ? lifecycle : undefined, target.context));
  const makeReverseClient = profile => RpcClient.make(options.serverRequestRpcs, {
    spanPrefix: "McpServer/Client"
  }).pipe(Effect.map(client => options.toReverseClient(profile, client)));
  return {
    ...options,
    payloadCodecs,
    installHandlers,
    makeReverseClient
  };
};
//# sourceMappingURL=mcpProtocol.js.map