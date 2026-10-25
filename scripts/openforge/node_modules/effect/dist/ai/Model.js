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
import * as Context from "../Context.js";
import * as Effect from "../Effect.js";
import { identity } from "../Function.js";
import { PipeInspectableProto } from "../internal/core.js";
import * as Layer from "../Layer.js";
const TypeId = "~effect/ai/Model";
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
export class ProviderName extends /*#__PURE__*/Context.Service()("effect/ai/Model/ProviderName") {}
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
export class ModelName extends /*#__PURE__*/Context.Service()("effect/ai/Model/ModelName") {}
const Proto = {
  [TypeId]: TypeId,
  ["~effect/Layer"]: {
    _ROut: identity,
    _E: identity,
    _RIn: identity
  },
  get captureRequirements() {
    const self = this;
    return Effect.contextWith(context => Effect.succeed(Layer.provide(self, Layer.succeedContext(context))));
  },
  ...PipeInspectableProto,
  toJSON() {
    return {
      _id: "effect/ai/Model",
      provider: this.provider
    };
  }
};
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
export const make = (
/**
 * Provider identifier (e.g., "openai", "anthropic", "amazon-bedrock").
 */
provider,
/**
 * Model identifier (e.g., "gpt-5", "claude-3-5-haiku").
 */
modelName,
/**
 * Layer that provides the AI services for this provider.
 */
layer) => Object.assign(Object.create(Proto), {
  provider
}, Layer.merge(layer, Layer.succeedContext(ProviderName.context(provider).pipe(Context.add(ModelName, modelName)))));
//# sourceMappingURL=Model.js.map