export * as LLM from "./llm.js";
import { Schema } from "effect";
export declare const FinishReason: Schema.Literals<readonly ["stop", "length", "tool-calls", "content-filter", "error", "unknown"]>;
export type FinishReason = typeof FinishReason.Type;
