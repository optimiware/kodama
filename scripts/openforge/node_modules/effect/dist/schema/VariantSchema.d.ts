/**
 * Builds related schemas for named variants from shared field definitions.
 *
 * `make` fixes the variant names and default variant, then lets callers define
 * fields that are shared by all variants or specific to some variants. From
 * those definitions it can create schema classes, unions, extracted struct
 * schemas, and helpers for changing fields across variants.
 *
 * @stability unstable
 * @since 4.0.0
 */
import type { Brand } from "../Brand.ts";
import * as Effect from "../Effect.ts";
import { type Pipeable } from "../Pipeable.ts";
import * as Schema from "../Schema.ts";
import * as Struct_ from "../Struct.ts";
/**
 * Runtime type identifier attached to variant schema structs.
 *
 * @stability unstable
 * @category type IDs
 * @since 4.0.0
 */
export declare const TypeId = "~effect/schema/VariantSchema";
/**
 * Pipeable container of schema fields that can be extracted into per-variant
 * `Schema.Struct` schemas.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Struct<in out A extends Field.Fields> extends Pipeable {
    readonly [TypeId]: A;
}
/**
 * Returns `true` when a value is a variant schema struct.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isStruct: (u: unknown) => u is Struct<any>;
/**
 * Type-level helpers for variant schema structs.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Struct {
    /**
     * Minimal structural type for any variant schema struct.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Any = {
        readonly [TypeId]: any;
    };
    /**
     * Field map accepted by a variant struct, where each property may be a schema, a
     * variant field, a nested struct, or `undefined`.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Fields = {
        readonly [key: string]: Schema.Top | Field<any> | Struct<any> | undefined;
    };
    /**
     * Type-level validation that every variant field in a struct only uses variants
     * from the configured variant set.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Validate<A, Variant extends string> = {
        readonly [K in keyof A]: A[K] extends {
            readonly [TypeId]: infer _;
        } ? Validate<A[K], Variant> : A[K] extends Field<infer Config> ? [keyof Config] extends [Variant] ? {} : "field must have valid variants" : unknown;
    };
}
declare const FieldTypeId = "~effect/schema/VariantSchema/Field";
/**
 * Pipeable collection of variant-specific schemas for a single logical field.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Field<in out A extends Field.Config> extends Pipeable {
    readonly [FieldTypeId]: typeof FieldTypeId;
    readonly schemas: A;
}
/**
 * Returns `true` when a value is a variant schema field.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isField: (u: unknown) => u is Field<any>;
/**
 * Type-level helpers for variant schema fields.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Field {
    /**
     * Minimal structural type for any variant schema field.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Any = {
        readonly [FieldTypeId]: typeof FieldTypeId;
    };
    /**
     * Map from variant name to the schema used for a field in that variant.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Config = {
        readonly [key: string]: Schema.Top | undefined;
    };
    /**
     * Variant field configuration restricted to an optional subset of the supplied
     * variant keys.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type ConfigWithKeys<K extends string> = {
        readonly [P in K]?: Schema.Top | undefined;
    };
    /**
     * Field map whose properties may be schemas, variant fields, nested structs, or
     * `undefined`.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Fields = {
        readonly [key: string]: Schema.Top | Field<any> | Struct<any> | undefined;
    };
}
/**
 * Computes the `Schema.Struct` field map for a variant by selecting matching
 * field schemas and recursively extracting nested structs.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type ExtractFields<V extends string, Fields extends Struct.Fields, IsDefault = false> = {
    readonly [K in keyof Fields as [Fields[K]] extends [Field<infer Config>] ? V extends keyof Config ? K : never : K]: [Fields[K]] extends [Struct<infer _>] ? Extract<V, Fields[K], IsDefault> : [Fields[K]] extends [Field<infer Config>] ? [Config[V]] extends [Schema.Top] ? Config[V] : never : [Fields[K]] extends [Schema.Top] ? Fields[K] : never;
};
/**
 * Computes the schema type produced by extracting a single variant from a variant
 * schema struct.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Extract<V extends string, A extends Struct<any>, IsDefault = false> = [A] extends [
    Struct<infer Fields>
] ? IsDefault extends true ? [A] extends [Schema.Top] ? A : Schema.Struct<Struct_.Simplify<ExtractFields<V, Fields>>> : Schema.Struct<Struct_.Simplify<ExtractFields<V, Fields>>> : never;
/**
 * Returns the original field definitions stored on a variant schema struct.
 *
 * @stability unstable
 * @category accessors
 * @since 4.0.0
 */
