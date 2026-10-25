export * as SharedEvents from "./shared-events.js";
export type SubscribeOptions = {
    readonly signal?: AbortSignal;
    /** Reports transport activity on the shared stream, including keepalive frames that carry no event. */
    readonly onActivity?: () => void;
};
export declare function make<A extends {
    readonly type: string;
}>(connect: (signal: AbortSignal, onActivity: () => void) => AsyncIterable<A>): {
    subscribe(options?: SubscribeOptions): AsyncIterable<A>;
};
