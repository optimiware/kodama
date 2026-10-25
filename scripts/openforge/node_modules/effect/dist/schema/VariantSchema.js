import * as Effect from "../Effect.js";
import { dual } from "../Function.js";
import * as InternalRecord from "../internal/record.js";
import { pipeArguments } from "../Pipeable.js";
import * as Predicate from "../Predicate.js";
import * as Schema from "../Schema.js";
import * as Struct_ from "../Struct.js";
/**
 * Runtime type identifier attached to variant schema structs.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export const TypeId = "~effect/schema/VariantSchema";
const cacheSymbol = /*#__PURE__*/Symbol.for(`${TypeId}/cache`);
const defaultCacheSymbol = /*#__PURE__*/Symbol.for(`${TypeId}/defaultCache`);
/**
 * Returns `true` when a value is a variant schema struct.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isStruct = u => Predicate.hasProperty(u, TypeId);
const FieldTypeId = "~effect/schema/VariantSchema/Field";
/**
 * Returns `true` when a value is a variant schema field.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isField = u => Predicate.hasProperty(u, FieldTypeId);
const extract = /*#__PURE__*/dual(args => isStruct(args[0]), (self, variant, options) => {
  if (options?.isDefault === true && Schema.isSchema(self)) {
    return self;
  }
  const cache = options?.isDefault === true ? self[defaultCacheSymbol] ?? (self[defaultCacheSymbol] = Object.create(null)) : self[cacheSymbol] ?? (self[cacheSymbol] = Object.create(null));
  if (Object.hasOwn(cache, variant)) {
    return cache[variant];
  }
  const fields = {};
  for (const key of Object.keys(self[TypeId])) {
    const value = self[TypeId][key];
    if (value === undefined) {
      continue;
    }
    if (TypeId in value) {
      if (options?.isDefault === true && Schema.isSchema(value)) {
        InternalRecord.assignProperty(fields, key, value);
      } else {
        InternalRecord.assignProperty(fields, key, extract(value, variant));
      }
    } else if (FieldTypeId in value) {
      if (Object.hasOwn(value.schemas, variant)) {
        const schema = value.schemas[variant];
        if (schema !== undefined) {
          InternalRecord.assignProperty(fields, key, schema);
        }
      }
    } else {
      InternalRecord.assignProperty(fields, key, value);
    }
  }
  const schema = Schema.Struct(fields);
  cache[variant] = schema;
  return schema;
});
/**
 * Returns the original field definitions stored on a variant schema struct.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export const fields = self => self[TypeId];
/**
 * Creates a variant schema toolkit for a fixed set of variant names and a default
 * variant.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const make = options => {
  function Class(identifier) {
    return function (fields, annotations) {
      const variantStruct = Struct(fields);
      const schema = extract(variantStruct, options.defaultVariant, {
        isDefault: true
      });
      const SClass = Schema.Class;
      class Base extends SClass(identifier)(schema.fields, annotations) {
        static [TypeId] = fields;
      }
      for (const variant of options.variants) {
        Object.defineProperty(Base, variant, {
          value: extract(variantStruct, variant).annotate({
            id: `${identifier}.${variant}`,
            title: `${identifier}.${variant}`
          })
        });
      }
      return Base;
    };
  }
  function FieldOnly(keys) {
    return function (schema) {
      const obj = {};
      for (const key of keys) {
        InternalRecord.assignProperty(obj, key, schema);
      }
      return Field(obj);
    };
  }
  function FieldExcept(keys) {
    return function (schema) {
      const obj = {};
      for (const variant of options.variants) {
        if (!keys.includes(variant)) {
          InternalRecord.assignProperty(obj, variant, schema);
        }
      }
      return Field(obj);
    };
  }
  function UnionVariants(members) {
    return Union(members, options.defaultVariant, options.variants);
  }
  const fieldEvolve = dual(2, (self, f) => {
    const field = isField(self) ? self : Field(Object.fromEntries(options.variants.map(variant => [variant, self])));
    return Field(Struct_.evolve(field.schemas, f));
  });
  const extractVariants = dual(2, (self, variant) => extract(self, variant, {
    isDefault: variant === options.defaultVariant
  }));
  return {
    Struct,
    Field,
    FieldOnly,
    FieldExcept,
    Class,
    Union: UnionVariants,
    fieldEvolve,
    // fieldFromKey,
    extract: extractVariants
  };
};
/**
 * Marks a value as an explicit override for an `Overrideable` schema default.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Override = value => value;
/**
 * Wraps a schema with an effectful constructor default while allowing explicit
 * values to be marked with `Override`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export const Overrideable = (schema, options) => schema.pipe(Schema.decodeTo(Schema.brand("Override")(Schema.toType(schema))), Schema.withConstructorDefault(Effect.map(options.defaultValue, Override)));
const StructProto = {
  pipe() {
    return pipeArguments(this, arguments);
  }
};
const Struct = fields => {
  const self = Object.create(StructProto);
  self[TypeId] = fields;
  return self;
};
const FieldProto = {
  [FieldTypeId]: FieldTypeId,
  pipe() {
    return pipeArguments(this, arguments);
  }
};
const Field = schemas => {
  const self = Object.create(FieldProto);
  self.schemas = schemas;
  return self;
};
const Union = (members, defaultVariant, variants) => {
  const VariantUnion = Schema.Union(members.map(member => Schema.isSchema(member) ? member : extract(member, defaultVariant, {
    isDefault: true
  })));
  for (const variant of variants) {
    Object.defineProperty(VariantUnion, variant, {
      value: Schema.Union(members.map(member => extract(member, variant)))
    });
  }
  return VariantUnion;
};
//# sourceMappingURL=VariantSchema.js.map