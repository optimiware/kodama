import type { SessionMessageAssistant, SessionMessageInfo } from "../promise";
/**
 * The assistant step whose tokens fill the context window now: the latest one after the last completed
 * compaction and before the revert boundary. Every client shows context usage from this step, so the TUI
 * and the desktop app agree on it.
 */
export declare function lastAssistantWithUsage(messages: ReadonlyArray<SessionMessageInfo>, boundary?: string): (SessionMessageAssistant & {
    tokens: NonNullable<SessionMessageAssistant["tokens"]>;
}) | undefined;
