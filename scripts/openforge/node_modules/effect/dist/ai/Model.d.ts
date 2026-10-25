/**
 * Wraps an AI model layer with provider and model metadata.
 *
 * A `Model` can be used anywhere its underlying `Layer` can be used. It also
 * provides the current provider name and model name through the Effect context.
 * This module includes the `Model` interface, the `ProviderName` and
 * `ModelName` service tags, and the `make` constructor. Models can also capture
 * their required services from the current context when they need to be used
 * inside another Effect service.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Context from "../Context.ts";
import * as Effect from "../Effect.ts";
import * as Layer from "../Layer.ts";
declare const TypeId: "~effect/ai/Model";
/**
 * A Model represents a provider-specific AI service.
 *
 * **When to use**
 *
 * Use when you use a Model directly as a Layer to provide a particular model implementation
 * to an Effect program, or use it as an Effect to "lift" dependencies of the
 * Model constructor into the parent Effect when you want to use a Model from
 * within an Effect service.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Model<in out Provider, in out Provides, in out Requires> extends Layer.Layer<Provides | ProviderName | ModelName, never, Requires> {
    readonly [TypeId]: typeof TypeId;
    /**
     * The provider identifier (e.g., "openai", "anthropic", "amazon-bedrock").
     */
    readonly provider: Provider;
    /**
     * Returns a `Layer` with the requirements satisfied, using the current context.
     */
    readonly captureRequirements: Effect.Effect<Layer.Layer<Provides | ProviderName | ModelName>, never, Requires>;
}
declare const ProviderName_base: Context.ServiceClass<ProviderName, "effect/ai/Model/ProviderName", string>;
/**
 * Service tag that provides the current large language model provider name.
 *
 * **Details**
 *
 * This tag is automatically provided by Model instances and can be used to
 * access the name of the provider that is currently in use within a given
 * Effect program.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ProviderName extends ProviderName_base {
}
declare const ModelName_base: Context.ServiceClass<ModelName, "effect/ai/Model/ModelName", string>;
/**
 * Service tag that provides the current large language model name.
 *
 * **Details**
 *
 * This tag is automatically provided by Model instances and can be used to
 * access the name of the model that is currently in use within a given Effect
 * program.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare class ModelName extends ModelName_base {
}
/**
 * Creates a Model from a provider name and a Layer that constructs AI services.
 *
 * **Example** (Providing model metadata)
 *
 * ```ts import.meta.vitest
 * import { Effect, Layer } from "effect"
 * import { Model } from "effect/ai"
 *
 * const model = Model.make("amazon-bedrock", "claude-3-5-haiku", Layer.empty)
 * const program = Effect.gen(function*() {
 *   const provider = yield* Model.ProviderName
 *   const modelName = yield* Model.ModelName
 *   return { provider, modelName }
 * }).pipe(Effect.provide(model))
 *
 * await Effect.runPromise(program) // => { provider: "amazon-bedrock", modelName: "claude-3-5-haiku" }
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <const Provider extends string, const Name extends string, Provides, Requires>(
/**
 * Provider identifier (e.g., "openai", "anthropic", "amazon-bedrock").
 */
provider: Provider, 
/**
 * Model identifier (e.g., "gpt-5", "claude-3-5-haiku").
 */
modelName: Name, 
/**
 * Layer that provides the AI services for this provider.
 */
layer: Layer.Layer<Provides, never, Requires>) => Model<Provider, Provides, Requires>;
export {};
//# sourceMappingURL=Model.d.ts.map