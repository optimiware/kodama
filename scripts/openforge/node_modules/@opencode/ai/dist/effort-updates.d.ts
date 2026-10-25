import { LLMRequest, type EffortPart, type Message } from "./schema/messages.js";
export declare const effortUpdate: (message: Message) => EffortPart | undefined;
export declare const stripEffortUpdates: (request: LLMRequest) => LLMRequest<import("./index.js").LanguageModel<{
    readonly [x: string]: unknown;
}, import("./route.js").CompactionOperations | undefined>>;
export declare const applyEffortUpdates: (request: LLMRequest) => LLMRequest;
export declare const resolveEffortUpdates: (request: LLMRequest, current: string | undefined) => {
    request: LLMRequest<import("./index.js").LanguageModel<{
        readonly [x: string]: unknown;
    }, import("./route.js").CompactionOperations | undefined>>;
    effort: string | undefined;
};
