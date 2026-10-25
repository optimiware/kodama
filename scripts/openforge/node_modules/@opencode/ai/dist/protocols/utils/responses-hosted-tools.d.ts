import { Effect } from "effect";
import { type AIError, type ToolResultPart } from "../../schema/index.js";
import { OpenResponses } from "../open-responses.js";
export type Item = OpenResponses.OutputItem & {
    readonly status?: string;
    readonly action?: unknown;
    readonly queries?: unknown;
    readonly results?: unknown;
    readonly code?: string;
    readonly container_id?: string;
    readonly outputs?: unknown;
    readonly server_label?: string;
    readonly output?: unknown;
    readonly result?: string;
    readonly output_format?: "png" | "jpeg" | "webp";
    readonly error?: unknown;
};
export interface Definition {
    readonly name: string;
    readonly input: (item: Item) => unknown;
    readonly result?: (item: Item) => Effect.Effect<ToolResultPart["result"], AIError>;
}
export type Definitions = Readonly<Record<string, Definition>>;
export declare const isItem: <Tools extends Definitions>(item: OpenResponses.OutputItem, tools: Tools) => item is Item;
export declare const onDone: (state: OpenResponses.ParserState, item: Item, tools: Definitions) => Effect.Effect<OpenResponses.StepResult, AIError>;
export * as ResponsesHostedTools from "./responses-hosted-tools.js";
