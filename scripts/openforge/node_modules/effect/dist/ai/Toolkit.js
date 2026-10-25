/**
 * Groups AI tools together with their handlers.
 *
 * A toolkit connects `Tool` schemas to the handler functions an application
 * provides for a language model workflow. It can build a handler context or
 * layer and execute tool calls by name. Execution validates parameters, runs the
 * handler, encodes the result, supports preliminary streamed results, and
 * applies the tool's failure mode.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cause from "../Cause.js";
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import * as Effectable from "../Effectable.js";
import * as Fiber from "../Fiber.js";
import { identity } from "../Function.js";
import * as InternalRecord from "../internal/record.js";
import * as Layer from "../Layer.js";
import * as Predicate from "../Predicate.js";
import * as Queue from "../Queue.js";
import * as Result from "../Result.js";
import * as Schema from "../Schema.js";
import * as Stream from "../Stream.js";
import * as AiError from "./AiError.js";
import * as Tool from "./Tool.js";
const TypeId = "~effect/ai/Toolkit";
/**
 * Cause annotation identifying the phase of a typed `Toolkit.handle` failure.
 *
 * **Details**
 *
 * Typed failures raised by `Toolkit.handle` carry one of these origins:
 *
 * - `"parameters"`: the tool call arguments failed to decode
 * - `"handler"`: the tool handler itself failed
 * - `"result"`: the handler's output failed to validate or encode
 *
 * Unannotated causes default to `"result"`, allowing consumers to treat them
 * as internal failures.
 *
 * Returned failures expose the same origin in `Tool.HandlerResult.failureOrigin`.
 *
 * **Example** (Reading a handler failure's origin)
 *
 * ```ts import.meta.vitest
 * import { Cause, Context, Effect, Schema, Stream } from "effect"
 * import { Tool, Toolkit } from "effect/ai"
 *
 * const toolkit = Toolkit.make(Tool.make("Lookup", {
 *   failure: Schema.String,
 *   failureMode: "error"
 * }))
 *
 * const program = Effect.gen(function*() {
 *   const handlers = yield* toolkit
 *   yield* handlers.handle("Lookup", {}).pipe(Effect.flatMap(Stream.runDrain))
 * }).pipe(
 *   Effect.catchCause((cause) =>
 *     Effect.succeed(Context.get(Cause.annotations(cause), Toolkit.FailureOrigin))
 *   ),
 *   Effect.provide(toolkit.toLayer({ Lookup: () => Effect.fail("Not found") }))
 * )
 *
 * await Effect.runPromise(program) // => "handler"
 * ```
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export const FailureOrigin = /*#__PURE__*/Context.Reference("effect/ai/Toolkit/FailureOrigin", {
  defaultValue: () => "result"
});
const failureCause = (error, origin) => Cause.annotate(Cause.fail(error), Context.make(FailureOrigin, origin));
const Proto = {
  ... /*#__PURE__*/Effectable.Prototype({
    label: "Toolkit",
    evaluate: /*#__PURE__*/Effect.fnUntraced(function* (parent) {
      const tools = this.tools;
      const services = parent.context;
      const schemasCache = new WeakMap();
      const getSchemas = tool => {
        let schemas = schemasCache.get(tool);
        if (Predicate.isUndefined(schemas)) {
          const handler = services.mapUnsafe.get(tool.id);
          const decodeParameters = Schema.isSchema(tool.parametersSchema) ? Schema.decodeUnknownEffect(tool.parametersSchema) : u => Effect.succeed(u);
          const encodeSuccess = Schema.encodeUnknownEffect(tool.successSchema);
          const encodeFailure = Schema.encodeUnknownEffect(Tool.failureResultSchema(tool));
          const encodeResult = (u, isFailure) => isFailure ? encodeFailure(u) : encodeSuccess(u);
          schemas = {
            context: handler.context,
            handler: handler.handler,
            decodeParameters,
            encodeResult
          };
          schemasCache.set(tool, schemas);
        }
        return schemas;
      };
      const handle = Effect.fnUntraced(function* (name, params, toolCallId, options) {
        const tool = Object.hasOwn(tools, name) ? tools[name] : undefined;
        yield* Effect.annotateCurrentSpan({
          tool: name,
          parameters: params
        });
        if (Predicate.isUndefined(tool)) {
          return yield* AiError.make({
            module: "Toolkit",
            method: `${name}.handle`,
            reason: new AiError.ToolNotFoundError({
              toolName: name,
              availableTools: Object.keys(tools)
            })
          });
        }
        const schemas = getSchemas(tool);
        const encodeResult = (result, isFailure) => schemas.encodeResult(result, isFailure).pipe(Effect.mapError(cause => AiError.make({
          module: "Toolkit",
          method: `${name}.handle`,
          reason: new AiError.ToolResultEncodingError({
            toolName: name,
            toolResult: result,
            description: cause.message
          })
        })));
        const decodedParamsResult = yield* Effect.result(schemas.decodeParameters(params, options));
        if (Result.isFailure(decodedParamsResult)) {
          const error = AiError.make({
            module: "Toolkit",
            method: `${name}.handle`,
            reason: new AiError.ToolParameterValidationError({
              toolName: name,
              description: decodedParamsResult.failure.message
            })
          });
          if (tool.failureMode === "error") {
            return yield* Effect.failCause(failureCause(error, "parameters"));
          }
          return Stream.fromEffect(Effect.map(encodeResult(error, true), encodedResult => ({
            result: error,
            isFailure: true,
            failureOrigin: "parameters",
            preliminary: false,
            encodedResult
          })));
        }
        const decodedParams = decodedParamsResult.success;
        const queue = yield* Queue.make();
        const context = {
          toolCallId,
          preliminary: result => Effect.asVoid(Queue.offer(queue, {
            result,
            isFailure: false,
            preliminary: true
          }))
        };
        const fiber = yield* schemas.handler(decodedParams, context).pipe(Effect.flatMap(result => Queue.offer(queue, {
          result,
          isFailure: false,
          preliminary: false
        })), Effect.updateContext(input => Context.merge(schemas.context, input)), Effect.matchCauseEffect({
          onFailure: cause => Queue.failCause(queue, cause),
          onSuccess: () => Queue.end(queue)
        }), Effect.forkChild);
        const normalizeError = error => {
          const normalizedError = Schema.isSchemaError(error) ? AiError.make({
            module: "Toolkit",
            method: `${name}.handle`,
            reason: new AiError.InvalidToolResultError({
              toolName: name,
              description: `Tool handler returned invalid result: ${error.message}`
            })
          }) : AiError.isAiErrorReason(error) ? AiError.make({
            module: "Toolkit",
            method: `${name}.handle`,
            reason: error
          }) : error;
          return normalizedError;
        };
        return Stream.fromQueue(queue).pipe(Stream.catch(error => {
          const normalizedError = normalizeError(error);
          const failureOrigin = Schema.isSchemaError(error) ? "result" : "handler";
          return tool.failureMode === "error" ? Stream.failCause(failureCause(normalizedError, failureOrigin)) : Stream.succeed({
            result: normalizedError,
            isFailure: true,
            failureOrigin,
            preliminary: false
          });
        }), Stream.mapEffect(Effect.fnUntraced(function* (output) {
          const encodedResult = yield* encodeResult(output.result, output.isFailure);
          return {
            ...output,
            encodedResult
          };
        })), Stream.onEnd(Fiber.interrupt(fiber)));
      });
      return {
        tools,
        handle: handle
      };
    })
  }),
  [TypeId]: TypeId,
  of: identity,
  toHandlers(build) {
    return Effect.gen({
      self: this
    }, function* () {
      const services = yield* Effect.context();
      const handlers = Effect.isEffect(build) ? yield* build : build;
      const context = new Map();
      for (const [name, handler] of Object.entries(handlers)) {
        const tool = Object.hasOwn(this.tools, name) ? this.tools[name] : undefined;
        if (tool !== undefined) {
          context.set(tool.id, {
            name,
            handler,
            context: services
          });
        }
      }
      return Context.makeUnsafe(context);
    });
  },
  toLayer(build) {
    return Layer.effectContext(this.toHandlers(build));
  },
  toJSON() {
    return {
      _id: "effect/ai/Toolkit",
      tools: Array.from(Object.values(this.tools)).map(tool => tool.name)
    };
  }
};
const makeProto = tools => Object.assign(function () {}, Proto, {
  tools
});
const resolveInput = (...tools) => {
  const output = {};
  for (const tool of tools) {
    InternalRecord.assignProperty(output, tool.name, tool);
  }
  return output;
};
/**
 * An empty toolkit with no tools.
 *
 * **When to use**
 *
 * Use when you need an empty starting point for building toolkits or a default
 * toolkit value that can be extended with `merge`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const empty = /*#__PURE__*/makeProto({});
