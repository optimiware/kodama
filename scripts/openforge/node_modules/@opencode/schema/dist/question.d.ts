export * as Question from "./question.js";
import { Schema } from "effect";
export declare const Prompt: Schema.Struct<{
    question: Schema.String;
    header: Schema.String;
    options: Schema.$Array<Schema.Struct<{
        readonly label: Schema.String;
        readonly description: Schema.String;
    }>>;
    multiple: Schema.decodeTo<Schema.optional<Schema.toType<Schema.Boolean>>, Schema.optionalKey<Schema.Boolean>, never, never>;
}>;
export interface Prompt extends Schema.Schema.Type<typeof Prompt> {
}
export declare const Answer: Schema.$Array<Schema.String>;
export type Answer = typeof Answer.Type;
