import type { ProviderPackage } from "../../provider-package.js";
import { Cohere } from "../cohere.js";
export type Settings = Cohere.Settings<Cohere.ChatOptionsInput>;
export declare const model: ProviderPackage.Definition<Settings, Cohere.ChatOptionsInput>["model"];
