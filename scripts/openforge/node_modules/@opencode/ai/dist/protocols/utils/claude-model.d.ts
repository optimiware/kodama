import type { LLMRequest } from "../../schema/index.js";
export declare const THINKING_BINDING_BETA = "thinking-binding-controls-2026-08-01";
export declare const claudeVersion: (id: string) => {
    family: string;
    major: number;
    minor: number;
} | undefined;
export declare const supportsThinkingBlockBinding: (model: LLMRequest["model"]) => boolean;
