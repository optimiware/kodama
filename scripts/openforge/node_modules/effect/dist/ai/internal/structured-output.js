import * as Arr from "../../Array.js";
import * as InternalRecord from "../../internal/record.js";
import * as JsonSchema from "../../JsonSchema.js";
import * as Option from "../../Option.js";
import * as Predicate from "../../Predicate.js";
import * as Schema from "../../Schema.js";
import * as SchemaAST from "../../SchemaAST.js";
import * as SchemaTransformation from "../../SchemaTransformation.js";
import * as Tool from "../Tool.js";
const REST_PROPERTY_NAME = "__rest__";
const TAIL_PROPERTY_PREFIX = "__tail_";
const RECORD_DESCRIPTION = "Object encoded as array of [key, value] pairs. Apply object constraints to the decoded object";
const TUPLE_DESCRIPTION = "Tuple encoded as an object with numeric string keys ('0', '1', ...). If present, '__rest__' contains remaining elements";
const TUPLE_TAIL_DESCRIPTION = `${TUPLE_DESCRIPTION}. Post-rest elements use '__tail_0__', '__tail_1__', and so on`;
/**
 * @internal
 */
export function resolveReference($ref, definitions) {
  const key = JsonSchema.getReferenceKey($ref);
  if (key === undefined) {
    throw new Error(`Unsupported reference ${JSON.stringify($ref)}`);
  }
  if (!Object.hasOwn(definitions, key)) {
    throw new Error(`Invalid reference ${JSON.stringify($ref)}`);
  }
  return definitions[key];
}
/**
 * @internal
 */
export function resolveTopLevelReference(document) {
  if (typeof document.schema.$ref !== "string") return document;
  return {
    ...document,
    schema: resolveReference(document.schema.$ref, document.definitions)
  };
}
/**
 * @internal
 */
