export * as PtyHandoff from "./pty-handoff.js";
import type { Info } from "./service.js";
/**
 * Publish the ticket before stopping its owner so every replacement contender can adopt it.
 * Never throws: terminals are best-effort, so any failure replaces the service without a handoff.
 */
export declare function prepare(file: string, info: Info, timeout: number): Promise<void>;
export declare function environment(file: string, env?: Readonly<Record<string, string>>): Promise<{
    OPENCODE_PTY_HANDOFF: string | undefined;
}>;
export declare function complete(file: string, info: Info): Promise<void>;
export declare function clear(file: string): Promise<void>;
