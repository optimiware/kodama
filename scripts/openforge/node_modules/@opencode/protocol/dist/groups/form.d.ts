import { Context, Schema } from "effect";
import { HttpApiEndpoint, HttpApiGroup, HttpApiMiddleware } from "effect/http-api";
export declare const makeFormGroup: <LocationId extends HttpApiMiddleware.AnyId, LocationService>(locationMiddleware: Context.Key<LocationId, LocationService>) => HttpApiGroup.HttpApiGroup<"server.form", HttpApiEndpoint.HttpApiEndpoint<"form.list", "GET", "/api/form", never, Schema.toCodecStringTree<Schema.Struct<{
    readonly location: Schema.optional<Schema.Struct<{
        readonly directory: Schema.optional<Schema.String>;
    }>>;
}>>, never, never, Schema.toCodecJson<Schema.Struct<{
    readonly location: Schema.Struct<{
        readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
    }>;
    readonly data: Schema.$Array<Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Form.ID"> & {
            create: (id?: string) => string & import("effect/Brand").Brand<"Form.ID">;
        };
        readonly sessionID: Schema.String;
        readonly title: Schema.String;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly fields: Schema.NonEmptyArray<Schema.Union<readonly [Schema.Struct<{
            readonly key: Schema.String;
            readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly required: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly when: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>, never, never>;
            readonly type: Schema.Literal<"string">;
            readonly format: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["email", "uri", "date", "date-time"]>>>, Schema.optionalKey<Schema.Literals<readonly ["email", "uri", "date", "date-time"]>>, never, never>;
            readonly minLength: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            readonly maxLength: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            readonly pattern: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly placeholder: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly options: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
                readonly value: Schema.String;
                readonly label: Schema.String;
                readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
                readonly value: Schema.String;
                readonly label: Schema.String;
                readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>>, never, never>;
            readonly custom: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        }>, Schema.Struct<{
            readonly key: Schema.String;
            readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly required: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly when: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>, never, never>;
            readonly type: Schema.Literal<"number">;
            readonly minimum: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Number>>, Schema.optionalKey<Schema.Number>, never, never>;
            readonly maximum: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Number>>, Schema.optionalKey<Schema.Number>, never, never>;
            readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Number>>, Schema.optionalKey<Schema.Number>, never, never>;
        }>, Schema.Struct<{
            readonly key: Schema.String;
            readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly required: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly when: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>, never, never>;
            readonly type: Schema.Literal<"integer">;
            readonly minimum: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Number>>, Schema.optionalKey<Schema.Number>, never, never>;
            readonly maximum: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Number>>, Schema.optionalKey<Schema.Number>, never, never>;
            readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Number>>, Schema.optionalKey<Schema.Number>, never, never>;
        }>, Schema.Struct<{
            readonly key: Schema.String;
            readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly required: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly when: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>, never, never>;
            readonly type: Schema.Literal<"boolean">;
            readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
        }>, Schema.Struct<{
            readonly key: Schema.String;
            readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly required: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly hidden: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly when: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
                readonly key: Schema.String;
                readonly op: Schema.Literals<readonly ["eq", "neq"]>;
                readonly value: Schema.Union<readonly [Schema.String, Schema.Number, Schema.Boolean]>;
            }>>>, never, never>;
            readonly type: Schema.Literal<"multiselect">;
            readonly options: Schema.$Array<Schema.Struct<{
                readonly value: Schema.String;
                readonly label: Schema.String;
                readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            }>>;
            readonly minItems: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            readonly maxItems: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
            readonly custom: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
            readonly default: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
        }>, Schema.Struct<{
            readonly key: Schema.String;
            readonly type: Schema.Literal<"external">;
            readonly url: Schema.String;
            readonly title: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
            readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
        }>]>>;
    }>>;
}>>, never, LocationId, HttpApiMiddleware.Requires<LocationId>>, false>;
