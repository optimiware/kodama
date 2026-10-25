import type { DiscoverOptions, Endpoint, EnsureOptions, StopOptions } from "../service.js";
import { headers } from "../service-probe.js";
export * from "../service.js";
export { headers };
/** Discover a healthy, compatible local service without starting one. */
export declare function discover(options?: DiscoverOptions): Promise<{
    url: string;
    auth: {
        type: "basic";
        username: string;
        password: string;
    } | undefined;
} | undefined>;
/** Ensure a healthy, compatible local service is running. */
export declare function ensure(options?: EnsureOptions): Promise<Endpoint>;
/** Stop the registered local service. */
export declare function stop(options?: StopOptions): Promise<void>;
/** Promise-based local service lifecycle operations. */
export declare const Service: {
    discover: typeof discover;
    ensure: typeof ensure;
    stop: typeof stop;
    headers: typeof headers;
};
