/**
 * Defines prompts sent to AI language models.
 *
 * A prompt is an ordered list of messages. Messages can use roles such as
 * system, user, assistant, and tool, and their content can be split into typed
 * parts such as text, files, reasoning, tool calls, tool results, and approval
 * messages. This module helps build prompts, combine them, and convert raw
 * input or response parts into the shared prompt shape.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Effect from "../Effect.js";
import * as Base64 from "../encoding/Base64.js";
import { dual } from "../Function.js";
import { pipeArguments } from "../Pipeable.js";
import * as Predicate from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as SchemaIssue from "../SchemaIssue.js";
import * as SchemaParser from "../SchemaParser.js";
import * as SchemaTransformation from "../SchemaTransformation.js";
// =============================================================================
// Options
// =============================================================================
/**
 * Schema for provider-specific options that can be attached to content parts
 * and messages.
 *
 * **Details**
 *
 * Provider-specific options are keyed by provider-specific names, and each
 * value is JSON or `null`.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export const ProviderOptions = /*#__PURE__*/Schema.Record(Schema.String, /*#__PURE__*/Schema.NullOr(Schema.Json));
// =============================================================================
// Base Part
// =============================================================================
const PartTypeId = "~effect/ai/Prompt/Part";
/**
 * Type guard to check if a value is a Part.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isPart = u => Predicate.hasProperty(u, PartTypeId);
const BasePart = /*#__PURE__*/Schema.Struct({
  [PartTypeId]: /*#__PURE__*/Schema.Literal(PartTypeId).pipe(/*#__PURE__*/Schema.withDecodingDefaultKey(/*#__PURE__*/Effect.succeed(PartTypeId), {
    encodingStrategy: "omit"
  })),
  options: /*#__PURE__*/ProviderOptions.pipe(/*#__PURE__*/Schema.withDecodingDefault(/*#__PURE__*/Effect.succeed({})))
});
/**
 * Creates a new content part of the specified type.
 *
 * **Example** (Creating content parts)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const textPart = Prompt.makePart("text", {
 *   text: "Hello, world!"
 * })
 *
 * const filePart = Prompt.makePart("file", {
 *   mediaType: "image/png",
 *   fileName: "screenshot.png",
 *   data: new Uint8Array([1, 2, 3])
 * })
 *
 * const result = [textPart.type, filePart.type] // => ["text", "file"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makePart = (
/**
 * The type of part to create.
 */
type,
/**
 * Parameters specific to the part type being created.
 */
