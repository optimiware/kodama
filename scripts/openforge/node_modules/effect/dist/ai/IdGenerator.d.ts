/**
 * Provides identifier generation for AI features.
 *
 * The `IdGenerator` service exposes one operation, `generateId`, which returns
 * a string inside `Effect`. AI modules use it for values such as tool call ids
 * and generated response item ids. This module includes the service tag,
 * service interface, default generator, configurable custom generator, and layer
 * for providing the service.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Cause from "../Cause.ts";
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
declare const IdGenerator_base: Context.ServiceClass<IdGenerator, "@effect/ai/IdGenerator", Service>;
/**
 * Service tag for AI identifier generation services.
 *
 * **When to use**
 *
 * Use to access or provide the service that creates identifiers for AI tool
 * calls and related generated values.
 *
 * **Details**
 *
 * This tag is used to provide and access ID generation functionality throughout
 * the application. It follows Effect's standard service pattern for type-safe
 * dependency injection.
 *
 * **Example** (Accessing the ID generator service)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { IdGenerator } from "effect/ai"
 *
 * const useIdGenerator = Effect.gen(function*() {
 *   const idGenerator = yield* IdGenerator.IdGenerator
 *   const newId = yield* idGenerator.generateId()
 *   return newId
 * })
 *
 * const program = useIdGenerator.pipe(
 *   Effect.provideService(IdGenerator.IdGenerator, {
 *     generateId: () => Effect.succeed("id-1")
 *   })
 * )
 * await Effect.runPromise(program) // => "id-1"
 * ```
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class IdGenerator extends IdGenerator_base {
}
/**
 * The service interface for ID generation.
 *
 * **Details**
 *
 * Defines the contract that all ID generator implementations must fulfill.
 * The service provides a single method for generating unique identifiers
 * in an effectful context.
 *
 * **Example** (Implementing a custom ID generator)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import type { IdGenerator } from "effect/ai"
 *
 * // Custom deterministic implementation
 * let nextId = 0
 * const customService: IdGenerator.Service = {
 *   generateId: () => Effect.sync(() => `custom_${++nextId}`)
 * }
 *
 * const program = customService.generateId()
 *
 * await Effect.runPromise(program) // => "custom_1"
 * ```
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Service {
    readonly generateId: () => Effect.Effect<string>;
}
/**
 * Configuration options for creating custom ID generators.
 *
 * **Example** (Configuring generated IDs)
 *
 * ```ts import.meta.vitest
 * import type { IdGenerator } from "effect/ai"
 *
 * // Configuration for tool call IDs
 * const toolCallOptions: IdGenerator.MakeOptions = {
 *   alphabet: "0123456789ABCDEF",
 *   prefix: "tool",
 *   separator: "_",
 *   size: 8
 * }
 *
 * // This will generate IDs like: "tool_A1B2C3D4"
 * const result = [toolCallOptions.prefix, toolCallOptions.size] // => ["tool", 8]
 * ```
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface MakeOptions {
    /**
     * The character set to use for generating the random portion of IDs.
     */
    readonly alphabet: string;
    /**
     * Optional prefix to prepend to generated IDs.
     */
    readonly prefix?: string | undefined;
    /**
     * Character used to separate the prefix from the random portion.
     */
    readonly separator: string;
    /**
     * Length of the random portion of the generated ID.
     */
    readonly size: number;
}
/**
 * Default ID generator service implementation.
 *
 * **Details**
 *
 * Uses the standard configuration with "id" prefix and generates IDs in the
 * format "id_XXXXXXXXXXXXXXXX" where X represents random alphanumeric
 * characters.
 *
 * **Example** (Generating default IDs)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { IdGenerator } from "effect/ai"
 *
 * const program = Effect.gen(function*() {
 *   const id = yield* IdGenerator.defaultIdGenerator.generateId()
 *   return id
 * })
 *
 * // Or provide it as a service
 * const withDefault = program.pipe(
 *   Effect.provideService(
 *     IdGenerator.IdGenerator,
 *     IdGenerator.defaultIdGenerator
 *   )
 * )
 *
 * const id = await Effect.runPromise(withDefault)
 * const result = [id.startsWith("id_"), id.length] // => [true, 19]
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const defaultIdGenerator: Service;
/**
 * Creates a custom ID generator service with the specified options.
 *
 * **Details**
 *
 * Validates the configuration to ensure the separator is not part of the
 * alphabet, which would cause ambiguity in parsing generated IDs.
 *
 * **Example** (Creating a custom generator)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { IdGenerator } from "effect/ai"
 *
 * const program = Effect.gen(function*() {
 *   // Create a generator for AI assistant message IDs
 *   const messageIdGen = yield* IdGenerator.make({
 *     alphabet: "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ",
 *     prefix: "msg",
 *     separator: "-",
 *     size: 10
 *   })
 *
 *   return yield* messageIdGen.generateId()
 * })
 *
 * const messageId = await Effect.runPromise(program)
 * const result = [messageId.startsWith("msg-"), messageId.length] // => [true, 14]
 * ```
 *
 * **Example** (Handling invalid generator options)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { IdGenerator } from "effect/ai"
 *
 * // This will fail with IllegalArgumentError
 * const invalidConfig = IdGenerator.make({
 *   alphabet: "ABC123",
 *   prefix: "test",
 *   separator: "A", // Error: separator is part of alphabet
 *   size: 8
 * })
 *
 * const error = await Effect.runPromise(Effect.flip(invalidConfig))
 * error.message // => 'The separator "A" must not be part of the alphabet "ABC123".'
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (args_0: MakeOptions) => Effect.Effect<{
    readonly generateId: () => Effect.Effect<string, never, never>;
}, Cause.IllegalArgumentError, never>;
/**
 * Creates a Layer that provides the IdGenerator service with custom
 * configuration.
 *
 * **When to use**
 *
 * Use when you need to provide ID generation capabilities from validated
 * configuration.
 *
 * **Example** (Providing an ID generator layer)
 *
 * ```ts import.meta.vitest
 * import { Effect } from "effect"
 * import { IdGenerator } from "effect/ai"
 *
 * // Create a layer for generating AI tool call IDs
 * const toolCallIdLayer = IdGenerator.layer({
 *   alphabet: "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ",
 *   prefix: "tool_call",
 *   separator: "_",
 *   size: 12
 * })
 *
 * const program = Effect.gen(function*() {
 *   const idGen = yield* IdGenerator.IdGenerator
 *   return yield* idGen.generateId()
 * }).pipe(Effect.provide(toolCallIdLayer))
 *
 * const toolCallId = await Effect.runPromise(program)
 * const result = [toolCallId.startsWith("tool_call_"), toolCallId.length] // => [true, 22]
 * ```
 *
 * @stability unstable
 * @category layers
 * @since 4.0.0
 */
export declare const layer: (options: MakeOptions) => Layer.Layer<IdGenerator, Cause.IllegalArgumentError>;
export {};
//# sourceMappingURL=IdGenerator.d.ts.map