import type { CacheHint } from "../../schema/index.js";
export interface Breakpoints {
    remaining: number;
    dropped: number;
}
export declare const newBreakpoints: (cap: number) => Breakpoints;
export declare const ttlBucket: (ttlSeconds: number | undefined) => "1h" | undefined;
export declare const cacheControl: () => (cache: CacheHint | undefined) => {
    type: "ephemeral";
    ttl: "1h" | undefined;
} | undefined;
