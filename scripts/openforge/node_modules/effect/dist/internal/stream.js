import { identity } from "../Function.js";
import { pipeArguments } from "../Pipeable.js";
const TypeId = "~effect/Stream";
const streamVariance = {
  _R: identity,
  _E: identity,
  _A: identity
};
const Stream = function (channel) {
  this.channel = channel;
};
Stream.prototype = {
  [TypeId]: streamVariance,
  pipe() {
    return pipeArguments(this, arguments);
  }
};
/** @internal */
export const fromChannel = channel => new Stream(channel);
//# sourceMappingURL=stream.js.map