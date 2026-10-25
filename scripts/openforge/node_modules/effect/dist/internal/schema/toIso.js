import * as Optic from "../../Optic.js";
import * as SchemaParser from "../../SchemaParser.js";
import * as InternalToCodec from "./toCodec.js";
/** @internal */
export function toIso(schema) {
  const serializer = InternalToCodec.toCodecIso(schema);
  return Optic.makeIso(SchemaParser.encodeSync(serializer), SchemaParser.decodeSync(serializer));
}
/** @internal */
export function toIsoSource(_) {
  return Optic.id();
}
/** @internal */
export function toIsoFocus(_) {
  return Optic.id();
}
//# sourceMappingURL=toIso.js.map