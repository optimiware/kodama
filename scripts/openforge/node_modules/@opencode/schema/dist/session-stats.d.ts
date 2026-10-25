export * as SessionStats from "./session-stats.js";
import { Schema } from "effect";
export declare const Activity: Schema.Struct<{
    readonly date: Schema.String;
    readonly steps: Schema.Int;
}>;
export type Activity = typeof Activity.Type;
export declare const ModelUsage: Schema.Struct<{
    readonly model: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Model.ID">;
        readonly providerID: Schema.brand<Schema.String, "Provider.ID"> & {
            opencode: string & import("effect/Brand").Brand<"Provider.ID">;
            anthropic: string & import("effect/Brand").Brand<"Provider.ID">;
            openai: string & import("effect/Brand").Brand<"Provider.ID">;
            google: string & import("effect/Brand").Brand<"Provider.ID">;
            googleVertex: string & import("effect/Brand").Brand<"Provider.ID">;
            githubCopilot: string & import("effect/Brand").Brand<"Provider.ID">;
            amazonBedrock: string & import("effect/Brand").Brand<"Provider.ID">;
            azure: string & import("effect/Brand").Brand<"Provider.ID">;
            openrouter: string & import("effect/Brand").Brand<"Provider.ID">;
            mistral: string & import("effect/Brand").Brand<"Provider.ID">;
            gitlab: string & import("effect/Brand").Brand<"Provider.ID">;
        };
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }> & {
        parse: (input: string) => {
            readonly id: string & import("effect/Brand").Brand<"Model.ID">;
            readonly providerID: string & import("effect/Brand").Brand<"Provider.ID">;
            readonly variant?: (string & import("effect/Brand").Brand<"Model.VariantID">) | undefined;
        };
    };
    readonly steps: Schema.Int;
    readonly tokens: Schema.Struct<{
        readonly input: Schema.Finite;
        readonly output: Schema.Finite;
        readonly reasoning: Schema.Finite;
        readonly cache: Schema.Struct<{
            readonly read: Schema.Finite;
            readonly write: Schema.Finite;
        }>;
    }>;
    readonly cost: Schema.brand<Schema.Finite, "Money.USD"> & {
        zero: number & import("effect/Brand").Brand<"Money.USD">;
    };
}>;
export type ModelUsage = typeof ModelUsage.Type;
export declare const ToolUsage: Schema.Struct<{
    readonly name: Schema.String;
    readonly calls: Schema.Int;
    readonly succeeded: Schema.Int;
    readonly failed: Schema.Int;
    readonly unfinished: Schema.Int;
    readonly durationP50: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
}>;
export type ToolUsage = typeof ToolUsage.Type;
export declare const ToolMode: Schema.Literals<readonly ["none", "summary", "detail"]>;
export type ToolMode = typeof ToolMode.Type;
export declare const ToolTotals: Schema.Struct<{
    readonly calls: Schema.Int;
    readonly succeeded: Schema.Int;
    readonly failed: Schema.Int;
    readonly unfinished: Schema.Int;
}>;
export type ToolTotals = typeof ToolTotals.Type;
export declare const Tools: Schema.Union<readonly [Schema.Struct<{
    readonly mode: Schema.Literal<"none">;
}>, Schema.Struct<{
    readonly mode: Schema.Literal<"summary">;
    readonly totals: Schema.Struct<{
        readonly calls: Schema.Int;
        readonly succeeded: Schema.Int;
        readonly failed: Schema.Int;
        readonly unfinished: Schema.Int;
    }>;
}>, Schema.Struct<{
    readonly mode: Schema.Literal<"detail">;
    readonly totals: Schema.Struct<{
        readonly calls: Schema.Int;
        readonly succeeded: Schema.Int;
        readonly failed: Schema.Int;
        readonly unfinished: Schema.Int;
    }>;
    readonly usage: Schema.$Array<Schema.Struct<{
        readonly name: Schema.String;
        readonly calls: Schema.Int;
        readonly succeeded: Schema.Int;
        readonly failed: Schema.Int;
        readonly unfinished: Schema.Int;
        readonly durationP50: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
    }>>;
}>]>;
export type Tools = typeof Tools.Type;
export declare const Info: Schema.Struct<{
    readonly range: Schema.Struct<{
        readonly from: Schema.decodeTo<Schema.DateTimeUtc, Schema.Finite, never, never>;
        readonly to: Schema.decodeTo<Schema.DateTimeUtc, Schema.Finite, never, never>;
    }>;
    readonly sessions: Schema.Int;
    readonly subagents: Schema.Int;
    readonly prompts: Schema.Int;
    readonly steps: Schema.Int;
    readonly tokens: Schema.Struct<{
        readonly input: Schema.Finite;
        readonly output: Schema.Finite;
        readonly reasoning: Schema.Finite;
        readonly cache: Schema.Struct<{
            readonly read: Schema.Finite;
            readonly write: Schema.Finite;
        }>;
    }>;
    readonly cost: Schema.brand<Schema.Finite, "Money.USD"> & {
        zero: number & import("effect/Brand").Brand<"Money.USD">;
    };
    readonly tools: Schema.Union<readonly [Schema.Struct<{
        readonly mode: Schema.Literal<"none">;
    }>, Schema.Struct<{
        readonly mode: Schema.Literal<"summary">;
        readonly totals: Schema.Struct<{
            readonly calls: Schema.Int;
            readonly succeeded: Schema.Int;
            readonly failed: Schema.Int;
            readonly unfinished: Schema.Int;
        }>;
    }>, Schema.Struct<{
        readonly mode: Schema.Literal<"detail">;
        readonly totals: Schema.Struct<{
            readonly calls: Schema.Int;
            readonly succeeded: Schema.Int;
            readonly failed: Schema.Int;
            readonly unfinished: Schema.Int;
        }>;
        readonly usage: Schema.$Array<Schema.Struct<{
            readonly name: Schema.String;
            readonly calls: Schema.Int;
            readonly succeeded: Schema.Int;
            readonly failed: Schema.Int;
            readonly unfinished: Schema.Int;
            readonly durationP50: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Finite>>, Schema.optionalKey<Schema.Finite>, never, never>;
        }>>;
    }>]>;
    readonly activeDays: Schema.Int;
    readonly streak: Schema.Int;
    readonly activity: Schema.$Array<Schema.Struct<{
        readonly date: Schema.String;
        readonly steps: Schema.Int;
    }>>;
    readonly models: Schema.$Array<Schema.Struct<{
        readonly model: Schema.Struct<{
            readonly id: Schema.brand<Schema.String, "Model.ID">;
            readonly providerID: Schema.brand<Schema.String, "Provider.ID"> & {
                opencode: string & import("effect/Brand").Brand<"Provider.ID">;
                anthropic: string & import("effect/Brand").Brand<"Provider.ID">;
                openai: string & import("effect/Brand").Brand<"Provider.ID">;
                google: string & import("effect/Brand").Brand<"Provider.ID">;
                googleVertex: string & import("effect/Brand").Brand<"Provider.ID">;
                githubCopilot: string & import("effect/Brand").Brand<"Provider.ID">;
                amazonBedrock: string & import("effect/Brand").Brand<"Provider.ID">;
                azure: string & import("effect/Brand").Brand<"Provider.ID">;
                openrouter: string & import("effect/Brand").Brand<"Provider.ID">;
                mistral: string & import("effect/Brand").Brand<"Provider.ID">;
                gitlab: string & import("effect/Brand").Brand<"Provider.ID">;
            };
            readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
        }> & {
            parse: (input: string) => {
                readonly id: string & import("effect/Brand").Brand<"Model.ID">;
                readonly providerID: string & import("effect/Brand").Brand<"Provider.ID">;
                readonly variant?: (string & import("effect/Brand").Brand<"Model.VariantID">) | undefined;
            };
        };
        readonly steps: Schema.Int;
        readonly tokens: Schema.Struct<{
            readonly input: Schema.Finite;
            readonly output: Schema.Finite;
            readonly reasoning: Schema.Finite;
            readonly cache: Schema.Struct<{
                readonly read: Schema.Finite;
                readonly write: Schema.Finite;
            }>;
        }>;
        readonly cost: Schema.brand<Schema.Finite, "Money.USD"> & {
            zero: number & import("effect/Brand").Brand<"Money.USD">;
        };
    }>>;
}>;
export type Info = typeof Info.Type;
