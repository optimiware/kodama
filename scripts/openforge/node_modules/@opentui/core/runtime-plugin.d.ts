import { type BunPlugin } from "bun";
export type RuntimeModuleExports = Record<string, unknown>;
export type RuntimeModuleLoader = () => RuntimeModuleExports | Promise<RuntimeModuleExports>;
export type RuntimeModuleEntry = RuntimeModuleExports | RuntimeModuleLoader;
export type RuntimeSpecifierPreserve = readonly string[] | ReadonlySet<string> | ((specifier: string) => boolean);
export interface RuntimePluginRewriteOptions {
    nodeModulesRuntimeSpecifiers?: boolean;
    nodeModulesBareSpecifiers?: boolean;
}
export interface CreateRuntimePluginOptions {
    core?: RuntimeModuleEntry;
    additional?: Record<string, RuntimeModuleEntry>;
    preserve?: RuntimeSpecifierPreserve;
    rewrite?: RuntimePluginRewriteOptions;
}
export declare const isCoreRuntimeModuleSpecifier: (specifier: string) => boolean;
export declare const runtimeModuleIdForSpecifier: (specifier: string) => string;
export declare function createRuntimePlugin(input?: CreateRuntimePluginOptions): BunPlugin;
