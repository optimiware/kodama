export type RuntimeModuleLoader = () => Record<string, unknown> | Promise<Record<string, unknown>>;
export declare function resolveHostPackageRoots(from?: string, packages?: readonly string[]): string[];
export declare function loadRuntimeModules(): Readonly<Record<string, RuntimeModuleLoader>>;
export declare function discoverPluginRuntimeSpecifiers(from?: string, packages?: readonly string[]): ReadonlyMap<string, string>;
export declare function pluginRuntimeLoaderCode(specifier: string, entries: ReadonlyMap<string, string>): string;
export declare function createLoader(resolved: string): RuntimeModuleLoader;
