import { type RuntimeModuleLoader } from "./runtime-modules.js";
export declare function provides(specifier: string): boolean;
export declare function ensurePluginRuntime(): Readonly<Record<string, RuntimeModuleLoader>>;
export declare function createForeignPackageFilter(rootsInput?: Iterable<string>): RegExp;