params) => ({
  ...params,
  [PartTypeId]: PartTypeId,
  type,
  options: params.options ?? {}
});
/**
 * Schema for validation and encoding of text parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const TextPart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("text"),
  text: Schema.String
}).annotate({
  identifier: "TextPart"
});
/**
 * Constructs a new text part.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const textPart = params => makePart("text", params);
/**
 * Schema for validation and encoding of reasoning parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ReasoningPart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("reasoning"),
  text: Schema.String
}).annotate({
  identifier: "ReasoningPart"
});
/**
 * Constructs a new reasoning part.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const reasoningPart = params => makePart("reasoning", params);
/**
 * Schema for validation and encoding of file parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const FilePart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("file"),
  mediaType: Schema.String,
  fileName: Schema.optional(Schema.String),
  data: Schema.Union([Schema.String, Schema.Uint8Array, Schema.URL])
}).annotate({
  identifier: "FilePart"
});
/**
 * Constructs a `FilePart` for prompt file attachments.
 *
 * **When to use**
 *
 * Use to create the file-attachment part of a prompt from typed file part
 * parameters.
 *
 * @see {@link makePart} for the generic part constructor
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const filePart = params => makePart("file", params);
/**
 * Schema for validation and encoding of tool call parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ToolCallPart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("tool-call"),
  id: Schema.String,
  name: Schema.String,
  params: Schema.Unknown,
  providerExecuted: Schema.Boolean.pipe(Schema.withDecodingDefault(Effect.succeed(false)))
}).annotate({
  identifier: "ToolCallPart"
});
/**
 * Constructs a new tool call part.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const toolCallPart = params => makePart("tool-call", params);
/**
 * Schema for validation and encoding of tool result parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ToolResultPart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("tool-result"),
  id: Schema.String,
  name: Schema.String,
  isFailure: Schema.Boolean,
  result: Schema.Unknown,
  providerExecuted: Schema.Boolean.pipe(Schema.withDecodingDefault(Effect.succeed(false)))
}).annotate({
  identifier: "ToolResultPart"
});
/**
 * Constructs a new tool result part.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const toolResultPart = params => makePart("tool-result", params);
/**
 * Schema for validation and encoding of tool approval response parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ToolApprovalResponsePart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("tool-approval-response"),
  approvalId: Schema.String,
  approved: Schema.Boolean,
  reason: Schema.optional(Schema.String)
}).annotate({
  identifier: "ToolApprovalResponsePart"
});
/**
 * Constructs a new tool approval response part.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const toolApprovalResponsePart = params => makePart("tool-approval-response", params);
/**
 * Schema for validation and encoding of tool approval request parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ToolApprovalRequestPart = /*#__PURE__*/Schema.Struct({
  ...BasePart.fields,
  type: Schema.Literal("tool-approval-request"),
  approvalId: Schema.String,
  toolCallId: Schema.String
}).annotate({
  identifier: "ToolApprovalRequestPart"
});
/**
 * Constructs a new tool approval request part.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const toolApprovalRequestPart = params => makePart("tool-approval-request", params);
/**
 * Schema for validation and encoding of content parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Part = /*#__PURE__*/Schema.Union([TextPart, ReasoningPart, FilePart, ToolCallPart, ToolResultPart, ToolApprovalResponsePart, ToolApprovalRequestPart]);
// =============================================================================
// Base Message
// =============================================================================
const MessageTypeId = "~effect/ai/Prompt/Message";
/**
 * Type guard to check if a value is a Message.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isMessage = u => Predicate.hasProperty(u, MessageTypeId);
const BaseMessage = /*#__PURE__*/Schema.Struct({
  [MessageTypeId]: /*#__PURE__*/Schema.Literal(MessageTypeId).pipe(/*#__PURE__*/Schema.withDecodingDefaultKey(/*#__PURE__*/Effect.succeed(MessageTypeId), {
    encodingStrategy: "omit"
  })),
  options: /*#__PURE__*/ProviderOptions.pipe(/*#__PURE__*/Schema.withDecodingDefault(/*#__PURE__*/Effect.succeed({})))
});
/**
 * Creates a new message with the specified role.
 *
 * **Example** (Creating messages)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const textPart = Prompt.makePart("text", {
 *   text: "Hello, world!"
 * })
 *
 * const userMessage = Prompt.makeMessage("user", {
 *   content: [textPart]
 * })
 * const result = [userMessage.role, userMessage.content.length] // => ["user", 1]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeMessage = (role, params) => ({
  ...params,
  [MessageTypeId]: MessageTypeId,
  role,
  options: params.options ?? {}
});
/**
 * Schema that decodes a string into content containing a single `TextPart` and,
 * when encoding, emits the `text` value of the first part.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ContentFromString = /*#__PURE__*/Schema.String.pipe(/*#__PURE__*/Schema.decodeTo(/*#__PURE__*/Schema.NonEmptyArray(/*#__PURE__*/Schema.toType(TextPart)), /*#__PURE__*/SchemaTransformation.transform({
  decode: text => Arr.of(makePart("text", {
    text
  })),
  encode: content => content[0].text
})));
/**
 * Schema for validation and encoding of system messages.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const SystemMessage = /*#__PURE__*/Schema.Struct({
  ...BaseMessage.fields,
  role: Schema.Literal("system"),
  content: Schema.String
}).annotate({
  identifier: "SystemMessage"
});
/**
 * Constructs a new system message.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const systemMessage = params => makeMessage("system", params);
/**
 * Schema for validation and encoding of user message content parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const UserMessagePart = /*#__PURE__*/Schema.Union([TextPart, FilePart]);