/**
 * Creates a new toolkit from the specified tools.
 *
 * **Details**
 *
 * This is the primary constructor for creating toolkits. It accepts multiple
 * tools and organizes them into a toolkit that can be provided to AI language
 * models.
 *
 * **Example** (Creating a toolkit)
 *
 * ```ts import.meta.vitest
 * import { Effect, Schema } from "effect"
 * import { Tool, Toolkit } from "effect/ai"
 *
 * const GetCurrentTime = Tool.make("GetCurrentTime", {
 *   description: "Get the current timestamp",
 *   success: Schema.Number
 * })
 *
 * const GetWeather = Tool.make("get_weather", {
 *   description: "Get weather information",
 *   parameters: Schema.Struct({ location: Schema.String }),
 *   success: Schema.Struct({
 *     temperature: Schema.Number,
 *     condition: Schema.String
 *   })
 * })
 *
 * const toolkit = Toolkit.make(GetCurrentTime, GetWeather)
 * const ready = toolkit.pipe(Effect.provide(toolkit.toLayer({
 *   GetCurrentTime: () => Effect.succeed(0),
 *   get_weather: () => Effect.succeed({ temperature: 20, condition: "clear" })
 * })))
 *
 * Object.keys((await Effect.runPromise(ready)).tools) // => ["GetCurrentTime", "get_weather"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = (...tools) => makeProto(resolveInput(...tools));
/**
 * Merges multiple toolkits into a single toolkit.
 *
 * **Details**
 *
 * Combines all tools from the provided toolkits into one unified toolkit.
 * If there are naming conflicts, tools from later toolkits will override
 * tools from earlier ones.
 *
 * **Example** (Merging toolkits)
 *
 * ```ts import.meta.vitest
 * import { Effect, Schema } from "effect"
 * import { Tool, Toolkit } from "effect/ai"
 *
 * const mathToolkit = Toolkit.make(
 *   Tool.make("add", { success: Schema.Number }),
 *   Tool.make("subtract", { success: Schema.Number })
 * )
 *
 * const utilityToolkit = Toolkit.make(
 *   Tool.make("get_time", { success: Schema.Number }),
 *   Tool.make("get_weather", { success: Schema.String })
 * )
 *
 * const combined = Toolkit.merge(mathToolkit, utilityToolkit)
 * const ready = combined.pipe(Effect.provide(combined.toLayer({
 *   add: () => Effect.succeed(1),
 *   subtract: () => Effect.succeed(0),
 *   get_time: () => Effect.succeed(0),
 *   get_weather: () => Effect.succeed("clear")
 * })))
 *
 * Object.keys((await Effect.runPromise(ready)).tools) // => ["add", "subtract", "get_time", "get_weather"]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const merge = (
/**
 * The toolkits to merge together.
 */
...toolkits) => {
  const tools = {};
  for (const toolkit of toolkits) {
    for (const [name, tool] of Object.entries(toolkit.tools)) {
      InternalRecord.assignProperty(tools, name, tool);
    }
  }
  return makeProto(tools);
};
//# sourceMappingURL=Toolkit.js.map