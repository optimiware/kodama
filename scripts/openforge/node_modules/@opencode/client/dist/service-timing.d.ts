export type EnsureTiming = {
    readonly pollInterval: number;
    readonly requestTimeout: number;
    readonly spawnDelay: number;
    readonly maxSpawnDelay: number;
    readonly promiseTimeout: number;
    readonly stopPollInterval: number;
    readonly stopPollAttempts: number;
};
export declare const defaultEnsureTiming: EnsureTiming;
export declare function ensureTiming(options: object): EnsureTiming;
export declare function withEnsureTiming<A extends object>(options: A, overrides: Partial<EnsureTiming>): A;
