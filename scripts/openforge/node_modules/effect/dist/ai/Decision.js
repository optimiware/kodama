/**
 * Brand for decision definitions.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = "~effect/ai/Decision";
/**
 * Creates a classification decision from labelled criteria.
 * Throws if fewer than two labels are supplied.
 *
 * **Example** (Choosing a department)
 *
 * ```ts
 * import { Decision } from "effect/ai"
 *
 * const department = Decision.classify({
 *   instructions: "Which team should handle this",
 *   criteria: {
 *     billing: "payments",
 *     technical: "bugs",
 *     sales: "pricing"
 *   }
 * })
 * ```
 *
 * @see {@link rate} for an ordered scale
 * @see {@link probability} for a single yes or no likelihood
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const classify = options => {
  if (Object.keys(options.criteria).length < 2) {
    throw new Error("Decision.classify: criteria must contain at least two labels");
  }
  return {
    _tag: "Classify",
    instructions: options.instructions,
    criteria: options.criteria
  };
};
/**
 * Creates a rating decision from an ordered list of criteria.
 * Throws if fewer than two levels or duplicate levels are supplied.
 *
 * **Example** (Rating frustration)
 *
 * ```ts
 * import { Decision } from "effect/ai"
 *
 * const frustration = Decision.rate({
 *   instructions: "How frustrated",
 *   criteria: ["calm", "frustrated", "angry"]
 * })
 * ```
 *
 * @see {@link classify} for unordered labels
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const rate = options => {
  if (options.criteria.length < 2) {
    throw new Error("Decision.rate: criteria must contain at least two levels");
  }
  if (new Set(options.criteria).size !== options.criteria.length) {
    throw new Error("Decision.rate: criteria must contain distinct levels");
  }
  return {
    _tag: "Rate",
    instructions: options.instructions,
    criteria: options.criteria
  };
};
/**
 * Creates a probability decision from instructions and optional outcome descriptions.
 * When `criteria` is supplied, descriptions for both `false` and `true` are required.
 *
 * **Example** (Estimating urgency)
 *
 * ```ts
 * import { Decision } from "effect/ai"
 *
 * const urgent = Decision.probability({
 *   instructions: "The message is time-sensitive"
 * })
 * ```
 *
 * **Example** (Providing outcome descriptions)
 *
 * ```ts
 * import { Decision } from "effect/ai"
 *
 * const urgent = Decision.probability({
 *   instructions: "The message is time-sensitive",
 *   criteria: {
 *     false: "The message can wait",
 *     true: "The message needs immediate attention"
 *   }
 * })
 * ```
 *
 * @see {@link classify} for more than two outcomes
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const probability = options => ({
  _tag: "Probability",
  instructions: options.instructions,
  criteria: options.criteria
});
/**
 * Creates a decision definition from an input schema and named decisions.
 * `DecisionModel.decide` encodes the input with `Schema.toCodecJson` before
 * calling the provider. Answer keys and types are inferred from the decisions.
 * Throws if `decisions` is empty.
 *
 * **Example** (Defining ticket triage)
 *
 * ```ts
 * import { Schema } from "effect"
 * import { Decision } from "effect/ai"
 *
 * const Ticket = Schema.Struct({
 *   subject: Schema.String,
 *   body: Schema.String
 * })
 *
 * const TicketTriage = Decision.make({
 *   input: Ticket,
 *   decisions: {
 *     department: Decision.classify({
 *       instructions: "Which team should handle this",
 *       criteria: { billing: "payments", technical: "bugs" }
 *     }),
 *     urgent: Decision.probability({
 *       instructions: "The message is time-sensitive",
 *       criteria: { false: "No time pressure", true: "Needs action now" }
 *     })
 *   }
 * })
 * ```
 *
 * @see {@link Definition} for the returned shape
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = options => {
  if (Object.keys(options.decisions).length === 0) {
    throw new Error("Decision.make: decisions must not be empty");
  }
  return {
    [TypeId]: TypeId,
    input: options.input,
    decisions: options.decisions
  };
};
//# sourceMappingURL=Decision.js.map