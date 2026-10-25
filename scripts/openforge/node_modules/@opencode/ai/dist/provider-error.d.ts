import { AIError, type HttpContext, type HttpRateLimitDetails } from "./schema/index.js";
export declare const isContextOverflow: (message: string) => boolean;
export declare const isPayloadTooLarge: (message: string) => boolean;
export declare const isContextOverflowFailure: (failure: unknown) => boolean;
/**
 * Whether a failed call may succeed when sent again: rate limits, provider-side failures, transport failures that did
 * not deliver an accepted write, and unrecognized failures. Callers decide which calls are safe to repeat.
 */
export declare const isRetryable: (error: AIError) => boolean;
export declare const providerErrorMessage: (body: string) => string | undefined;
export interface ProviderFailure {
    readonly message: string;
    readonly status?: number | undefined;
    readonly rawBody?: string | undefined;
    readonly data?: unknown;
    readonly http?: HttpContext | undefined;
    readonly cause?: unknown;
    readonly retryAfterMs?: number | undefined;
    readonly rateLimit?: HttpRateLimitDetails | undefined;
}
export declare function classifyProviderFailure(input: ProviderFailure): AIError["reason"];