/**
 * Schema for validation and encoding of user messages.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const UserMessage = /*#__PURE__*/Schema.Struct({
  ...BaseMessage.fields,
  role: Schema.Literal("user"),
  content: Schema.Union([ContentFromString.check(Schema.makeFilter(content => content.length === 1 && Object.keys(content[0].options).length === 0)), Schema.Array(Schema.Union([TextPart, FilePart]))])
}).annotate({
  identifier: "UserMessage"
});
/**
 * Constructs a new user message.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const userMessage = params => makeMessage("user", params);
/**
 * Schema for validation and encoding of assistant message content parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const AssistantMessagePart = /*#__PURE__*/Schema.Union([TextPart, FilePart, ReasoningPart, ToolCallPart, ToolResultPart, ToolApprovalRequestPart]);
/**
 * Schema for validation and encoding of assistant messages.
 *
 * **Details**
 *
 * Assistant content can be a string decoded through `ContentFromString` or an
 * array of text, file, reasoning, tool-call, tool-result, and
 * tool-approval-request parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const AssistantMessage = /*#__PURE__*/Schema.Struct({
  ...BaseMessage.fields,
  role: Schema.Literal("assistant"),
  content: Schema.Union([ContentFromString.check(Schema.makeFilter(content => content.length === 1 && Object.keys(content[0].options).length === 0)), Schema.Array(Schema.Union([TextPart, FilePart, ReasoningPart, ToolCallPart, ToolResultPart, ToolApprovalRequestPart]))])
}).annotate({
  identifier: "AssistantMessage"
});
/**
 * Constructs a new assistant message.
 *
 * **When to use**
 *
 * Use to add assistant-role prompt history or model responses.
 *
 * **Details**
 *
 * This is the role-specific wrapper around `makeMessage("assistant", params)`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const assistantMessage = params => makeMessage("assistant", params);
/**
 * Schema for validation and encoding of tool message content parts.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ToolMessagePart = /*#__PURE__*/Schema.Union([ToolResultPart, ToolApprovalResponsePart]);
/**
 * Schema for validation and encoding of tool messages.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const ToolMessage = /*#__PURE__*/Schema.Struct({
  ...BaseMessage.fields,
  role: Schema.Literal("tool"),
  content: Schema.Array(Schema.Union([ToolResultPart, ToolApprovalResponsePart]))
}).annotate({
  identifier: "ToolMessage"
});
/**
 * Constructs a new tool message.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const toolMessage = params => makeMessage("tool", params);
/**
 * Schema for validation and encoding of messages.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Message = /*#__PURE__*/Schema.Union([SystemMessage, UserMessage, AssistantMessage, ToolMessage]);
