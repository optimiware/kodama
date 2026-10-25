import * as Effect from "../../Effect.js";
import * as Fiber from "../../Fiber.js";
import { Abandon } from "../ClusterSchema.js";
export const interrupt = /*#__PURE__*/Effect.interruptible(/*#__PURE__*/Effect.callback(() => {
  const fiber = Fiber.getCurrent();
  fiber.interruptUnsafe(fiber.id, Abandon.annotation);
}));
export const isCause = cause => cause.reasons.some(reason => reason._tag === "Interrupt" && reason.annotations.has(Abandon.key));
//# sourceMappingURL=clusterAbandon.js.map