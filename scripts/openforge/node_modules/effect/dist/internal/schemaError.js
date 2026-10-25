import * as Predicate from "../Predicate.js";
export const SchemaErrorTypeId = "~effect/Schema/SchemaError";
export function isSchemaError(u) {
  return Predicate.hasProperty(u, SchemaErrorTypeId) && u[SchemaErrorTypeId] === SchemaErrorTypeId;
}
//# sourceMappingURL=schemaError.js.map