export function toCodec(schema) {
  const jsonCodec = Schema.toCodecJson(schema);
  const encoded = SchemaAST.toEncoded(jsonCodec.ast);
  const from = transform(encoded);
  if (from === encoded) {
    return jsonCodec.ast === schema.ast ? schema : jsonCodec;
  }
  return Schema.make(SchemaAST.decodeTo(from, jsonCodec.ast, SchemaTransformation.passthrough()));
}
function transform(root) {
  let cache;
  function recur(ast) {
    switch (ast._tag) {
      case "Union":
        {
          const types = SchemaAST.mapOrSame(ast.types, recur);
          const checks = prepareChecks(ast.checks);
          const options = ast.options?.mode === "oneOf" ? {
            ...ast.options,
            mode: "anyOf"
          } : ast.options;
          if (types === ast.types && checks === ast.checks && options === ast.options) return ast;
          return new SchemaAST.Union(types, options, ast.annotations, checks, ast.encoding, ast.context, ast.encodingChecks);
        }
      case "Arrays":
        {
          if (ast.elements.length > 0 || ast.rest.length > 1) {
            return tupleToObject(ast, recur);
          }
          const rest = SchemaAST.mapOrSame(ast.rest, recur);
          const checks = prepareChecks(ast.checks);
          if (rest === ast.rest && checks === ast.checks) return ast;
          return new SchemaAST.Arrays(ast.isMutable, ast.elements, rest, ast.annotations, checks, ast.encoding, ast.context, ast.encodingChecks);
        }
      case "Objects":
        {
          if (ast.indexSignatures.length === 1 && ast.propertySignatures.length === 0) {
            const indexSignature = ast.indexSignatures[0];
            if (Tool.isEmptyParamsRecord(indexSignature)) return ast;
          }
          if (ast.indexSignatures.length > 0) {
            return objectToEntries(ast, recur);
          }
          const propertySignatures = SchemaAST.mapOrSame(ast.propertySignatures, propertySignature => {
            let type = recur(propertySignature.type);
            if (SchemaAST.isOptional(propertySignature.type)) {
              type = optionalToNullable(type);
            }
            return type === propertySignature.type ? propertySignature : new SchemaAST.PropertySignature(propertySignature.name, type);
          });
          const checks = prepareChecks(ast.checks);
          if (propertySignatures === ast.propertySignatures && checks === ast.checks) {
            return ast;
          }
          return new SchemaAST.Objects(propertySignatures, ast.indexSignatures, ast.annotations, checks, ast.encoding, ast.context, ast.encodingChecks);
        }
      case "Suspend":
        {
          const cached = cache?.get(ast);
          if (cached !== undefined) return cached;
          const out = new SchemaAST.Suspend(() => recur(ast.thunk()), ast.annotations, undefined, ast.encoding, ast.context);
          if (cache === undefined) cache = new Map();
          cache.set(ast, out);
          return out;
        }
      default:
        {
          const checks = prepareChecks(ast.checks);
          return checks === ast.checks ? ast : SchemaAST.replaceChecks(ast, checks);
        }
    }
  }
  return recur(root);
}
function tupleToObject(ast, recur) {
  const propertySignatures = ast.elements.map((element, index) => new SchemaAST.PropertySignature(String(index), element));
  if (ast.rest.length === 1) {
    propertySignatures.push(new SchemaAST.PropertySignature(REST_PROPERTY_NAME, new SchemaAST.Arrays(false, [], ast.rest)));
  } else if (ast.rest.length > 1) {
    propertySignatures.push(new SchemaAST.PropertySignature(REST_PROPERTY_NAME, new SchemaAST.Arrays(false, [], [ast.rest[0]])));
    for (let index = 1; index < ast.rest.length; index++) {
      propertySignatures.push(new SchemaAST.PropertySignature(`${TAIL_PROPERTY_PREFIX}${index - 1}__`, ast.rest[index]));
    }
  }
  const from = recur(new SchemaAST.Objects(propertySignatures, [], structuralAnnotations(ast, ast.rest.length > 1 ? TUPLE_TAIL_DESCRIPTION : TUPLE_DESCRIPTION), compilerChecks(ast.checks)));
  return SchemaAST.decodeTo(from, ast, SchemaTransformation.transform({
    decode: object => {
      const tuple = [];
      for (let index = 0; index < ast.elements.length; index++) {
        const key = String(index);
        if (object[key] !== undefined) tuple.push(object[key]);
      }
      if (REST_PROPERTY_NAME in object) {
        const rest = object[REST_PROPERTY_NAME];
        for (let index = 0; index < rest.length; index++) {
          tuple.push(rest[index]);
        }
      }
      for (let index = 1; index < ast.rest.length; index++) {
        tuple.push(object[`${TAIL_PROPERTY_PREFIX}${index - 1}__`]);
      }
      return tuple;
    },
    encode: tuple => {
      const object = {};
      for (let index = 0; index < ast.elements.length; index++) {
        if (index < tuple.length) object[String(index)] = tuple[index];
      }
      if (ast.rest.length >= 1) {
        const tailLength = ast.rest.length - 1;
        const restEnd = Math.max(ast.elements.length, tuple.length - tailLength);
        object[REST_PROPERTY_NAME] = tuple.slice(ast.elements.length, restEnd);
        for (let index = 0; index < tailLength; index++) {
          object[`${TAIL_PROPERTY_PREFIX}${index}__`] = tuple[restEnd + index];
        }
      }
      return object;
    }
  }));
}
function objectToEntries(ast, recur) {
  const checks = combineChecks(recordChecks(ast.checks), compilerChecks(ast.checks));
  const key = unionOrSingle([...ast.propertySignatures.map(propertySignature => new SchemaAST.Literal(propertySignature.name)), ...ast.indexSignatures.map(indexSignature => indexSignature.parameter)]);
  const value = unionOrSingle([...ast.propertySignatures.map(propertySignature => propertySignature.type), ...ast.indexSignatures.map(indexSignature => indexSignature.type)]);
  const from = recur(new SchemaAST.Arrays(false, [], [new SchemaAST.Arrays(false, [key, value], [])], structuralAnnotations(ast, RECORD_DESCRIPTION), checks));
  return SchemaAST.decodeTo(from, ast, SchemaTransformation.transform({
    decode: Object.fromEntries,
    encode: Object.entries
  }));
}
function unionOrSingle(types) {
  if (types.length === 1) return types[0];
  const unique = Array.from(new Set(types));
  return unique.length === 1 ? unique[0] : new SchemaAST.Union(unique);
}
function combineChecks(left, right) {
  if (left === undefined) return right;
  if (right === undefined) return left;
  return [...left, ...right];
}
function compilerChecks(checks) {
  if (checks === undefined) return undefined;
  const out = checks.flatMap(compilerCheck);
  return Arr.isArrayNonEmpty(out) ? out : undefined;
}
function compilerCheck(check) {
  // Structural rewrites change the value seen by provider-side checks. A
  // no-op proxy lets the generic compiler call `toJsonSchema` with the real
  // provider type and dependency schemas without running the original filter
  // against the wrong runtime shape.
  const annotations = compilerAnnotations(check.annotations);
  if (check._tag === "Filter") {
    return annotations === undefined ? [] : [new SchemaAST.Filter(() => undefined, annotations)];
  }
  const checks = check.checks.flatMap(compilerCheck);
  if (Arr.isArrayNonEmpty(checks)) {
    return [new SchemaAST.FilterGroup(checks, annotations)];
  }
  return annotations === undefined ? [] : [new SchemaAST.Filter(() => undefined, annotations)];
}
function compilerAnnotations(annotations) {
  if (annotations?.toJsonSchema === undefined) return undefined;
  return {
    representation: annotations.representation,
    toJsonSchema: annotations.toJsonSchema
  };
}
function optionalToNullable(type) {
  return SchemaAST.decodeTo(new SchemaAST.Union([type, SchemaAST.null]), SchemaAST.optionalKey(type), SchemaTransformation.transformOptional({
    decode: Option.filter(Predicate.isNotNull),
    encode: Option.orElseSome(() => null)
  }));
}
function prepareChecks(checks) {
  if (checks === undefined) return undefined;
  const out = SchemaAST.mapOrSame(checks, prepareCheck);
  return out;
}
function prepareCheck(check) {
  switch (check._tag) {
    case "Filter":
      {
        const id = check.annotations?.representation?.id;
        return id === "effect/schema/isFinite" || id === "effect/schema/isInt" ? check.annotate({
          expected: undefined
        }) : check;
      }
    case "FilterGroup":
      {
        const checks = SchemaAST.mapOrSame(check.checks, prepareCheck);
        return checks === check.checks ? check : new SchemaAST.FilterGroup(checks, check.annotations);
      }
  }
}
function recordChecks(checks) {
  if (checks === undefined) return undefined;
  const out = checks.flatMap(recordCheck);
  return Arr.isArrayNonEmpty(out) ? out : undefined;
}
function recordCheck(check) {
  if (check._tag === "FilterGroup") return check.checks.flatMap(recordCheck);
  const representation = check.annotations?.representation;
  const payload = representation?.payload;
  if (!isJsonObject(payload)) return [];
  // A decoded record with at least N properties necessarily came from at least
  // N entries. Upper bounds are not transported because duplicate provider
  // keys can collapse during `Object.fromEntries`, so `maxItems` could reject
  // an input that the returned codec accepts.
  switch (representation?.id) {
    case "effect/schema/isMinProperties":
      return typeof payload.minProperties === "number" ? [withoutDescription(Schema.isMinLength(payload.minProperties))] : [];
    case "effect/schema/isBetweenProperties":
      return typeof payload.minimum === "number" ? [withoutDescription(Schema.isMinLength(payload.minimum))] : [];
    default:
      return [];
  }
}
function isJsonObject(input) {
  return typeof input === "object" && input !== null && !Array.isArray(input);
}
function withoutDescription(check) {
  return check.annotate({
    description: undefined,
    expected: undefined,
    title: undefined
  });
}
function structuralAnnotations(ast, structuralDescription) {
  const descriptions = [];
  appendAnnotationDescription(descriptions, ast.annotations);
  if (ast.checks !== undefined) {
    for (const check of ast.checks) appendCheckDescriptions(descriptions, check);
  }
  return {
    description: descriptions.length === 0 ? structuralDescription : `${structuralDescription}; ${descriptions.join(" and ")}`
  };
}
function appendCheckDescriptions(descriptions, check) {
  appendAnnotationDescription(descriptions, check.annotations);
  if (check._tag === "FilterGroup") {
    for (const child of check.checks) appendCheckDescriptions(descriptions, child);
  }
}
function appendAnnotationDescription(descriptions, annotations) {
  const description = annotations?.description ?? annotations?.expected;
  if (typeof description === "string" && !descriptions.includes(description)) {
    descriptions.push(description);
  }
}
/**
 * @internal
 */
