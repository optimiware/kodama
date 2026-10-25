import { Schema } from "effect";
import { HttpApiEndpoint, HttpApiGroup } from "effect/http-api";
export declare const V1MigrationStatus: Schema.Union<readonly [Schema.Struct<{
    readonly status: Schema.Literals<readonly ["required", "completed"]>;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"running">;
    readonly progress: Schema.Struct<{
        readonly label: Schema.String;
        readonly numerator: Schema.optional<Schema.Int>;
        readonly denominator: Schema.optional<Schema.Int>;
    }>;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"error">;
    readonly error: Schema.String;
}>]>;
export declare const MigrationGroup: HttpApiGroup.HttpApiGroup<"server.migration", HttpApiEndpoint.HttpApiEndpoint<"migration.v1.status", "GET", "/api/experimental/migration/v1", never, never, never, never, Schema.toCodecJson<Schema.Union<readonly [Schema.Struct<{
    readonly status: Schema.Literals<readonly ["required", "completed"]>;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"running">;
    readonly progress: Schema.Struct<{
        readonly label: Schema.String;
        readonly numerator: Schema.optional<Schema.Int>;
        readonly denominator: Schema.optional<Schema.Int>;
    }>;
}>, Schema.Struct<{
    readonly status: Schema.Literal<"error">;
    readonly error: Schema.String;
}>]>>, never, never, never>, false>;
