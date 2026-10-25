import type { CliRenderer } from "../renderer.js";
import { NativeSessionRenderStatus } from "../zig.js";
/** Runs `turns` host turns, so ready frames, Session pumps, and stream callbacks can run. */
export declare function settle(turns?: number): Promise<void>;
/** Runs host turns until `predicate` holds. Throws after `maxTurns`, so a stalled renderer fails fast. */
export declare function settleUntil(predicate: () => boolean, maxTurns?: number): Promise<void>;
/**
 * Runs host turns until no frame renders, no ready frame is queued, and no output-idle retry waits. Timer-driven
 * frames are not run; advance the clock first. Reads scheduler internals that `getSchedulerState()` merges.
 */
export declare function serviceReadyFrames(renderer: CliRenderer, maxTurns?: number): Promise<void>;
/**
 * Replaces the Session frame commit. `next()` picks the status of each commit: Presented commits normally; any other
 * status consumes the frame without output, as the native Session does under pressure. Returns a restore function.
 */
export declare function forceRenderStatus(renderer: CliRenderer, next: () => NativeSessionRenderStatus): () => void;
export interface HeldOutputIdle {
    /** Resolves the held wait; later `idle()` calls pass through until `hold()`. */
    release(): Promise<void>;
    hold(): void;
    /** Number of `idle()` calls since the hold started. */
    calls(): number;
    restore(): void;
}
/** Holds every Session `idle()` wait, so a skipped frame's output-idle retry runs only on `release()`. */
export declare function holdOutputIdle(renderer: CliRenderer): HeldOutputIdle;
/** Counts the process listeners that a renderer installs. Take it before construction. */
export declare function processListenerCounts(): Map<string, number>;
/**
 * Waits for `closed` (a Session failure is allowed) and checks that the renderer released everything it acquired:
 * process listeners (against `before`), its stdin listener, the stream lease, the root tree, and the Session.
 */
export declare function assertRendererReleased(renderer: CliRenderer, before: Map<string, number>): Promise<void>;
