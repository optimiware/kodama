import type { ProviderPackage } from "../../provider-package.js";
import { MiniMax } from "../minimax.js";
export type Settings = MiniMax.Settings<MiniMax.ChatOptionsInput>;
export declare const model: ProviderPackage.Definition<Settings, MiniMax.ChatOptionsInput>["model"];
