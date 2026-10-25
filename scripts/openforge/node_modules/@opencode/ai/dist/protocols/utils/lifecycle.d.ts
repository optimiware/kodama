import { LLMEvent, type FinishReasonDetails, type ProviderMetadata, type Usage } from "../../schema/index.js";
export interface State {
    readonly stepStarted: boolean;
    readonly text: ReadonlySet<string>;
    readonly reasoning: ReadonlySet<string>;
}
export declare const initial: () => State;
export declare const stepStart: (state: State, events: LLMEvent[]) => State;
export declare const textStart: (state: State, events: LLMEvent[], id: string, providerMetadata?: ProviderMetadata) => State;
export declare const textDelta: (state: State, events: LLMEvent[], id: string, text: string, providerMetadata?: ProviderMetadata) => State;
export declare const reasoningStart: (state: State, events: LLMEvent[], id: string, providerMetadata?: ProviderMetadata) => State;
export declare const reasoningDelta: (state: State, events: LLMEvent[], id: string, text: string, providerMetadata?: ProviderMetadata) => State;
export declare const reasoningEnd: (state: State, events: LLMEvent[], id: string, providerMetadata?: ProviderMetadata, 
/** Authoritative complete value; replaces accumulated deltas when present. */
text?: string) => State;
export declare const textEnd: (state: State, events: LLMEvent[], id: string, providerMetadata?: ProviderMetadata, 
/** Authoritative complete value; replaces accumulated deltas when present. */
text?: string) => State;
export declare const finish: (state: State, events: LLMEvent[], input: {
    readonly reason: FinishReasonDetails;
    readonly usage?: Usage;
    readonly providerMetadata?: ProviderMetadata;
}) => State;
export * as Lifecycle from "./lifecycle.js";