export declare const fields: <A extends Struct<any>>(self: A) => A[typeof TypeId];
/**
 * Schema class type returned by variant class constructors, combining the default
 * variant schema with access to the original variant fields.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Class<Self, Fields extends Struct.Fields, S extends Schema.Top & {
    readonly fields: Schema.Struct.Fields;
}> extends Schema.Class<Self, S, {}>, Struct<Struct_.Simplify<Fields>> {
    readonly "Type": Self;
    readonly "Encoded": S["Encoded"];
    readonly "DecodingServices": S["DecodingServices"];
    readonly "EncodingServices": S["EncodingServices"];
    readonly "~type.make.in": S["~type.make.in"];
    readonly "~type.make": Self;
    readonly "Iso": S["Iso"];
    new (props: S["~type.make.in"], options?: {
        readonly disableChecks?: boolean;
    } | undefined): S["Type"];
    make<Args extends Array<any>, X>(this: {
        new (...args: Args): X;
    }, ...args: Args): X;
    readonly fields: S["fields"];
}
type MissingSelfGeneric<Params extends string = ""> = `Missing \`Self\` generic - use \`class Self extends Class<Self>()(${Params}{ ... })\``;
/**
 * Union schema over the default schemas of a list of variant schema structs.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Union<Members extends ReadonlyArray<Struct<any>>, Default extends string = string> extends Schema.Union<{
    readonly [K in keyof Members]: Extract<Default, Members[K], true>;
}> {
}
/**
 * Type-level helpers for unions of variant schema structs.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace Union {
    /**
     * Computes a union schema for each variant from a list of variant schema structs.
     *
     * @stability unstable
     * @category models
     * @since 4.0.0
     */
    type Variants<Members extends ReadonlyArray<Struct<any>>, Variants extends string> = {
        readonly [Variant in Variants]: Schema.Union<{
            [K in keyof Members]: Extract<Variant, Members[K]>;
        }>;
    };
}
/**
 * Creates a variant schema toolkit for a fixed set of variant names and a default
 * variant.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <const Variants extends ReadonlyArray<string>, const Default extends Variants[number]>(options: {
    readonly variants: Variants;
    readonly defaultVariant: Default;
}) => {
    readonly Struct: <const A extends Struct.Fields>(fields: A & Struct.Validate<A, Variants[number]>) => Struct<A>;
    readonly Field: <const A extends Field.ConfigWithKeys<Variants[number]>>(config: A & { readonly [K in Exclude<keyof A, Variants[number]>]: never; }) => Field<A>;
    readonly FieldOnly: <const Keys extends ReadonlyArray<Variants[number]>>(keys: Keys) => <S extends Schema.Top>(schema: S) => Field<{ readonly [K in Keys[number]]: S; }>;
    readonly FieldExcept: <const Keys extends ReadonlyArray<Variants[number]>>(keys: Keys) => <S extends Schema.Top>(schema: S) => Field<{ readonly [K in Exclude<Variants[number], Keys[number]>]: S; }>;
    readonly fieldEvolve: {
        <Self extends Field<any> | Schema.Top, const Mapping extends (Self extends Field<infer S> ? { readonly [K in keyof S]?: (variant: S[K]) => Schema.Top; } : { readonly [K in Variants[number]]?: (variant: Self) => Schema.Top; })>(f: Mapping): (self: Self) => Field<Self extends Field<infer S> ? { readonly [K in keyof S]: K extends keyof Mapping ? Mapping[K] extends (arg: any) => any ? ReturnType<Mapping[K]> : S[K] : S[K]; } : { readonly [K in Variants[number]]: K extends keyof Mapping ? Mapping[K] extends (arg: any) => any ? ReturnType<Mapping[K]> : Self : Self; }>;
        <Self extends Field<any> | Schema.Top, const Mapping extends (Self extends Field<infer S> ? { readonly [K in keyof S]?: (variant: S[K]) => Schema.Top; } : { readonly [K in Variants[number]]?: (variant: Self) => Schema.Top; })>(self: Self, f: Mapping): Field<Self extends Field<infer S> ? { readonly [K in keyof S]: K extends keyof Mapping ? Mapping[K] extends (arg: any) => any ? ReturnType<Mapping[K]> : S[K] : S[K]; } : { readonly [K in Variants[number]]: K extends keyof Mapping ? Mapping[K] extends (arg: any) => any ? ReturnType<Mapping[K]> : Self : Self; }>;
    };
    readonly Class: <Self = never>(identifier: string) => <const Fields extends Struct.Fields>(fields: Fields & Struct.Validate<Fields, Variants[number]>, annotations?: Schema.Annotations.Declaration<Self, readonly [Schema.Struct<ExtractFields<Default, Fields, true>>]> | undefined) => [Self] extends [never] ? MissingSelfGeneric : Class<Self, Fields, Schema.Struct<ExtractFields<Default, Fields, true>>> & { readonly [V in Variants[number]]: Extract<V, Struct<Fields>>; };
    readonly Union: <const Members extends ReadonlyArray<Struct<any>>>(members: Members) => Union<Members, Default> & Union.Variants<Members, Variants[number]>;
    readonly extract: {
        <V extends Variants[number]>(variant: V): <A extends Struct<any>>(self: A) => Extract<V, A, V extends Default ? true : false>;
        <V extends Variants[number], A extends Struct<any>>(self: A, variant: V): Extract<V, A, V extends Default ? true : false>;
    };
};
/**
 * Marks a value as an explicit override for an `Overrideable` schema default.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Override: <A>(value: A) => A & Brand<"Override">;
/**
 * Schema type whose constructor can use an effectful default unless a value is
 * explicitly branded with `Override`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export interface Overrideable<S extends Schema.Top & Schema.WithoutConstructorDefault> extends Schema.BottomLazy<S["ast"], Overrideable<S>, S["~type.parameters"], S["~type.mutability"], "required", "with-default", S["~encoded.mutability"], S["~encoded.optionality"]> {
    readonly "Type": S["Type"] & Brand<"Override">;
    readonly "Encoded": S["Encoded"];
    readonly "DecodingServices": S["DecodingServices"];
    readonly "EncodingServices": S["EncodingServices"];
    readonly "~type.make.in": S["~type.make.in"];
    readonly "~type.make": (S["Type"] & Brand<"Override">) | undefined;
    readonly "Iso": (S["Type"] & Brand<"Override">) | undefined;
}
/**
 * Wraps a schema with an effectful constructor default while allowing explicit
 * values to be marked with `Override`.
 *
 * @stability unstable
 * @category schemas
 * @since 4.0.0
 */
export declare const Overrideable: <S extends Schema.Top & Schema.WithoutConstructorDefault>(schema: S, options: {
    readonly defaultValue: Effect.Effect<S["~type.make.in"]>;
}) => Overrideable<S>;
export {};
//# sourceMappingURL=VariantSchema.d.ts.map