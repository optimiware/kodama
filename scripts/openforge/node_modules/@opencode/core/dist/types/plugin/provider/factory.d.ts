import type { AISDKHooks } from "@opencode/plugin/effect/aisdk";
export declare function createProviderPlugin(input: {
    readonly id: string;
    readonly package: string;
    readonly load: (options: AISDKHooks["sdk"]["options"]) => Promise<unknown>;
}): import("@opencode/plugin/effect/plugin").Plugin<import("effect/Scope").Scope>;
