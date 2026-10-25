import { type ChildProcess } from "node:child_process";
import type { EnsureTiming } from "./service-timing.js";
export type ServiceContender = {
    readonly child: ChildProcess;
    readonly error: () => Error | undefined;
    readonly closed: () => boolean;
    readonly stderr: () => string;
    readonly release: () => void;
};
export declare function spawnServiceContender(command: string, args: ReadonlyArray<string>, env?: Readonly<Record<string, string | undefined>>): ServiceContender;
/**
 * The startup attempts of one `ensure()` call. It keeps at most two attempts alive, remembers
 * the first startup failure, and backs off when attempts exit cleanly because another one won.
 */
export declare function contenderPool(timing: EnsureTiming): {
    /** The first startup failure seen since the last eviction. */
    failure: () => Error | undefined;
    /** A registered service answered, so the next attempt waits the base delay again. */
    serviceAnswered(): void;
    /**
     * The owner `pid` was replaced. Drop its attempt and any finished ones, forget their
     * failure, and restart the spawn clock.
     */
    evict(pid: number): void;
    /** Let the next attempt start without waiting a spawn delay. */
    recruitNow(): void;
    /** Collect finished attempts. Returns the startup failure once no attempt is left alive. */
    reap(): Error | undefined;
    /**
     * Whether to start another attempt now. A registration that has not answered yet gets one
     * spawn delay before an attempt competes with it.
     */
    shouldRecruit(registered: boolean): boolean;
    add(contender: ServiceContender): void;
    releaseAll(): void;
};
export declare function contenderFinished(contender: ServiceContender): boolean;
