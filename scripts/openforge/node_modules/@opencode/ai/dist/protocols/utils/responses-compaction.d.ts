import { Effect } from "effect";
import { AIError, LLMRequest } from "../../schema/index.js";
import type { CompactOperation } from "../../route/client.js";
import { OpenResponses } from "../open-responses.js";
export declare const make: (adapter: OpenResponses.ProviderAdapter, lowerTools: (request: LLMRequest) => Effect.Effect<ReadonlyArray<Record<string, unknown>>, AIError>) => CompactOperation;
export * as ResponsesCompaction from "./responses-compaction.js";
