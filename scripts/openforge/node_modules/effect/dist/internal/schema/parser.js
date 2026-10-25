import * as Exit from "../../Exit.js";
import * as Option from "../../Option.js";
import { args } from "../core.js";
/** @internal */
export const missing = /*#__PURE__*/Symbol();
/** @internal */
export { args };
/** @internal */
export const succeed = Exit.succeed;
/** @internal */
export const missingExit = /*#__PURE__*/succeed(missing);
// Shared success for a present input returned unchanged. It must be resolved
// before crossing an asynchronous or transformation boundary.
/** @internal */
export const sameExit = /*#__PURE__*/succeed(missing);
/** @internal */
export const toOption = value => value === missing ? Option.none() : Option.some(value);
/** @internal */
export const fromOptionExit = option => option._tag === "None" ? missingExit : succeed(option.value);
//# sourceMappingURL=parser.js.map