// =============================================================================
// Prompt
// =============================================================================
const TypeId = "~effect/ai/Prompt";
/**
 * Type guard to check if a value is a Prompt.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isPrompt = u => Predicate.hasProperty(u, TypeId);
const $Prompt = /*#__PURE__*/Schema.declare(u => isPrompt(u), {
  identifier: "Prompt"
});
// TODO: is the type annotation necessary?
// TODO: shoudn't the name be `PromptFrom...`?
// TODO: is the explicit encoding necessary? maybe use the default JSON serializer?
/**
 * Schema for AI prompt instances.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Prompt = /*#__PURE__*/Schema.Struct({
  content: Schema.Array(Schema.toEncoded(Message))
}).pipe(/*#__PURE__*/Schema.decodeTo($Prompt, /*#__PURE__*/SchemaTransformation.transformEffect({
  decode: (input, options) => Effect.mapBothEager(SchemaParser.decodeEffect(Schema.Array(Message))(input.content), {
    onSuccess: makePrompt,
    onFailure: () => new SchemaIssue.InvalidValue({
      message: "Invalid Prompt messages"
    }, input.content, options)
  }),
  encode: (prompt, options) => Effect.mapBothEager(SchemaParser.encodeEffect(Schema.Array(Message))(prompt.content), {
    onSuccess: messages => ({
      content: messages
    }),
    onFailure: () => new SchemaIssue.InvalidValue({
      message: "Invalid Prompt messages"
    }, prompt.content, options)
  })
})));
const Proto = {
  [TypeId]: TypeId,
  pipe() {
    return pipeArguments(this, arguments);
  }
};
const makePrompt = content => Object.assign(Object.create(Proto), {
  content
});
const decodeMessagesSync = /*#__PURE__*/Schema.decodeSync(/*#__PURE__*/Schema.Array(Message));
/**
 * An empty prompt with no messages.
 *
 * **Example** (Creating an empty prompt)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const emptyPrompt = Prompt.empty
 * emptyPrompt.content // => []
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const empty = /*#__PURE__*/makePrompt([]);
/**
 * Creates a `Prompt` from an input.
 *
 * **Details**
 *
 * This is the primary constructor for creating prompts, supporting multiple
 * input formats for convenience and flexibility.
 *
 * **Example** (Creating prompts from inputs)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * // From string - creates a user message
 * const textPrompt = Prompt.make("Hello, how are you?")
 *
 * // From messages array
 * const structuredPrompt = Prompt.make([
 *   { role: "system", content: "You are a helpful assistant." },
 *   { role: "user", content: [{ type: "text", text: "Hi!" }] }
 * ])
 *
 * const copiedPrompt = Prompt.make(Prompt.empty)
 *
 * const result = [textPrompt.content[0].role, structuredPrompt.content.length, copiedPrompt.content.length] // => ["user", 2, 0]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = input => {
  if (typeof input === "string") {
    const part = makePart("text", {
      text: input
    });
    const message = makeMessage("user", {
      content: [part]
    });
    return makePrompt([message]);
  }
  if (Predicate.isIterable(input)) {
    return makePrompt(decodeMessagesSync(Arr.fromIterable(input), {
      errors: "all"
    }));
  }
  return input;
};
/**
 * Creates a Prompt from an array of messages.
 *
 * **Example** (Creating prompts from messages)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const messages: ReadonlyArray<Prompt.Message> = [
 *   Prompt.makeMessage("system", {
 *     content: "You are a coding assistant."
 *   }),
 *   Prompt.makeMessage("user", {
 *     content: [Prompt.makePart("text", { text: "Help me with TypeScript" })]
 *   })
 * ]
 *
 * const prompt = Prompt.fromMessages(messages)
 * prompt.content.length // => 2
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromMessages = messages => makePrompt(messages);
const mergeOptions = (left, right) => {
  const result = {
    ...left
  };
  for (const [provider, metadata] of Object.entries(right)) {
    const previous = result[provider];
    result[provider] = Predicate.isObject(previous) && Predicate.isObject(metadata) ? Object.assign({}, previous, metadata) : metadata;
  }
  return result;
};
/**
 * Creates a `Prompt` from response parts by folding completed text and
 * reasoning streams into assistant parts, preserving provider metadata as
 * prompt options, placing tool calls and approval requests in an assistant
 * message, and placing non-preliminary tool results in a tool message using
 * their encoded results.
 *
 * **Example** (Creating prompts from response parts)
 *
 * ```ts import.meta.vitest
 * import { Prompt, Response } from "effect/ai"
 *
 * const responseParts: ReadonlyArray<Response.AnyPart> = [
 *   Response.makePart("text", {
 *     text: "Hello there!"
 *   }),
 *   Response.makePart("tool-call", {
 *     id: "call_1",
 *     name: "get_time",
 *     params: {},
 *     providerExecuted: false
 *   }),
 *   Response.makePart("tool-result", {
 *     id: "call_1",
 *     name: "get_time",
 *     isFailure: false,
 *     result: "10:30 AM",
 *     encodedResult: "10:30 AM",
 *     providerExecuted: false,
 *     preliminary: false
 *   })
 * ]
 *
 * const prompt = Prompt.fromResponseParts(responseParts)
 * // Creates an assistant message with the response content
 * prompt.content.map((message) => message.role) // => ["assistant", "tool"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const fromResponseParts = parts => {
  if (parts.length === 0) {
    return empty;
  }
  const assistantParts = [];
  const toolParts = [];
  const activeTextDeltas = new Map();
  const activeReasoningDeltas = new Map();
  for (const part of parts) {
    switch (part.type) {
      // Text Parts
      case "text":
        {
          assistantParts.push(makePart("text", {
            text: part.text,
            options: part.metadata
          }));
          break;
        }
      // Text Parts (streaming)
      case "text-start":
        {
          activeTextDeltas.set(part.id, {
            text: "",
            options: part.metadata
          });
          break;
        }
      case "text-delta":
        {
          if (activeTextDeltas.has(part.id)) {
            const active = activeTextDeltas.get(part.id);
            active.text += part.delta;
            active.options = mergeOptions(active.options, part.metadata);
          }
          break;
        }
      case "text-end":
        {
          if (activeTextDeltas.has(part.id)) {
            const active = activeTextDeltas.get(part.id);
            active.options = mergeOptions(active.options, part.metadata);
            assistantParts.push(makePart("text", active));
          }
          break;
        }
      // Reasoning Parts
      case "reasoning":
        {
          assistantParts.push(makePart("reasoning", {
            text: part.text,
            options: part.metadata
          }));
          break;
        }
      // Reasoning Parts (streaming)
      case "reasoning-start":
        {
          activeReasoningDeltas.set(part.id, {
            text: "",
            options: part.metadata
          });
          break;
        }
      case "reasoning-delta":
        {
          if (activeReasoningDeltas.has(part.id)) {
            const active = activeReasoningDeltas.get(part.id);
            active.text += part.delta;
            active.options = mergeOptions(active.options, part.metadata);
          }
          break;
        }
      case "reasoning-end":
        {
          if (activeReasoningDeltas.has(part.id)) {
            const active = activeReasoningDeltas.get(part.id);
            active.options = mergeOptions(active.options, part.metadata);
            assistantParts.push(makePart("reasoning", active));
          }
          break;
        }
      // File Parts
      case "file":
        {
          assistantParts.push(makePart("file", {
            data: Base64.encode(part.data),
            mediaType: part.mediaType,
            options: part.metadata
          }));
          break;
        }
      // Tool Call Parts
      case "tool-call":
        {
          assistantParts.push(makePart("tool-call", {
            id: part.id,
            name: part.name,
            params: part.params,
            providerExecuted: part.providerExecuted ?? false,
            options: part.metadata
          }));
          break;
        }
      // Tool Result Parts (skip preliminary results)
      case "tool-result":
        {
          if (part.preliminary !== true) {
            const target = part.providerExecuted === true ? assistantParts : toolParts;
            target.push(makePart("tool-result", {
              id: part.id,
              name: part.name,
              isFailure: part.isFailure,
              result: part.encodedResult,
              providerExecuted: part.providerExecuted ?? false,
              options: part.metadata
            }));
          }
          break;
        }
      // Tool Approval Request Parts
      case "tool-approval-request":
        {
          assistantParts.push(makePart("tool-approval-request", {
            approvalId: part.approvalId,
            toolCallId: part.toolCallId
          }));
          break;
        }
    }
  }
  if (assistantParts.length === 0 && toolParts.length === 0) {
    return empty;
  }
  const messages = [];
  if (assistantParts.length > 0) {
    messages.push(makeMessage("assistant", {
      content: assistantParts
    }));
  }
  if (toolParts.length > 0) {
    messages.push(makeMessage("tool", {
      content: toolParts
    }));
  }
  return makePrompt(messages);
};
// =============================================================================
// Merging Prompts
// =============================================================================
/**
 * Concatenates a prompt with additional raw input by concatenating messages.
 *
 * **Details**
 *
 * The returned prompt contains all messages from the original prompt followed
 * by the provided raw input, preserving message order.
 *
 * **Example** (Concatenating prompts)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const systemPrompt = Prompt.make([{
 *   role: "system",
 *   content: "You are a helpful assistant."
 * }])
 *
 * const merged = Prompt.concat(systemPrompt, "Hello, world!")
 * merged.content.map((message) => message.role) // => ["system", "user"]
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const concat = /*#__PURE__*/dual(2, (self, input) => {
  const other = make(input);
  if (self.content.length === 0) {
    return other;
  }
  if (other.content.length === 0) {
    return self;
  }
  return fromMessages([...self.content, ...other.content]);
});
// =============================================================================
// Manipulating Prompts
// =============================================================================
/**
 * Creates a new prompt from the specified prompt with the system message set
 * to the specified text content.
 *
 * **Gotchas**
 *
 * This method removes and replaces any previous system message from the
 * prompt.
 *
 * **Example** (Replacing system instructions)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const systemPrompt = Prompt.make([{
 *   role: "system",
 *   content: "You are a helpful assistant."
 * }])
 *
 * const userPrompt = Prompt.make("Hello, world!")
 *
 * const prompt = Prompt.concat(systemPrompt, userPrompt)
 *
 * const replaced = Prompt.setSystem(
 *   prompt,
 *   "You are an expert in programming"
 * )
 * replaced.content[0].content // => "You are an expert in programming"
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const setSystem = /*#__PURE__*/dual(2, (self, content) => {
  const messages = [makeMessage("system", {
    content
  })];
  for (const message of self.content) {
    if (message.role !== "system") {
      messages.push(message);
    }
  }
  return makePrompt(messages);
});
/**
 * Creates a new prompt with a leading system message. If the prompt already has
 * a system message, the new message uses the provided content prepended to the
 * first existing system message's content; the original messages remain after
 * it.
 *
 * **Example** (Prepending system instructions)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const systemPrompt = Prompt.make([{
 *   role: "system",
 *   content: "You are an expert in programming."
 * }])
 *
 * const userPrompt = Prompt.make("Hello, world!")
 *
 * const prompt = Prompt.concat(systemPrompt, userPrompt)
 *
 * const replaced = Prompt.prependSystem(
 *   prompt,
 *   "You are a helpful assistant. "
 * )
 * // result content: "You are a helpful assistant. You are an expert in programming."
 * replaced.content[0].content // => "You are a helpful assistant. You are an expert in programming."
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const prependSystem = /*#__PURE__*/dual(2, (self, content) => {
  let system = undefined;
  for (const message of self.content) {
    if (message.role === "system") {
      system = makeMessage("system", {
        content: content + message.content
      });
      break;
    }
  }
  if (Predicate.isUndefined(system)) {
    system = makeMessage("system", {
      content
    });
  }
  return makePrompt([system, ...self.content]);
});
/**
 * Creates a new prompt with a leading system message. If the prompt already has
 * a system message, the new message uses the provided content appended to the
 * first existing system message's content; the original messages remain after
 * it.
 *
 * **Example** (Appending system instructions)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/ai"
 *
 * const systemPrompt = Prompt.make([{
 *   role: "system",
 *   content: "You are an expert in programming."
 * }])
 *
 * const userPrompt = Prompt.make("Hello, world!")
 *
 * const prompt = Prompt.concat(systemPrompt, userPrompt)
 *
 * const replaced = Prompt.appendSystem(
 *   prompt,
 *   " You are a helpful assistant."
 * )
 * // result content: "You are an expert in programming. You are a helpful assistant."
 * replaced.content[0].content // => "You are an expert in programming. You are a helpful assistant."
 * ```
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const appendSystem = /*#__PURE__*/dual(2, (self, content) => {
  let system = undefined;
  for (const message of self.content) {
    if (message.role === "system") {
      system = makeMessage("system", {
        content: message.content + content
      });
      break;
    }
  }
  if (Predicate.isUndefined(system)) {
    system = makeMessage("system", {
      content
    });
  }
  return makePrompt([system, ...self.content]);
});
//# sourceMappingURL=Prompt.js.map