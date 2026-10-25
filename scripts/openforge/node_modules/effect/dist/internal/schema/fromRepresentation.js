import * as Arr from "../../Array.js";
import * as Result from "../../Result.js";
import * as Schema from "../../Schema.js";
import * as SchemaParser from "../../SchemaParser.js";
import { errorWithPath } from "../errors.js";
import * as InternalRecord from "../record.js";
import * as InternalToCodec from "./toCodec.js";
/** @internal */
export function fromRepresentations(document, revivers) {
  return revivePersisted(document.representations, document.references, makeReviverMap(revivers), false);
}
class ReferenceSlot {
  body;
  resolving = false;
  wrapper;
  constructor(key) {
    this.wrapper = Schema.suspend(() => {
      if (this.body === undefined) {
        throw new Error(`Reference ${key} was evaluated before it was resolved`);
      }
      return this.body;
    });
  }
}
function makeReviverMap(revivers) {
  const out = new Map();
  for (let index = 0; index < revivers.length; index++) {
    const reviver = revivers[index];
    if (out.has(reviver.id)) {
      throw errorWithPath(`Duplicate reviver for ${reviver.id}`, ["revivers", index, "id"]);
    }
    out.set(reviver.id, {
      ...reviver,
      payloadSchema: InternalToCodec.toCodecJson(reviver.payloadSchema)
    });
  }
  return out;
}
function revivePersisted(representations, references, reviverMap, singleRoot) {
  const slots = new Map();
  function resolveReference(key, path) {
    let slot = slots.get(key);
    if (slot === undefined) {
      if (!Object.hasOwn(references, key)) {
        throw errorWithPath(`Invalid reference ${key}`, [...path, "$ref"]);
      }
      slot = new ReferenceSlot(key);
      slots.set(key, slot);
    }
    if (slot.body !== undefined) {
      return slot.body;
    }
    if (slot.resolving) {
      return slot.wrapper;
    }
    slot.resolving = true;
    try {
      slot.body = annotate(recur(references[key], ["references", key]), {
        identifier: key
      });
      return slot.body;
    } finally {
      slot.resolving = false;
    }
  }
  function resolveReviver(representation, path) {
    const reviver = reviverMap.get(representation.id);
    if (reviver === undefined) {
      throw errorWithPath(`Missing reviver for ${representation.id}`, path);
    }
    return reviver;
  }
  function decodePayload(representation, reviver, path) {
    const decoded = SchemaParser.decodeUnknownResult(reviver.payloadSchema)(representation.payload);
    if (Result.isFailure(decoded)) {
      throw errorWithPath(`Invalid representation payload for ${representation.id}`, path);
    }
    return decoded.success;
  }
  function reviveSchemas(representations, path) {
    return representations.map((representation, index) => recur(representation, [...path, index]));
  }
  function reviveDeclaration(declaration, path) {
    const representationPath = [...path, "representation"];
    const representation = declaration.representation;
    if (representation === undefined) {
      throw errorWithPath("Missing representation annotation", representationPath);
    }
    const reviver = resolveReviver(representation, representationPath);
    const payload = decodePayload(representation, reviver, [...representationPath, "payload"]);
    const typeParameters = reviveSchemas(declaration.typeParameters, [...path, "typeParameters"]);
    const schema = reviver.revive({
      payload,
      typeParameters,
      annotations: declaration.annotations
    });
    return appendChecks(schema, declaration.checks, [...path, "checks"]);
  }
  function reviveFilter(filter, path) {
    const representationPath = [...path, "representation"];
    const representation = filter.representation;
    if (representation === undefined) {
      throw errorWithPath("Missing representation annotation", representationPath);
    }
    const reviver = resolveReviver(representation, representationPath);
    const payload = decodePayload(representation, reviver, [...representationPath, "payload"]);
    const schemas = reviveSchemas(representation.schemas ?? [], [...representationPath, "schemas"]);
    const check = reviver.revive({
      payload,
      schemas,
      annotations: filter.annotations
    });
    return filter.aborted ? check.abort() : check;
  }
  function reviveFilterGroup(group, path) {
    const representationPath = [...path, "representation"];
    const representation = group.representation;
    if (representation === undefined) {
      const checks = group.checks.map((check, index) => reviveCheck(check, [...path, "checks", index]));
      return Schema.makeFilterGroup(checks, group.annotations);
    }
    const reviver = resolveReviver(representation, representationPath);
    const payload = decodePayload(representation, reviver, [...representationPath, "payload"]);
    const schemas = reviveSchemas(representation.schemas ?? [], [...representationPath, "schemas"]);
    return reviver.revive({
      payload,
      schemas,
      annotations: group.annotations
    });
  }
  function reviveCheck(check, path) {
    return check._tag === "Filter" ? reviveFilter(check, path) : reviveFilterGroup(check, path);
  }
  function appendChecks(schema, checks, path) {
    const revived = checks.map((check, index) => reviveCheck(check, [...path, index]));
    return Arr.isArrayNonEmpty(revived) ? schema.check(...revived) : schema;
  }
  function annotate(schema, annotations) {
    return annotations === undefined ? schema : schema.annotate(annotations);
  }
  function finishStructural(schema, representation, path) {
    return appendChecks(annotate(schema, representation.annotations), representation.checks, [...path, "checks"]);
  }
  function recur(representation, path) {
    switch (representation._tag) {
      case "Reference":
        return resolveReference(representation.$ref, path);
      case "Declaration":
        return reviveDeclaration(representation, path);
      case "Suspend":
        {
          const thunkPath = [...path, "thunk"];
          if (representation.thunk._tag === "Reference") {
            const key = representation.thunk.$ref;
            resolveReference(key, thunkPath);
            const slot = slots.get(key);
            return annotate(slot.wrapper, representation.annotations);
          }
          const thunk = recur(representation.thunk, thunkPath);
          return annotate(Schema.suspend(() => thunk), representation.annotations);
        }
      case "Null":
        return finishStructural(Schema.Null, representation, path);
      case "Undefined":
        return finishStructural(Schema.Undefined, representation, path);
      case "Void":
        return finishStructural(Schema.Void, representation, path);
      case "Never":
        return finishStructural(Schema.Never, representation, path);
      case "Unknown":
        return finishStructural(Schema.Unknown, representation, path);
      case "Any":
        return finishStructural(Schema.Any, representation, path);
      case "String":
        return finishStructural(Schema.String, representation, path);
      case "Number":
        return finishStructural(Schema.Number, representation, path);
      case "Boolean":
        return finishStructural(Schema.Boolean, representation, path);
      case "BigInt":
        return finishStructural(Schema.BigInt, representation, path);
      case "Symbol":
        return finishStructural(Schema.Symbol, representation, path);
      case "Literal":
        return finishStructural(Schema.Literal(representation.literal), representation, path);
      case "UniqueSymbol":
        return finishStructural(Schema.UniqueSymbol(representation.symbol), representation, path);
      case "ObjectKeyword":
        return finishStructural(Schema.ObjectKeyword, representation, path);
      case "Enum":
        return finishStructural(Schema.Enum(Object.fromEntries(representation.enums)), representation, path);
      case "TemplateLiteral":
        {
          const parts = representation.parts.map((part, index) => recur(part, [...path, "parts", index]));
          return finishStructural(Schema.TemplateLiteral(parts), representation, path);
        }
      case "Arrays":
        {
          const elements = representation.elements.map((element, index) => {
            let schema = recur(element.type, [...path, "elements", index, "type"]);
            if (element.annotations !== undefined) {
              schema = schema.annotateKey(element.annotations);
            }
            return element.isOptional ? Schema.optionalKey(schema) : schema;
          });
          const rest = representation.rest.map((item, index) => recur(item, [...path, "rest", index]));
          const schema = Arr.isArrayNonEmpty(rest) ? elements.length === 0 && rest.length === 1 ? Schema.Array(rest[0]) : Schema.TupleWithRest(Schema.Tuple(elements), rest) : Schema.Tuple(elements);
          return finishStructural(schema, representation, path);
        }
      case "Objects":
        {
          const fields = {};
          for (let index = 0; index < representation.propertySignatures.length; index++) {
            const property = representation.propertySignatures[index];
            let schema = recur(property.type, [...path, "propertySignatures", index, "type"]);
            if (property.annotations !== undefined) {
              schema = schema.annotateKey(property.annotations);
            }
            if (property.isOptional) {
              schema = Schema.optionalKey(schema);
            }
            if (property.isMutable) {
              schema = Schema.mutableKey(schema);
            }
            InternalRecord.assignProperty(fields, property.name, schema);
          }
          const records = representation.indexSignatures.map((indexSignature, index) => Schema.Record(recur(indexSignature.parameter, [...path, "indexSignatures", index, "parameter"]), recur(indexSignature.type, [...path, "indexSignatures", index, "type"])));
          const schema = Arr.isArrayNonEmpty(records) ? representation.propertySignatures.length === 0 && records.length === 1 ? records[0] : Schema.StructWithRest(Schema.Struct(fields), records) : Schema.Struct(fields);
          return finishStructural(schema, representation, path);
        }
      case "Union":
        {
          const members = representation.types.map((member, index) => recur(member, [...path, "types", index]));
          return finishStructural(Schema.Union(members, representation.options), representation, path);
        }
    }
  }
  const schemas = representations.map((representation, index) => recur(representation, singleRoot ? ["representation"] : ["representations", index]));
  return schemas;
}
/** @internal */
export function fromRepresentation(document, revivers) {
  return revivePersisted([document.representation], document.references, makeReviverMap(revivers), true)[0];
}
//# sourceMappingURL=fromRepresentation.js.map