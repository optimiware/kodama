export * as Config from "./config.js";
import { Schema } from "effect";
import { ConfigAgent } from "./config/agent.js";
import { ConfigMedia } from "./config/media.js";
import { ConfigCompaction } from "./config/compaction.js";
import { ConfigCommand } from "./config/command.js";
import { ConfigExperimental } from "./config/experimental.js";
import { ConfigFormatter } from "./config/formatter.js";
import { ConfigLSP } from "./config/lsp.js";
import { ConfigMCP } from "./config/mcp.js";
import { ConfigPlugin } from "./config/plugin.js";
import { ConfigProvider } from "./config/provider.js";
import { ConfigReference } from "./config/reference.js";
import { ConfigWebSearch } from "./config/websearch.js";
import { ConfigToolOutput } from "./config/tool-output.js";
import { ConfigWatcher } from "./config/watcher.js";
import { ConfigWarming } from "./config/warming.js";
declare const Info_base: Schema.Class<Info, Schema.Struct<{
    readonly $schema: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly shell: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly model: Schema.decodeTo<Schema.optional<Schema.toType<Schema.decodeTo<Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>, Schema.Union<readonly [Schema.String, Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>]>, never, never>>>, Schema.optionalKey<Schema.decodeTo<Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>, Schema.Union<readonly [Schema.String, Schema.Struct<{
        readonly providerID: Schema.brand<Schema.String, "Provider.ID">;
        readonly model: Schema.brand<Schema.String, "Model.ID">;
        readonly variant: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "Model.VariantID">>>, Schema.optionalKey<Schema.brand<Schema.String, "Model.VariantID">>, never, never>;
    }>]>, never, never>>, never, never>;
    readonly default_agent: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly update: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["disable", "notify", "auto"]>>>, Schema.optionalKey<Schema.Literals<readonly ["disable", "notify", "auto"]>>, never, never>;
    readonly share: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Literals<readonly ["manual", "auto", "disabled"]>>>, Schema.optionalKey<Schema.Literals<readonly ["manual", "auto", "disabled"]>>, never, never>;
    readonly enterprise: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly url: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    }>>, never, never>;
    readonly username: Schema.decodeTo<Schema.optional<Schema.toType<Schema.String>>, Schema.optionalKey<Schema.String>, never, never>;
    readonly permissions: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
        readonly action: Schema.String;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
    }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
        readonly action: Schema.String;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny", "ask"]>;
    }>>>, never, never>;
    readonly policies: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Struct<{
        readonly action: Schema.Literals<readonly ["provider.use", "tool.use", "integration.use"]>;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny"]>;
    }>>>>, Schema.optionalKey<Schema.$Array<Schema.Struct<{
        readonly action: Schema.Literals<readonly ["provider.use", "tool.use", "integration.use"]>;
        readonly resource: Schema.String;
        readonly effect: Schema.Literals<readonly ["allow", "deny"]>;
    }>>>, never, never>;
    readonly agents: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, typeof ConfigAgent.Info>>>, Schema.optionalKey<Schema.$Record<Schema.String, typeof ConfigAgent.Info>>, never, never>;
    readonly snapshots: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
    readonly watcher: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigWatcher.Info>>, Schema.optionalKey<typeof ConfigWatcher.Info>, never, never>;
    readonly formatter: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Boolean, Schema.$Record<Schema.String, typeof ConfigFormatter.Entry>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Boolean, Schema.$Record<Schema.String, typeof ConfigFormatter.Entry>]>>, never, never>;
    readonly lsp: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Boolean, Schema.$Record<Schema.String, Schema.Union<readonly [Schema.Struct<{
        readonly disabled: Schema.Literal<true>;
    }>, typeof ConfigLSP.Server]>>]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Boolean, Schema.$Record<Schema.String, Schema.Union<readonly [Schema.Struct<{
        readonly disabled: Schema.Literal<true>;
    }>, typeof ConfigLSP.Server]>>]>>, never, never>;
    readonly media: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigMedia.Info>>, Schema.optionalKey<typeof ConfigMedia.Info>, never, never>;
    readonly tool_output: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigToolOutput.Info>>, Schema.optionalKey<typeof ConfigToolOutput.Info>, never, never>;
    readonly mcp: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigMCP.Info>>, Schema.optionalKey<typeof ConfigMCP.Info>, never, never>;
    readonly compaction: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigCompaction.Info>>, Schema.optionalKey<typeof ConfigCompaction.Info>, never, never>;
    readonly skills: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly commands: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, typeof ConfigCommand.Info>>>, Schema.optionalKey<Schema.$Record<Schema.String, typeof ConfigCommand.Info>>, never, never>;
    readonly instructions: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.String>>>, Schema.optionalKey<Schema.$Array<Schema.String>>, never, never>;
    readonly references: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, typeof ConfigReference.Git, typeof ConfigReference.Local]>>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Union<readonly [Schema.String, typeof ConfigReference.Git, typeof ConfigReference.Local]>>>, never, never>;
    readonly websearch: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Literal<false>, typeof ConfigWebSearch.Info]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Literal<false>, typeof ConfigWebSearch.Info]>>, never, never>;
    readonly plugins: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Array<Schema.Union<readonly [Schema.String, typeof ConfigPlugin.Entry]>>>>, Schema.optionalKey<Schema.$Array<Schema.Union<readonly [Schema.String, typeof ConfigPlugin.Entry]>>>, never, never>;
    readonly worktree: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Struct<{
        readonly directory: Schema.Trim;
    }>>>, Schema.optionalKey<Schema.Struct<{
        readonly directory: Schema.Trim;
    }>>, never, never>;
    readonly warming: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Union<readonly [Schema.Boolean, typeof ConfigWarming.Info]>>>, Schema.optionalKey<Schema.Union<readonly [Schema.Boolean, typeof ConfigWarming.Info]>>, never, never>;
    readonly providers: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, typeof ConfigProvider.Info>>>, Schema.optionalKey<Schema.$Record<Schema.String, typeof ConfigProvider.Info>>, never, never>;
    readonly experimental: Schema.decodeTo<Schema.optional<Schema.toType<typeof ConfigExperimental.Info>>, Schema.optionalKey<typeof ConfigExperimental.Info>, never, never>;
}>, {}>;
export declare class Info extends Info_base {
}
export declare const Patch: Schema.Struct<{
    readonly shell: Schema.NullOr<Schema.String>;
}>;
export interface Patch extends Schema.Schema.Type<typeof Patch> {
}
declare const Document_base: Schema.Class<Document, Schema.Struct<{
    readonly type: Schema.Literal<"document">;
    readonly path: Schema.decodeTo<Schema.optional<Schema.toType<Schema.brand<Schema.String, "AbsolutePath">>>, Schema.optionalKey<Schema.brand<Schema.String, "AbsolutePath">>, never, never>;
    readonly info: typeof Info;
}>, {}>;
export declare class Document extends Document_base {
}
declare const Directory_base: Schema.Class<Directory, Schema.Struct<{
    readonly type: Schema.Literal<"directory">;
    readonly path: Schema.brand<Schema.String, "AbsolutePath">;
}>, {}>;
export declare class Directory extends Directory_base {
}
export declare const Entry: Schema.Union<readonly [typeof Document, typeof Directory]>;
export type Entry = typeof Entry.Type;
export declare const Event: {
    Updated: Schema.Struct<{
        readonly id: Schema.brand<Schema.String, "Event.ID"> & {
            create: () => string & import("effect/Brand").Brand<"Event.ID">;
        };
        readonly created: Schema.Finite;
        readonly metadata: Schema.decodeTo<Schema.optional<Schema.toType<Schema.$Record<Schema.String, Schema.Unknown>>>, Schema.optionalKey<Schema.$Record<Schema.String, Schema.Unknown>>, never, never>;
        readonly type: Schema.Literal<"config.updated">;
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
        type: "config.updated";
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
        readonly type: Schema.Literal<"config.updated">;
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
        type: "config.updated";
        durability: "ephemeral";
        durable: undefined;
        data: Schema.Struct<{}>;
    }];
};
