import { Effect, JsonSchema, Schema } from "effect";
import { Tool } from "@opencode/schema/tool";
import type { ToolCallPart, ToolDefinition as ToolDefinitionClass, ToolOutput as ToolOutputType } from "./schema/index.js";
import { ToolFailure } from "./schema/index.js";
/**
 * Schema constraint for tool parameters / success values: no decoding or
 * encoding services are allowed. Tools should be self-contained — anything
 * beyond pure data conversion belongs in the handler closure.
 */
export type ToolSchema<T> = Schema.Codec<T, any, never, never>;
export interface ToolExecuteContext {
    readonly id: ToolCallPart["id"];
    readonly name: ToolCallPart["name"];
    readonly namespace?: ToolCallPart["namespace"];
}
export type ToolExecute<Parameters extends ToolSchema<any>, Success extends ToolSchema<any>> = (params: Schema.Schema.Type<Parameters>, context?: ToolExecuteContext) => Effect.Effect<Schema.Schema.Type<Success>, ToolFailure>;
export interface ToolModelOutputInput<Parameters, Output> {
    readonly id: ToolCallPart["id"];
    readonly parameters: Parameters;
    readonly output: Output;
}
export type ToolToModelOutput<Parameters extends ToolSchema<any>, Success extends ToolSchema<any>> = (input: ToolModelOutputInput<Schema.Schema.Type<Parameters>, Success["Encoded"]>) => ReadonlyArray<Tool.Content>;
/**
 * A type-safe LLM tool. Each tool bundles its own description, parameter
 * Schema and success Schema. The execute handler is optional: omit it when you
 * only want to expose a tool schema to the model and handle tool calls outside
 * this package.
 *
 * Errors must be expressed as `ToolFailure`. Unmapped errors and defects fail
 * the stream.
 *
 * Internally each tool also carries memoized codecs and a precomputed
 * `ToolDefinition` so callers do not rebuild them per invocation.
 */
export interface Definition<Parameters extends ToolSchema<any>, Success extends ToolSchema<any>> {
    readonly description: string;
    readonly parameters: Parameters;
    readonly success: Success;
    readonly execute?: ToolExecute<Parameters, Success>;
    readonly toModelOutput?: ToolToModelOutput<Parameters, Success>;
    readonly toStructuredOutput?: (output: Success["Encoded"]) => unknown;
    /** @internal */
    readonly _decode: (input: unknown) => Effect.Effect<Schema.Schema.Type<Parameters>, Schema.SchemaError>;
    /** @internal */
    readonly _encode: (value: Schema.Schema.Type<Success>) => Effect.Effect<unknown, Schema.SchemaError>;
    /** @internal */
    readonly _project: (parameters: Schema.Schema.Type<Parameters>, id: ToolCallPart["id"], output: unknown) => ToolOutputType;
    /** @internal */
    readonly _legacyResult: boolean;
    /** @internal */
    readonly _definition: ToolDefinitionClass;
}
export type AnyTool = Definition<any, any>;
export type ExecutableTool<Parameters extends ToolSchema<any>, Success extends ToolSchema<any>> = Definition<Parameters, Success> & {
    readonly execute: ToolExecute<Parameters, Success>;
};
export type AnyExecutableTool = ExecutableTool<any, any>;
export type ExecutableTools = Record<string, AnyExecutableTool>;
/**
 * Constructs a tool. Two input modes:
 *
 * 1. **Typed** — pass Effect `parameters` and `success` Schemas; inputs and
 *    outputs are statically typed and decoded/encoded automatically.
 *
 *    ```ts
 *    Tool.make({
 *      description: "Get current weather",
 *      parameters: Schema.Struct({ city: Schema.String }),
 *      success: Schema.Struct({ temperature: Schema.Number }),
 *      execute: ({ city }) => Effect.succeed({ temperature: 22 }),
 *    })
 *    ```
 *
 * 2. **Dynamic** — pass raw JSON Schema as `jsonSchema`. Use this when the
 *    schema comes from an external source (MCP server, plugin manifest,
 *    dynamic config) and is not known at compile time. Inputs are typed as
 *    `unknown`; the handler is responsible for any validation it needs.
 *
 *    ```ts
 *    Tool.make({
 *      description: "Look something up",
 *      jsonSchema: { type: "object", properties: { ... } },
 *      execute: (params) => Effect.succeed(...),
 *    })
 *    ```
 *
 * In both modes the produced tool flows through `toDefinitions(...)`
 * identically.
 */
export declare function make<Parameters extends ToolSchema<any>, Success extends ToolSchema<any>>(config: {
    readonly description: string;
    readonly parameters: Parameters;
    readonly success: Success;
    readonly execute: ToolExecute<Parameters, Success>;
    readonly toModelOutput?: ToolToModelOutput<Parameters, Success>;
    readonly toStructuredOutput?: (output: Success["Encoded"]) => unknown;
}): ExecutableTool<Parameters, Success>;
export declare function make<Parameters extends ToolSchema<any>, Success extends ToolSchema<any>>(config: {
    readonly description: string;
    readonly parameters: Parameters;
    readonly success: Success;
    readonly execute?: undefined;
    readonly toModelOutput?: ToolToModelOutput<Parameters, Success>;
    readonly toStructuredOutput?: (output: Success["Encoded"]) => unknown;
}): Definition<Parameters, Success>;
export declare function make(config: {
    readonly description: string;
    readonly jsonSchema: JsonSchema.JsonSchema;
    readonly outputSchema?: JsonSchema.JsonSchema;
    readonly execute: (params: unknown, context?: ToolExecuteContext) => Effect.Effect<unknown, ToolFailure>;
    readonly toModelOutput?: (input: ToolModelOutputInput<unknown, unknown>) => ReadonlyArray<Tool.Content>;
    readonly toStructuredOutput?: (output: unknown) => unknown;
}): AnyExecutableTool;
export declare function make(config: {
    readonly description: string;
    readonly jsonSchema: JsonSchema.JsonSchema;
    readonly outputSchema?: JsonSchema.JsonSchema;
    readonly execute?: undefined;
    readonly toModelOutput?: (input: ToolModelOutputInput<unknown, unknown>) => ReadonlyArray<Tool.Content>;
    readonly toStructuredOutput?: (output: unknown) => unknown;
}): AnyTool;
/**
 * A record of named tools. The record key becomes the tool name on the wire.
 */
export type Tools = Record<string, AnyTool>;
/**
 * Convert a tools record into the `ToolDefinition[]` shape that
 * `LLMRequest.tools` expects.
 *
 * Tool names come from the record keys, so the per-tool cached
 * `_definition` is rebuilt with the correct name here. The JSON Schema body
 * is reused.
 */
export declare const toDefinitions: (tools: Tools) => ReadonlyArray<ToolDefinitionClass>;
/**
 * Describes a no-argument Effect tool input as an empty object, or returns
 * `undefined` when the input takes arguments.
 *
 * Effect emits `{ not: { type: "null" } }` only for an encoded struct with no
 * properties or index signatures, but providers require tool parameters to
 * describe an object. Annotations are kept; any other keyword, such as a
 * check's `minProperties`, leaves the schema as Effect emitted it.
 */
export declare const emptyInputJsonSchema: (schema: Schema.Top) => JsonSchema.JsonSchema | undefined;
export { ToolFailure };
export * as Tool from "./tool.js";
