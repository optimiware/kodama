export * as Tool from "./tool.js";
import { Effect, JsonSchema, Schema } from "effect";
import type { StandardSchemaV1 } from "@standard-schema/spec";
import type { Agent } from "./agent.js";
import type { Session } from "./session.js";
import type { SessionMessage } from "./session-message.js";
export type Metadata = Readonly<Record<string, any>>;
export declare const CallID: Schema.brand<Schema.String, "Tool.CallID">;
export type CallID = typeof CallID.Type;
export interface Context {
    readonly sessionID: Session.ID;
    readonly agent: Agent.ID;
    readonly messageID: SessionMessage.ID;
    readonly id: CallID;
    readonly progress: (update: Metadata) => Effect.Effect<void>;
}
export interface Namespace {
    readonly name: string;
    readonly description: string;
}
interface BaseOptions {
    readonly namespace?: string;
    readonly permission?: string;
}
export type Options = BaseOptions & ({
    readonly codemode?: true;
    readonly pinned?: boolean;
} | {
    readonly codemode: boolean;
    readonly pinned?: never;
});
export type ValueSchema<A = unknown> = Schema.Codec<A, any> | StandardSchemaV1<any, A> | JsonSchema.JsonSchema;
type InputValue<S> = 0 extends 1 & S ? any : S extends Schema.Codec<infer A, any> ? A : S extends StandardSchemaV1<any, infer A> ? A : unknown;
type OutputValue<S> = S extends undefined ? never : S extends Schema.Codec<infer A, any> ? A : S extends StandardSchemaV1<infer A, any> ? A : any;
declare const Error_base: Schema.Class<Error, Schema.TaggedStruct<"Tool.Error", {
    readonly message: Schema.String;
    readonly error: Schema.optional<Schema.Defect>;
    readonly metadata: Schema.optional<Schema.$Record<Schema.String, Schema.Unknown>>;
}>, import("effect/Cause").YieldableError>;
export declare class Error extends Error_base {
}
export interface TextContent extends Schema.Schema.Type<typeof TextContent> {
}
export declare const TextContent: Schema.Struct<{
    readonly type: Schema.Literal<"text">;
    readonly text: Schema.String;
}>;
export interface FileContent extends Schema.Schema.Type<typeof FileContent> {
}
export declare const FileContent: Schema.Struct<{
    readonly type: Schema.Literal<"file">;
    readonly uri: Schema.String;
    readonly mime: Schema.String;
    readonly name: Schema.optional<Schema.String>;
}>;
export declare const Content: Schema.Union<readonly [Schema.Struct<{
    readonly type: Schema.Literal<"text">;
    readonly text: Schema.String;
}>, Schema.Struct<{
    readonly type: Schema.Literal<"file">;
    readonly uri: Schema.String;
    readonly mime: Schema.String;
    readonly name: Schema.optional<Schema.String>;
}>]>;
export type Content = Schema.Schema.Type<typeof Content>;
export interface Result<Output extends ValueSchema<any> | undefined = ValueSchema<any> | undefined> {
    readonly output?: OutputValue<Output>;
    readonly content?: string | ReadonlyArray<Content>;
    readonly metadata?: Metadata;
}
export type Info<Input extends ValueSchema<any> = ValueSchema<any>, Output extends ValueSchema<any> | undefined = ValueSchema<any> | undefined> = {
    readonly name: string;
    readonly input: Input;
    readonly description: string;
    readonly execute: (input: InputValue<Input>, context: Context) => Effect.Effect<Result<Output>, Error>;
    readonly output?: Output;
    readonly options?: Options;
};
