import type { ProviderPackage } from "../../provider-package.js";
import type { GoogleInteractions } from "../../protocols/google-interactions.js";
export type Settings = ProviderPackage.Settings & GoogleInteractions.ProviderOptionsInput & {
    readonly apiKey?: string;
    readonly baseURL?: string;
};
export declare const model: ProviderPackage.Definition<Settings, GoogleInteractions.ProviderOptionsInput>["model"];
