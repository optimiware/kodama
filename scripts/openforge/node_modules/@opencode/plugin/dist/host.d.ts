export * as Host from "./host.js";
export interface Target {
    readonly directory: string;
    readonly name?: string;
}
export interface Entrypoints {
    readonly server?: string;
    readonly tui?: string;
    readonly rpc?: string;
}
export declare function resolve(target: Target): Entrypoints;
export declare function load(entrypoint: string): Promise<unknown>;
