import * as Iterable from "../../Iterable.js";
import * as JsonSchema from "../../JsonSchema.js";
import { remainder } from "../../Number.js";
import * as InternalAST from "../../SchemaAST.js";
import { errorWithPath } from "../errors.js";
import * as InternalRecord from "../record.js";
import { fromRepresentation, fromRepresentations } from "./fromRepresentation.js";
const never = {
  _tag: "Never",
  checks: []
};
const unknown = {
  _tag: "Unknown",
  checks: []
};
const string = {
  _tag: "String",
  checks: []
};
const unsupportedStructuredValue = "Only primitive values are supported in \"const\" and \"enum\".";
const jsonSchemaPatternFlags = "u";
function isObject(input) {
  return typeof input === "object" && input !== null && !Array.isArray(input);
}
function makeJsonLiteral(input, path) {
  if (input === null) return {
    _tag: "Null",
    checks: []
  };
  if (typeof input === "string" || typeof input === "number" || typeof input === "boolean") {
    return {
      _tag: "Literal",
      literal: input,
      checks: []
    };
  }
  if (typeof input === "object") throw errorWithPath(unsupportedStructuredValue, path);
}
const jsonSchemaValueTypes = ["null", "string", "number", "boolean", "object", "array"];
const jsonSchemaTypeSpecificKeys = ["minLength", "maxLength", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "properties", "required", "additionalProperties", "patternProperties", "propertyNames", "minProperties", "maxProperties", "items", "prefixItems", "minItems", "maxItems", "uniqueItems"];
function isImportedJsonSchemaType(input) {
  return input === "integer" || typeof input === "string" && jsonSchemaValueTypes.some(type => type === input);
}
function hasTypeSpecificKeywords(schema) {
  return jsonSchemaTypeSpecificKeys.some(key => schema[key] !== undefined);
}
function jsonSchemaFilter(id, payload, schemas) {
  return {
    _tag: "Filter",
    aborted: false,
    representation: {
      id,
      payload,
      ...(schemas === undefined ? undefined : {
        schemas
      })
    }
  };
}
function addNumberCheck(checks, value, id, key) {
  if (typeof value === "number") {
    checks.push(jsonSchemaFilter(id, {
      [key]: value
    }));
  }
}
function jsonSchemaAnnotations(schema) {
  const annotations = {};
  if (typeof schema.title === "string") annotations.title = schema.title;
  if (typeof schema.description === "string") annotations.description = schema.description;
  if (Object.hasOwn(schema, "default")) annotations.default = schema.default;
  if (Array.isArray(schema.examples)) annotations.examples = schema.examples;
  if (typeof schema.readOnly === "boolean") annotations.readOnly = schema.readOnly;
  if (typeof schema.writeOnly === "boolean") annotations.writeOnly = schema.writeOnly;
  if (typeof schema.format === "string") annotations.format = schema.format;
  if (typeof schema.contentEncoding === "string") annotations.contentEncoding = schema.contentEncoding;
  if (typeof schema.contentMediaType === "string") annotations.contentMediaType = schema.contentMediaType;
  if (InternalAST.isJson(schema.contentSchema)) annotations.contentSchema = schema.contentSchema;
  return Object.keys(annotations).length === 0 ? undefined : annotations;
}
function jsonDeclaration(annotations) {
  return {
    _tag: "Declaration",
    representation: {
      id: "effect/schema/Json",
      payload: null
    },
    annotations: {
      ...annotations,
      expected: "JSON value"
    },
    checks: [],
    typeParameters: []
  };
}
function translateJsonSchemaMultiDocument(document, options, singleRoot = false) {
  const definitionCache = new Map();
  const reachableDefinitions = new Map();
  const objectScopesByProperties = new WeakMap();
  const annotatedReferences = [];
  const resolvingChoices = new Set();
  const finalReferences = new Set();
  let inNestedResource = false;
  function finalize(representation) {
    switch (representation._tag) {
      case "Unknown":
        return jsonDeclaration(representation.annotations);
      case "Reference":
        finalReferences.add(representation.$ref);
        return representation;
      case "Suspend":
        return {
          ...representation,
          thunk: finalize(representation.thunk)
        };
      case "Arrays":
        return {
          ...representation,
          elements: representation.elements.map(element => ({
            ...element,
            type: finalize(element.type)
          })),
          rest: representation.rest.map(finalize)
        };
      case "Objects":
        // Intersections may reduce open patterns to fixed properties, so reject only
        // if an open pattern remains in the final representation.
        const [scopes, path] = objectScopesByProperties.get(representation.propertySignatures);
        if (representation.indexSignatures[0]?.parameter === string && scopes.some(scope => scope.patterns.length > 0)) {
          throw errorWithPath("Cannot import open \"patternProperties\": unmatched keys cannot be typed correctly.", path);
        }
        return {
          ...representation,
          propertySignatures: representation.propertySignatures.map(property => ({
            ...property,
            type: finalize(property.type)
          })),
          indexSignatures: representation.indexSignatures.map(indexSignature => ({
            parameter: finalize(indexSignature.parameter),
            type: finalize(indexSignature.type)
          })),
          checks: representation.checks.map(finalizeCheck)
        };
      case "Union":
        return {
          ...representation,
          types: representation.types.map(finalize)
        };
      default:
        return representation;
    }
  }
  function finalizeCheck(check) {
    const representation = check.representation;
    const schemas = representation?.schemas;
    if (representation === undefined || schemas === undefined) {
      return check;
    }
    return {
      ...check,
      representation: {
        ...representation,
        schemas: schemas.map(finalize)
      }
    };
  }
  function annotate(representation, annotations) {
    if (annotations === undefined) return representation;
    if (representation._tag === "Reference") {
      return {
        _tag: "Suspend",
        annotations,
        checks: [],
        thunk: representation
      };
    }
    return {
      ...representation,
      annotations: {
        ...representation.annotations,
        ...annotations
      }
    };
  }
  function translateDefinition(key, path, recursiveReferenceError) {
    const cached = definitionCache.get(key);
    if (cached !== undefined) {
      if (cached === null) {
        throw errorWithPath(recursiveReferenceError ?? `Definition ${JSON.stringify(key)} is a circular alias.`, [...path, "$ref"]);
      }
      return cached;
    }
    definitionCache.set(key, null);
    const representation = translateSchema(document.definitions[key], ["definitions", key]);
    definitionCache.set(key, representation);
    return representation;
  }
  function resolveReference(reference, path, recursiveReferenceError, seen = new Set()) {
    if (seen.has(reference.$ref)) {
      throw errorWithPath(`Definition ${JSON.stringify(reference.$ref)} is a circular alias.`, [...path, "$ref"]);
    }
    seen.add(reference.$ref);
    const representation = translateDefinition(reference.$ref, path, recursiveReferenceError);
    if (representation._tag === "Reference") {
      return resolveReference(representation, path, recursiveReferenceError, seen);
    }
    if (representation._tag === "Suspend" && representation.thunk._tag === "Reference") {
      return annotate(resolveReference(representation.thunk, path, recursiveReferenceError, seen), representation.annotations);
    }
    return representation;
  }
  function mergeAnnotations(left, right) {
    if (left === undefined) return right;
    if (right === undefined) return left;
    return {
      ...left,
      ...right
    };
  }
  function annotateIntersection(representation, left, right) {
    return annotate(representation, mergeAnnotations(left._tag === "Reference" ? undefined : left.annotations, right._tag === "Reference" ? undefined : right.annotations));
  }
  function intersectChecks(left, right, deduplicate = []) {
    let checks = right.checks;
    for (const id of deduplicate) {
      if (left.checks.some(check => check._tag === "Filter" && check.representation?.id === id)) {
        checks = checks.filter(check => check._tag !== "Filter" || check.representation?.id !== id);
      }
    }
    if (checks.length === 0) {
      const annotations = mergeAnnotations(left.annotations, right.annotations);
      return [left.checks, annotations];
    }
    if (right.annotations !== undefined) {
      checks = checks.length === 1 && checks[0].annotations === undefined ? [{
        ...checks[0],
        annotations: right.annotations
      }] : [{
        _tag: "FilterGroup",
        checks: checks,
        annotations: right.annotations
      }];
    }
    return [[...left.checks, ...checks], left.annotations];
  }
  function satisfiesPrimitiveCheck(check, value) {
    if (check._tag === "FilterGroup") {
      return check.checks.every(check => satisfiesPrimitiveCheck(check, value));
    }
    const representation = check.representation;
    const payload = representation.payload;
    switch (representation.id) {
      case "effect/schema/isMinLength":
        return value.length >= payload.minLength;
      case "effect/schema/isMinCodePoints":
        return Iterable.size(value) >= payload.minCodePoints;
      case "effect/schema/isMaxCodePoints":
        return Iterable.size(value) <= payload.maxCodePoints;
      case "effect/schema/isPattern":
        return new RegExp(payload.source, payload.flags).test(value);
      case "effect/schema/isFinite":
        return globalThis.Number.isFinite(value);
      case "effect/schema/isInt":
        return globalThis.Number.isSafeInteger(value);
      case "effect/schema/isMultipleOf":
        return remainder(value, payload.divisor) === 0;
      case "effect/schema/isGreaterThan":
        return value > payload.exclusiveMinimum;
      case "effect/schema/isGreaterThanOrEqualTo":
        return value >= payload.minimum;
      case "effect/schema/isLessThan":
        return value < payload.exclusiveMaximum;
      case "effect/schema/isLessThanOrEqualTo":
        return value <= payload.maximum;
    }
  }
  function satisfiesLiteral(representation, literal) {
    const value = literal.literal;
    if (representation._tag === "String" ? typeof value !== "string" : typeof value !== "number") {
      return false;
    }
    return representation.checks.every(check => satisfiesPrimitiveCheck(check, value));
  }
  function intersectPrimitiveWithLiteral(primitive, literal) {
    const satisfies = primitive._tag === "Boolean" ? typeof literal.literal === "boolean" : satisfiesLiteral(primitive, literal);
    return satisfies ? annotateIntersection(literal, primitive, literal) : never;
  }
  function intersectArrays(left, right, path) {
    const elements = [];
    const length = Math.max(left.elements.length, right.elements.length);
    for (let index = 0; index < length; index++) {
      const leftElement = left.elements[index];
      const rightElement = right.elements[index];
      const isOptional = leftElement?.isOptional !== false && rightElement?.isOptional !== false;
      const leftType = leftElement?.type ?? left.rest[0];
      const rightType = rightElement?.type ?? right.rest[0];
      if (leftType === undefined || rightType === undefined) {
        return isOptional ? {
          elements,
          rest: []
        } : undefined;
      }
      const type = intersect(leftType, rightType, [...path, "elements", index, "type"]);
      if (type._tag === "Never") {
        return isOptional ? {
          elements,
          rest: []
        } : undefined;
      }
      elements.push({
        isOptional,
        type
      });
    }
    const leftRest = left.rest[0];
    const rightRest = right.rest[0];
    if (leftRest === undefined || rightRest === undefined) {
      return {
        elements,
        rest: []
      };
    }
    const rest = intersect(leftRest, rightRest, [...path, "rest", 0]);
    return {
      elements,
      rest: rest._tag === "Never" ? [] : [rest]
    };
  }
  function intersectAll(types, path) {
    let out = types[0] ?? unknown;
    for (let index = 1; index < types.length; index++) {
      out = intersect(out, types[index], [...path, index]);
    }
    return out;
  }
  function lowerObject(scopes, checks, annotations, path) {
    const names = new Set();
    let hasFiniteKeyDomain = false;
    let requiresFiniteKeyDomain = false;
    for (const scope of scopes) {
      for (const name of scope.properties.keys()) names.add(name);
      hasFiniteKeyDomain ||= scope.additionalProperties._tag === "Never" && scope.patterns.length === 0;
      requiresFiniteKeyDomain ||= scope.additionalProperties._tag === "Never" || scope.additionalProperties._tag !== "Unknown" && (scope.hasProperties || scope.patterns.length > 0);
    }
    const closedPattern = scopes.length === 1 && names.size === 0 && scopes[0].additionalProperties._tag === "Never" && scopes[0].patterns.length === 1 ? scopes[0].patterns[0] : undefined;
    if (!hasFiniteKeyDomain && requiresFiniteKeyDomain && closedPattern === undefined) {
      throw errorWithPath(scopes.some(scope => scope.additionalProperties._tag === "Never") ? "Cannot import this closed patterned object: only one pattern without properties is supported." : "Cannot combine typed \"additionalProperties\" with other property schemas: Effect index signatures also check excluded keys.", path);
    }
    const properties = [];
    for (const name of names) {
      let isOptional = true;
      const types = [];
      for (const scope of scopes) {
        const property = scope.properties.get(name);
        if (property !== undefined) {
          if (!property.isOptional) isOptional = false;
          if (property.type !== undefined) types.push(property.type);
        }
        let matches = false;
        for (const pattern of scope.patterns) {
          if (globalThis.RegExp(pattern.source, jsonSchemaPatternFlags).test(name)) {
            types.push(pattern.type);
            matches = true;
          }
        }
        if (property?.type === undefined && !matches) types.push(scope.additionalProperties);
      }
      const type = intersectAll(types, [...path, "properties", name]);
      if (!isOptional && type._tag === "Never") return never;
      properties.push({
        name,
        type,
        isOptional,
        isMutable: false
      });
    }
    const indexSignatures = closedPattern !== undefined ? [{
      parameter: closedPattern.parameter,
      type: closedPattern.type
    }] : !hasFiniteKeyDomain ? [{
      parameter: string,
      type: intersectAll(scopes.map(scope => scope.additionalProperties), [...path, "additionalProperties"])
    }] : properties.length === 0 ? [{
      parameter: string,
      type: never
    }] : [];
    const representation = {
      _tag: "Objects",
      propertySignatures: properties,
      indexSignatures,
      checks,
      annotations
    };
    objectScopesByProperties.set(properties, [scopes, path]);
    return representation;
  }
  const jsonRootTags = ["Null", "String", "Number", "Boolean", "Objects", "Arrays"];
  const allMasks = (1 << jsonRootTags.length) - 1;
  function rootMask(representation) {
    switch (representation._tag) {
      case "Never":
        return 0;
      case "Unknown":
      case "Reference":
        return allMasks;
      case "Literal":
        switch (typeof representation.literal) {
          case "string":
            return 2;
          case "number":
            return 4;
          case "boolean":
            return 8;
          default:
            return 0;
        }
      case "Suspend":
        return rootMask(representation.thunk);
      case "Union":
        {
          let out = 0;
          for (const type of representation.types) {
            out |= rootMask(type);
          }
          return out;
        }
      default:
        return 1 << jsonRootTags.indexOf(representation._tag);
    }
  }
  function hasChoices(representation) {
    switch (representation._tag) {
      case "Reference":
        {
          const key = representation.$ref;
          const cached = definitionCache.get(key);
          if (cached === null || resolvingChoices.has(key)) return true;
          resolvingChoices.add(key);
          const out = hasChoices(cached ?? translateDefinition(key, reachableDefinitions.get(key)));
          resolvingChoices.delete(key);
          return out;
        }
      case "Suspend":
        return hasChoices(representation.thunk);
      case "Union":
        return representation.types.length > 1 || representation.types.some(type => hasChoices(type));
      case "Arrays":
        return representation.elements.some(element => hasChoices(element.type)) || representation.rest.some(type => hasChoices(type));
      case "Objects":
        return representation.propertySignatures.some(property => hasChoices(property.type)) || representation.indexSignatures.some(indexSignature => hasChoices(indexSignature.parameter) || hasChoices(indexSignature.type)) || objectScopesByProperties.get(representation.propertySignatures)[0].some(scope => scope.patterns.some(pattern => hasChoices(pattern.type))) || representation.checks.some(checkHasChoices);
      default:
        return false;
    }
  }
  function checkHasChoices(check) {
    return check._tag === "FilterGroup" ? check.checks.some(checkHasChoices) : check.representation?.schemas?.some(schema => hasChoices(schema)) === true;
  }
  function makeUnion(source, types) {
    if (types.length === 0) return never;
    if (types.length === 1 && source.checks.length === 0) return annotate(types[0], source.annotations);
    return {
      ...source,
      types
    };
  }
  function literalValue(representation) {
    if (representation._tag === "Null") return null;
    if (representation._tag !== "Literal") return undefined;
    const literal = representation.literal;
    return typeof literal === "string" || typeof literal === "number" || typeof literal === "boolean" ? literal : undefined;
  }
  function intersectLiteralUnions(left, right, path) {
    if ((left.options?.mode ?? "anyOf") !== "anyOf" || (right.options?.mode ?? "anyOf") !== "anyOf") {
      return undefined;
    }
    const rightByValue = new Map();
    for (const type of right.types) {
      const representation = type;
      const value = literalValue(representation);
      if (value === undefined) return undefined;
      rightByValue.set(value, representation);
    }
    const types = [];
    for (const type of left.types) {
      const representation = type;
      const value = literalValue(representation);
      if (value === undefined) return undefined;
      const match = rightByValue.get(value);
      if (match !== undefined) types.push(intersect(representation, annotate(match, right.annotations), path));
    }
    return makeUnion(left, types);
  }
  function intersectUnionWithType(union, type, path, unionOnLeft) {
    const mask = rootMask(type);
    const members = union.types.map((member, index) => ({
      index,
      type: member
    })).filter(member => (rootMask(member.type) & mask) !== 0);
    if (members.length > 1 && hasChoices(type)) {
      return unsupportedIntersection(path);
    }
    if (members.length === 1 && union.checks.length === 0) {
      const member = annotate(members[0].type, union.annotations);
      return unionOnLeft ? intersect(member, type, [...path, "types", members[0].index]) : intersect(type, member, [...path, "types", members[0].index]);
    }
    return makeUnion(union, members.map(member => unionOnLeft ? intersect(member.type, type, [...path, "types", member.index]) : intersect(type, member.type, [...path, "types", member.index])).filter(type => type._tag !== "Never"));
  }
  function isTypePartition(union) {
    let mask = 0;
    for (const type of union.types) {
      const memberMask = rootMask(type);
      if ((mask & memberMask) !== 0) return false;
      mask |= memberMask;
    }
    return true;
  }
  function unsupportedIntersection(path) {
    throw errorWithPath("Cannot intersect these \"anyOf\" or \"oneOf\" alternatives without expanding their branches.", path);
  }
  function intersectPartition(partition, other, path, partitionOnLeft) {
    const partitionTypes = partition.types;
    const otherTypes = other.types;
    for (const type of otherTypes) {
      const mask = rootMask(type);
      if (partitionTypes.filter(member => (rootMask(member) & mask) !== 0).length > 1 && hasChoices(type)) {
        return unsupportedIntersection(path);
      }
    }
    const types = [];
    for (let index = 0; index < partitionTypes.length; index++) {
      const member = partitionTypes[index];
      const type = intersectUnionWithType(other, member, [...path, "types", index], !partitionOnLeft);
      if (type._tag !== "Never") types.push(type);
    }
    return makeUnion(partition, types);
  }
  function intersectUnions(left, right, path) {
    const literals = intersectLiteralUnions(left, right, path);
    if (literals !== undefined) return literals;
    if (isTypePartition(left)) return intersectPartition(left, right, path, true);
    if (isTypePartition(right)) return intersectPartition(right, left, path, false);
    return unsupportedIntersection(path);
  }
  function intersect(left, right, path) {
    if (left._tag === "Never") return left;
    if (right._tag === "Never") return right;
    if (left._tag === "Unknown") return annotateIntersection(right, left, right);
    if (right._tag === "Unknown") return annotateIntersection(left, left, right);
    if (left._tag === "Reference") return intersect(resolveReference(left, path), right, path);
    if (right._tag === "Reference") return intersect(left, resolveReference(right, path), path);
    if (left._tag === "Suspend") {
      return annotate(intersect(left.thunk, right, path), left.annotations);
    }
    if (right._tag === "Suspend") {
      return annotate(intersect(left, right.thunk, path), right.annotations);
    }
    if (left._tag === "Union" && right._tag === "Union") return intersectUnions(left, right, path);
    if (left._tag === "Union") return intersectUnionWithType(left, right, path, true);
    if (right._tag === "Union") return intersectUnionWithType(right, left, path, false);
    switch (left._tag) {
      case "Null":
        return right._tag === "Null" ? annotateIntersection({
          _tag: "Null",
          checks: [...left.checks, ...right.checks]
        }, left, right) : never;
      case "String":
        if (right._tag === "Literal") {
          return intersectPrimitiveWithLiteral(left, right);
        }
        if (right._tag !== "String") return never;
        const [stringChecks, stringAnnotations] = intersectChecks(left, right);
        return annotate({
          _tag: "String",
          checks: stringChecks
        }, stringAnnotations);
      case "Number":
        if (right._tag === "Literal") {
          return intersectPrimitiveWithLiteral(left, right);
        }
        if (right._tag !== "Number") return never;
        const [numberChecks, numberAnnotations] = intersectChecks(left, right, ["effect/schema/isFinite", "effect/schema/isInt"]);
        return annotate({
          _tag: "Number",
          checks: numberChecks
        }, numberAnnotations);
      case "Boolean":
        if (right._tag === "Literal") {
          return intersectPrimitiveWithLiteral(left, right);
        }
        return right._tag === "Boolean" ? annotateIntersection({
          _tag: "Boolean",
          checks: [...left.checks, ...right.checks]
        }, left, right) : never;
      case "Literal":
        if (right._tag === "Literal") {
          return left.literal === right.literal ? annotateIntersection({
            ...left,
            checks: [...left.checks, ...right.checks]
          }, left, right) : never;
        }
        if ((right._tag === "String" || right._tag === "Number") && satisfiesLiteral(right, left) || right._tag === "Boolean" && typeof left.literal === "boolean") {
          return annotateIntersection(left, left, right);
        }
        return never;
      case "Arrays":
        {
          if (right._tag !== "Arrays") return never;
          const arrays = intersectArrays(left, right, path);
          if (arrays === undefined) return never;
          const [checks, annotations] = intersectChecks(left, right, ["effect/schema/isUnique"]);
          return annotate({
            _tag: "Arrays",
            ...arrays,
            checks
          }, annotations);
        }
      case "Objects":
        {
          if (right._tag !== "Objects") return never;
          const [checks, annotations] = intersectChecks(left, right);
          const scopes = [...objectScopesByProperties.get(left.propertySignatures)[0], ...objectScopesByProperties.get(right.propertySignatures)[0]];
          return lowerObject(scopes, checks, annotations, path);
        }
    }
  }
  function translateSchema(input, path, isRoot = false) {
    if (input === false) {
      return never;
    }
    if (!isObject(input)) return unknown;
    const schema = options?.onEnter === undefined ? input : options.onEnter(input);
    if (schema === undefined) {
      return unknown;
    }
    if (isObject(schema.not) && Object.keys(schema.not).length === 0) return annotate(never, jsonSchemaAnnotations(schema));
    const wasInNestedResource = inNestedResource;
    inNestedResource ||= !isRoot && typeof schema.$id === "string";
    for (const keyword of Object.keys(schema)) {
      if (keyword === "if" && !Object.hasOwn(schema, "then") && !Object.hasOwn(schema, "else")) continue;
      switch (keyword) {
        case "if":
        case "$dynamicRef":
        case "contains":
        case "dependentRequired":
        case "dependentSchemas":
        case "not":
        case "unevaluatedItems":
        case "unevaluatedProperties":
          throw errorWithPath(`Cannot import JSON Schema keyword "${keyword}": Effect has no equivalent constraint.`, [...path, keyword]);
      }
    }
    let representation = translateType(schema, path);
    if (Object.hasOwn(schema, "const")) {
      const constPath = [...path, "const"];
      const literal = makeJsonLiteral(schema.const, constPath);
      if (literal !== undefined) representation = intersect(representation, literal, constPath);
    }
    if (Array.isArray(schema.enum)) {
      const types = schema.enum.map((value, index) => makeJsonLiteral(value, [...path, "enum", index]) ?? unknown);
      representation = intersect(representation, types.length === 1 ? types[0] : {
        _tag: "Union",
        types,
        checks: []
      }, [...path, "enum"]);
    }
    if (typeof schema.$ref === "string") {
      if (inNestedResource) {
        throw errorWithPath("Cannot resolve $ref under a nested \"$id\". Resolve or flatten it first.", [...path, "$ref"]);
      }
      const $ref = JsonSchema.getReferenceKey(schema.$ref);
      if ($ref === undefined) {
        throw errorWithPath(`Unsupported $ref ${JSON.stringify(schema.$ref)}. Use "#/$defs/Name".`, [...path, "$ref"]);
      }
      if (!Object.hasOwn(document.definitions, $ref)) {
        throw errorWithPath(`Missing definition ${JSON.stringify($ref)} for $ref ${JSON.stringify(schema.$ref)}.`, [...path, "$ref"]);
      }
      if (!reachableDefinitions.has($ref)) reachableDefinitions.set($ref, path);
      const reference = {
        _tag: "Reference",
        $ref
      };
      representation = representation._tag === "Unknown" ? reference : representation._tag === "Never" ? never : intersect(resolveReference(reference, path, `Recursive $ref ${JSON.stringify(reference.$ref)} cannot have sibling constraints.`), representation, path);
    }
    const annotations = jsonSchemaAnnotations(schema);
    if (annotations !== undefined && representation._tag === "Reference") {
      annotatedReferences.push({
        reference: representation,
        path
      });
    }
    representation = annotate(representation, annotations);
    if (Array.isArray(schema.allOf)) {
      for (let index = 0; index < schema.allOf.length; index++) {
        representation = intersect(representation, translateSchema(schema.allOf[index], [...path, "allOf", index]), [...path, "allOf", index]);
      }
    }
    for (const mode of ["anyOf", "oneOf"]) {
      const members = schema[mode];
      if (Array.isArray(members)) {
        const union = {
          _tag: "Union",
          types: members.map((member, index) => translateSchema(member, [...path, mode, index])),
          ...(mode === "oneOf" ? {
            options: {
              mode
            }
          } : {}),
          checks: []
        };
        representation = intersect(union, representation, [...path, mode]);
      }
    }
    inNestedResource = wasInNestedResource;
    return representation;
  }
  function translateType(schema, path) {
    const types = Array.isArray(schema.type) && schema.type.every(isImportedJsonSchemaType) ? schema.type : !isImportedJsonSchemaType(schema.type) && hasTypeSpecificKeywords(schema) ? jsonSchemaValueTypes : undefined;
    if (types !== undefined) {
      return {
        _tag: "Union",
        types: types.map(type => translateType({
          ...schema,
          type
        }, path)),
        checks: []
      };
    }
    const type = isImportedJsonSchemaType(schema.type) ? schema.type : undefined;
    switch (type) {
      case "null":
        return {
          _tag: "Null",
          checks: []
        };
      case "string":
        return {
          _tag: "String",
          checks: collectStringChecks(schema, path)
        };
      case "number":
      case "integer":
        return {
          _tag: "Number",
          checks: [jsonSchemaFilter(type === "number" ? "effect/schema/isFinite" : "effect/schema/isInt", null), ...collectNumberChecks(schema)]
        };
      case "boolean":
        return {
          _tag: "Boolean",
          checks: []
        };
      case "array":
        {
          const prefixItems = Array.isArray(schema.prefixItems) ? schema.prefixItems : undefined;
          const minItems = typeof schema.minItems === "number" ? schema.minItems : 0;
          const elements = prefixItems?.map((element, index) => ({
            isOptional: index + 1 > minItems,
            type: translateSchema(element, [...path, "prefixItems", index])
          })) ?? [];
          const isTupleClosed = schema.items === false || schema.items === undefined && prefixItems !== undefined && schema.maxItems === prefixItems.length;
          const isMaxItemsRedundant = isTupleClosed && typeof schema.maxItems === "number" && schema.maxItems >= elements.length;
          return {
            _tag: "Arrays",
            elements,
            rest: isTupleClosed ? [] : [schema.items === undefined ? unknown : translateSchema(schema.items, [...path, "items"])],
            checks: collectArrayChecks(schema, isMaxItemsRedundant)
          };
        }
      case "object":
        {
          const scope = collectObjectScope(schema, path);
          return lowerObject([scope], collectObjectChecks(schema, path), undefined, path);
        }
      default:
        return unknown;
    }
  }
  function importPatternCheck(pattern, path) {
    switch (options?.patterns ?? "error") {
      case "error":
        throw errorWithPath("Patterns may block validation and are disabled. Use patterns: \"apply\" for trusted schemas or \"ignore\" to discard them.", path);
      case "ignore":
        return undefined;
      case "apply":
        {
          let regex;
          try {
            regex = globalThis.RegExp(pattern, jsonSchemaPatternFlags);
          } catch {
            throw errorWithPath("Cannot import pattern using ECMAScript Unicode mode.", path);
          }
          return jsonSchemaFilter("effect/schema/isPattern", {
            source: regex.source,
            flags: regex.flags
          });
        }
    }
  }
  function collectStringChecks(schema, path) {
    const checks = [];
    if (schema.minLength === 1) {
      addNumberCheck(checks, schema.minLength, "effect/schema/isMinLength", "minLength");
    } else {
      addNumberCheck(checks, schema.minLength, "effect/schema/isMinCodePoints", "minCodePoints");
    }
    addNumberCheck(checks, schema.maxLength, "effect/schema/isMaxCodePoints", "maxCodePoints");
    if (typeof schema.pattern === "string") {
      const check = importPatternCheck(schema.pattern, [...path, "pattern"]);
      if (check !== undefined) checks.push(check);
    }
    return checks;
  }
  function collectNumberChecks(schema) {
    const checks = [];
    addNumberCheck(checks, schema.minimum, "effect/schema/isGreaterThanOrEqualTo", "minimum");
    addNumberCheck(checks, schema.maximum, "effect/schema/isLessThanOrEqualTo", "maximum");
    addNumberCheck(checks, schema.exclusiveMinimum, "effect/schema/isGreaterThan", "exclusiveMinimum");
    addNumberCheck(checks, schema.exclusiveMaximum, "effect/schema/isLessThan", "exclusiveMaximum");
    addNumberCheck(checks, schema.multipleOf, "effect/schema/isMultipleOf", "divisor");
    return checks;
  }
  function collectArrayChecks(schema, isMaxItemsRedundant) {
    const checks = [];
    if (!Array.isArray(schema.prefixItems) || typeof schema.minItems === "number" && schema.minItems > schema.prefixItems.length) {
      addNumberCheck(checks, schema.minItems, "effect/schema/isMinLength", "minLength");
    }
    if (!isMaxItemsRedundant) {
      addNumberCheck(checks, schema.maxItems, "effect/schema/isMaxLength", "maxLength");
    }
    if (schema.uniqueItems === true) {
      checks.push(jsonSchemaFilter("effect/schema/isUnique", null));
    }
    return checks;
  }
  function collectObjectScope(schema, path) {
    const sourceProperties = isObject(schema.properties) ? schema.properties : {};
    const required = new Set(Array.isArray(schema.required) ? schema.required.filter(key => typeof key === "string") : []);
    const propertyNames = Object.keys(sourceProperties);
    const keys = new Set([...propertyNames, ...required]);
    const properties = new Map(Array.from(keys, name => [name, {
      type: Object.hasOwn(sourceProperties, name) ? translateSchema(sourceProperties[name], [...path, "properties", name]) : undefined,
      isOptional: !required.has(name)
    }]));
    const hasProperties = propertyNames.length > 0;
    const patterns = [];
    if (isObject(schema.patternProperties)) {
      for (const [pattern, value] of Object.entries(schema.patternProperties)) {
        const check = importPatternCheck(pattern, [...path, "patternProperties", pattern]);
        if (check === undefined) return {
          properties,
          hasProperties,
          patterns: [],
          additionalProperties: unknown
        };
        patterns.push({
          source: pattern,
          parameter: {
            _tag: "String",
            checks: [check]
          },
          type: translateSchema(value, [...path, "patternProperties", pattern])
        });
      }
    }
    const additionalProperties = schema.additionalProperties === false ? never : isObject(schema.additionalProperties) ? translateSchema(schema.additionalProperties, [...path, "additionalProperties"]) : unknown;
    return {
      properties,
      hasProperties,
      patterns,
      additionalProperties
    };
  }
  function collectObjectChecks(schema, path) {
    const checks = [];
    addNumberCheck(checks, schema.minProperties, "effect/schema/isMinProperties", "minProperties");
    addNumberCheck(checks, schema.maxProperties, "effect/schema/isMaxProperties", "maxProperties");
    if (schema.propertyNames !== undefined) {
      const propertyNamesPath = [...path, "propertyNames"];
      checks.push(jsonSchemaFilter("effect/schema/isPropertyNames", null, [intersect(string, translateSchema(schema.propertyNames, propertyNamesPath), propertyNamesPath)]));
    }
    return checks;
  }
  const references = {};
  const representations = document.schemas.map((schema, index) => finalize(translateSchema(schema, singleRoot ? ["schema"] : ["schemas", index], true)));
  // Definitions expanded away by intersections do not need a standalone output schema.
  for (const key of finalReferences) {
    InternalRecord.assignProperty(references, key, finalize(translateDefinition(key, reachableDefinitions.get(key))));
  }
  for (const {
    reference,
    path
  } of annotatedReferences) {
    resolveReference(reference, path);
  }
  return {
    representations,
    references
  };
}
/** @internal */
export function fromJsonSchemaDocument(document, options, revivers) {
  const translated = translateJsonSchemaMultiDocument({
    dialect: document.dialect,
    schemas: [document.schema],
    definitions: document.definitions
  }, options, true);
  return fromRepresentation({
    representation: translated.representations[0],
    references: translated.references
  }, revivers);
}
/** @internal */
export function fromJsonSchemaMultiDocument(document, options, revivers) {
  return fromRepresentations(translateJsonSchemaMultiDocument(document, options), revivers);
}
//# sourceMappingURL=fromJsonSchemaDocument.js.map