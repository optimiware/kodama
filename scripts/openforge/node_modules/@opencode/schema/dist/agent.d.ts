export * as Agent from "./agent.js";
import { Schema } from "effect";
export declare const ID: Schema.brand<Schema.String, "Agent.ID">;
export type ID = typeof ID.Type;
export declare const Name: Schema.brand<Schema.String, "Agent.Name">;
export type Name = typeof Name.Type;
export declare const Color: Schema.String;
export type Color = typeof Color.Type;
export interface Info extends Schema.Schema.Type<typeof Info> {
}
export declare const Info: Schema.Struct<{
    readonly id: Schema.brand<Schema.String, "Agent.ID">;
    readonly name: Schema.brand<Schema.String, "Agent.Name">;
    readonly model: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
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
    }>>, Schema.optionalKey<Schema.Struct<{
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
    }>, never, never>;
    readonly request: Schema.Struct<{
        readonly settings: Schema.withConstructorDefault<Schema.StructWithRest<Schema.Struct<{
            readonly timeout: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Finite, Schema.Literal<false>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Finite, Schema.Literal<false>]>>, never, never>;
            readonly headerTimeout: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Finite, Schema.Literal<false>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Finite, Schema.Literal<false>]>>, never, never>;
            readonly chunkTimeout: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Finite, Schema.Literal<false>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Finite, Schema.Literal<false>]>>, never, never>;
            readonly compaction: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"summary">;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"native">;
            }>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Struct<{
                readonly type: Schema.Literal<"summary">;
            }>, Schema.Struct<{
                readonly type: Schema.Literal<"native">;
            }>]>>, never, never>;
            readonly transport: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["http", "websocket"]>>>, Schema.optionalKey<Schema.Literals<readonly ["http", "websocket"]>>, never, never>;
            readonly encoding: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["zstd", "gzip", "none"]>>>, Schema.optionalKey<Schema.Literals<readonly ["zstd", "gzip", "none"]>>, never, never>;
        }>, readonly [Schema.$Record<Schema.String, Schema.Any>]>>;
        readonly headers: Schema.$Record<Schema.String, Schema.String>;
        readonly body: Schema.$Record<Schema.String, Schema.Any>;
    }>;
    readonly system: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly description: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly mode: Schema.Literals<readonly ["subagent", "primary", "all"]>;
    readonly hidden: Schema.Boolean;
    readonly color: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly steps: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Int>>, Schema.optionalKey<Schema.Int>, never, never>;
    readonly permissions: Schema.$Array<Schema.Struct<{
        readonly action: Schema.String;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
    }>>;
}> & {
    default: (id: ID) => {
        id: string & import("effect/Brand").Brand<"Agent.ID">;
        name: string & import("effect/Brand").Brand<"Agent.Name">;
        request: {
            settings: {};
            headers: {};
            body: {};
        };
        mode: "primary";
        hidden: false;
        permissions: ({
            action: string;
            resource: string;
            effect: "allow";
        } | {
            action: string;
            resource: string;
            effect: "ask";
        })[];
    };
};
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"agent.updated">;
        readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>>, Schema.optionalKey<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>, never, never>;
        readonly data: Schema.Struct<{}>;
    }> & {
        type: "agent.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    };
    Definitions: readonly [Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"agent.updated">;
        readonly location: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>>, Schema.optionalKey<Schema.Struct<{
            readonly directory: Schema.brand<Schema.String, "AbsolutePath">;
            readonly workspaceID: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>>, Schema.optionalKey<Schema.brand<Schema.String, "Workspace.ID"> & {
                ascending: (id?: string) => string & import("effect/Brand").Brand<"Workspace.ID">;
                create: () => string & import("effect/Brand").Brand<"Workspace.ID">;
            }>, never, never>;
        }>>, never, never>;
        readonly data: Schema.Struct<{}>;
    }> & {
        type: "agent.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    }];
};
