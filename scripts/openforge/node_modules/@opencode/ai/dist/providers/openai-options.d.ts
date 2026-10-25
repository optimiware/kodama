import { type ProviderOptions } from "../schema/index.js";
import type { OpenAIServiceTier } from "../protocols/utils/openai-options.js";
import type { Options } from "../protocols/utils/open-responses-options.js";
import type { ContextManagement } from "../protocols/openai-responses.js";
export type { OpenAIResponseIncludable, OpenAIServiceTier } from "../protocols/utils/openai-options.js";
export type OpenAIOptionsInput = Omit<Options, "serviceTier"> & {
    /** Advanced in-band compaction. The caller owns checkpoint persistence and recovery. */
    readonly contextManagement?: ContextManagement;
    readonly serviceTier?: OpenAIServiceTier;
    readonly [key: string]: unknown;
};
export type OpenAIProviderOptionsInput = OpenAIOptionsInput;
export declare const gpt5DefaultOptions: (modelID: string) => ProviderOptions | undefined;
export declare const openAIDefaultOptions: (modelID: string) => ProviderOptions | undefined;
export declare const withOpenAIOptions: <Options extends {
    readonly providerOptions?: ProviderOptions;
}>(modelID: string, options: Options) => Omit<Options, "providerOptions"> & {
    readonly providerOptions?: ProviderOptions;
};
export * as OpenAIProviderOptions from "./openai-options.js";
