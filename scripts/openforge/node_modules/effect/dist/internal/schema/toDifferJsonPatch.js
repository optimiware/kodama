import * as JsonPatch from "../../JsonPatch.js";
import * as SchemaParser from "../../SchemaParser.js";
import * as InternalToCodec from "./toCodec.js";
/** @internal */
export function toDifferJsonPatch(schema) {
  const serializer = InternalToCodec.toCodecJson(schema);
  const get = SchemaParser.encodeSync(serializer);
  const set = SchemaParser.decodeSync(serializer);
  return {
    empty: [],
    diff: (oldValue, newValue) => JsonPatch.get(get(oldValue), get(newValue)),
    combine: (first, second) => [...first, ...second],
    patch: (oldValue, patch) => {
      const value = get(oldValue);
      const patched = JsonPatch.apply(patch, value);
      return Object.is(patched, value) ? oldValue : set(patched);
    }
  };
}
//# sourceMappingURL=toDifferJsonPatch.js.map