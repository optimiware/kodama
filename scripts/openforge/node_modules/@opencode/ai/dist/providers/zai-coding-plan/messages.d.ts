import type { ProviderPackage } from "../../provider-package.js";
import { ZAICodingPlan } from "../zai-coding-plan.js";
export type Settings = ZAICodingPlan.Settings<ZAICodingPlan.MessagesOptionsInput>;
export declare const model: ProviderPackage.Definition<Settings, ZAICodingPlan.MessagesOptionsInput>["model"];
