import type { OpenCodeClient, OpenCodeEvent } from "../promise";
export type ClientConnectionStatus = "connected" | "connecting" | "reconnecting";
export type ClientConnectionEvent = {
    readonly type: "client.connection";
    readonly created: number;
    readonly data: {
        readonly status: "connecting" | "connected" | "disconnected" | "reconnecting";
        readonly attempt: number;
        readonly error?: string;
    };
};
export type ClientConnectionOptions = {
    readonly reconnect?: (signal: AbortSignal) => Promise<OpenCodeClient>;
    readonly onEvent: (event: OpenCodeEvent) => void;
    readonly flushInterval?: number;
    readonly pageLifecycle?: boolean;
    /**
     * Abort and reconnect a stream that receives no bytes for this long. The server writes a keepalive
     * comment every 15 seconds, so a quiet but healthy stream never trips this.
     */
    readonly idleTimeout?: number;
    readonly log?: {
        readonly debug?: (message: string, data?: Readonly<Record<string, unknown>>) => void;
        readonly info?: (message: string, data?: Readonly<Record<string, unknown>>) => void;
    };
};
export declare const defaultIdleTimeout = 45000;
export declare const foregroundIdleThreshold = 20000;
export declare function createClientConnection(initialApi: OpenCodeClient, options: ClientConnectionOptions): {
    status: () => ClientConnectionStatus;
    attempt: () => number;
    error: () => string | undefined;
    internal: {
        history: () => ClientConnectionEvent[];
        resync: (reason: string) => void;
    };
};
