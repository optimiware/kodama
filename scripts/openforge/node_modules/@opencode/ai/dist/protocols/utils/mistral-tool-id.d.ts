import type { LLMRequest } from "../../schema/index.js";
export declare const valid: RegExp;
export declare const hash: (value: string) => string;
export declare const normalizer: (request: LLMRequest) => (id: string) => string;
export * as MistralToolID from "./mistral-tool-id.js";
