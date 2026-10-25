/**
 * Version-neutral MCP server records and semantic operations.
 *
 * @internal
 */
import * as Arr from "../../Array.js";
import * as Data from "../../Data.js";
import * as Effect from "../../Effect.js";
import * as McpSchema from "../McpSchema.js";
/**
 * @internal
 */
export const OperationOutcome = {
  Complete: value => ({
    _tag: "Complete",
    value
  }),
  InputRequired: fields => ({
    _tag: "InputRequired",
    ...fields
  })
};
// NOTE: McpInvocation is runtime context, not a wire DTO. It combines the
// negotiated profile with the request-scoped service used by handlers.
/**
 * @internal
 */
export class ResourceNotFound extends /*#__PURE__*/Data.TaggedError("ResourceNotFound") {}
// NOTE: Core errors preserve semantic failure categories before adapters map
// them to dated MCP error codes and messages.
/**
 * @internal
 */
export class ToolNotFound extends /*#__PURE__*/Data.TaggedError("ToolNotFound") {}
/**
 * @internal
 */
export class InvalidToolInput extends /*#__PURE__*/Data.TaggedError("InvalidToolInput") {}
/**
 * @internal
 */
export class InvalidToolContinuation extends /*#__PURE__*/Data.TaggedError("InvalidToolContinuation") {}
/**
 * @internal
 */
export class ToolExecutionError extends /*#__PURE__*/Data.TaggedError("ToolExecutionError") {}
/**
 * @internal
 */
export class UnsupportedByProtocol extends /*#__PURE__*/Data.TaggedError("UnsupportedByProtocol") {}
/**
 * @internal
 */
export class PromptNotFound extends /*#__PURE__*/Data.TaggedError("PromptNotFound") {}
/**
 * @internal
 */
export const ClientNotification = /*#__PURE__*/Data.taggedEnum();
/**
 * @internal
 */
export const ServerNotification = /*#__PURE__*/Data.taggedEnum();
/**
 * @internal
 */
export const make = /*#__PURE__*/Effect.sync(() => {
  const registrations = new Map();
  const resourceRegistrations = [];
  const resourceTemplateRegistrations = [];
  const promptRegistrations = new Map();
  const completionRegistrations = new Map();
  const tools = {
    register: registration => Effect.sync(() => {
      registrations.set(registration.descriptor.name, registration);
    }),
    list: profile => Effect.sync(() => {
      const descriptors = [];
      for (const registration of registrations.values()) {
        if (registration.isVisible(profile)) {
          descriptors.push(registration.descriptor);
        }
      }
      return descriptors;
    }),
    call: (call, invocation) => Effect.suspend(() => {
      const registration = registrations.get(call.name);
      if (registration === undefined) {
        return new ToolNotFound({
          name: call.name
        });
      }
      if (!registration.isVisible(invocation.protocol)) {
        return new ToolNotFound({
          name: call.name
        });
      }
      return registration.handle(call, invocation);
    })
  };
  const resources = {
    register: registration => Effect.sync(() => {
      const index = resourceRegistrations.findIndex(entry => entry.descriptor.uri === registration.descriptor.uri);
      if (index === -1) {
        resourceRegistrations.push(registration);
      } else {
        resourceRegistrations[index] = registration;
      }
    }),
    registerTemplate: registration => Effect.sync(() => {
      const index = resourceTemplateRegistrations.findIndex(entry => entry.descriptor.uriTemplate === registration.descriptor.uriTemplate);
      if (index === -1) {
        resourceTemplateRegistrations.push(registration);
      } else {
        resourceTemplateRegistrations[index] = registration;
      }
    }),
    list: profile => Effect.sync(() => resourceRegistrations.filter(entry => entry.isVisible(profile)).map(entry => entry.descriptor)),
    listTemplates: profile => Effect.sync(() => resourceTemplateRegistrations.filter(entry => entry.isVisible(profile)).map(entry => entry.descriptor)),
    read: Effect.fnUntraced(function* (uri, invocation) {
      const resource = resourceRegistrations.find(entry => entry.descriptor.uri === uri);
      if (resource !== undefined && resource.isVisible(invocation.protocol)) {
        return yield* resource.read(invocation);
      }
      for (const template of resourceTemplateRegistrations) {
        const params = template.match(uri);
        if (params !== undefined && template.isVisible(invocation.protocol)) {
          return yield* template.read(uri, params, invocation);
        }
      }
      return yield* new ResourceNotFound({
        uri
      });
    })
  };
  const prompts = {
    register: registration => Effect.sync(() => {
      promptRegistrations.set(registration.descriptor.name, registration);
    }),
    list: profile => Effect.sync(() => Array.from(promptRegistrations.values()).filter(entry => entry.isVisible(profile)).map(entry => entry.descriptor)),
    get: Effect.fnUntraced(function* (name, args, invocation) {
      const registration = promptRegistrations.get(name);
      if (registration === undefined || !registration.isVisible(invocation.protocol)) {
        return yield* new PromptNotFound({
          name
        });
      }
      return yield* registration.get(args, invocation);
    })
  };
  const completions = {
    register: (key, complete) => Effect.sync(() => {
      completionRegistrations.set(key, complete);
    }),
    complete: Effect.fnUntraced(function* (request, invocation) {
      const key = request.reference.type === "prompt" ? `prompt/${request.reference.name}/${request.argument.name}` : `resource/${request.reference.uriTemplate}/${request.argument.name}`;
      const complete = completionRegistrations.get(key);
      if (complete === undefined) {
        return yield* new McpSchema.InvalidParams({
          message: "Unknown completion reference or argument"
        });
      }
      const result = yield* complete(request, invocation);
      const values = Arr.take(result.values, 100);
      return {
        ...result,
        values,
        hasMore: result.hasMore === true || values.length < result.values.length
      };
    })
  };
  const registrationPresence = Effect.sync(() => ({
    tools: registrations.size > 0,
    resources: resourceRegistrations.length > 0 || resourceTemplateRegistrations.length > 0,
    prompts: promptRegistrations.size > 0
  }));
  return {
    tools,
    resources,
    prompts,
    completions,
    registrationPresence
  };
});
//# sourceMappingURL=mcpCore.js.map