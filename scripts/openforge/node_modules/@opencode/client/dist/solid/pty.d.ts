import type { ExperimentalPersistentPtyConnectTokenInput, OpenCodeClient, PtyConnectTokenInput } from "../promise";
export type PtyClientOptions = {
    readonly url: string;
    readonly openSocket?: (url: URL) => WebSocket;
};
export type PtyConnectInput = {
    readonly ptyID: PtyConnectTokenInput["ptyID"];
    readonly location?: PtyConnectTokenInput["location"];
    readonly cursor?: number;
};
export type PersistentPtyConnectInput = {
    readonly ptyID: ExperimentalPersistentPtyConnectTokenInput["ptyID"];
    readonly cursor: number;
    readonly attachmentID: string;
    readonly takeover?: boolean;
};
export declare function createPtyClient(api: OpenCodeClient, options: PtyClientOptions): {
    connect(input: PtyConnectInput): Promise<WebSocket>;
};
export declare function createPersistentPtyClient(api: OpenCodeClient, options: PtyClientOptions): {
    connect(input: PersistentPtyConnectInput): Promise<WebSocket>;
};
