import type { ProviderPackage } from "../../provider-package.js";
import { Moonshot } from "../moonshot.js";
export type Settings = Moonshot.Settings<Moonshot.ResponsesOptionsInput>;
export declare const model: ProviderPackage.Definition<Settings, Moonshot.ResponsesOptionsInput>["model"];