export function walkJsonSchema(schema, visitor) {
  const out = {};
  for (const [key, value] of Object.entries(schema)) {
    switch (key) {
      case "properties":
      case "patternProperties":
        {
          if (isJsonSchema(value)) {
            const properties = {};
            for (const [name, property] of Object.entries(value)) {
              InternalRecord.assignProperty(properties, name, isJsonSchema(property) ? walkJsonSchema(property, visitor) : property);
            }
            InternalRecord.assignProperty(out, key, properties);
          } else {
            InternalRecord.assignProperty(out, key, value);
          }
          break;
        }
      case "additionalProperties":
      case "items":
      case "propertyNames":
        {
          InternalRecord.assignProperty(out, key, isJsonSchema(value) ? walkJsonSchema(value, visitor) : value);
          break;
        }
      case "prefixItems":
      case "allOf":
      case "anyOf":
      case "oneOf":
        {
          InternalRecord.assignProperty(out, key, Array.isArray(value) ? value.map(member => isJsonSchema(member) ? walkJsonSchema(member, visitor) : member) : value);
          break;
        }
      default:
        InternalRecord.assignProperty(out, key, value);
        break;
    }
  }
  return visitor(out);
}
/**
 * @internal
 */
export function appendDescription(schema, description) {
  if (typeof schema.description === "string") {
    const descriptions = schema.description.split(" and ");
    if (!descriptions.includes(description)) schema.description += ` and ${description}`;
  } else {
    schema.description = description;
  }
}
/**
 * @internal
 */
export function isJsonSchema(input) {
  return typeof input === "object" && input !== null && !Array.isArray(input);
}
//# sourceMappingURL=structured-output.js.map