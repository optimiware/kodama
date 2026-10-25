export interface ClipboardRepresentation {
    readonly mimeType: string;
    readonly bytes: Uint8Array;
}
export type ClipboardSelection = "clipboard" | "primary";
export interface ClipboardReadOptions {
    readonly preferredTypes: readonly [string, ...string[]];
    readonly selection?: ClipboardSelection;
    readonly signal?: AbortSignal;
}
export type ClipboardReadResult = {
    readonly status: "read";
    readonly representation: ClipboardRepresentation;
} | {
    readonly status: "empty" | "unsupported" | "cancelled" | "timed-out" | "limit-exceeded";
} | {
    readonly status: "failed";
    readonly error: Error;
};
export interface HostClipboardReadOptions {
    readonly preferredTypes: readonly [string, ...string[]];
    readonly selection: ClipboardSelection;
    readonly maxBytes: number;
    readonly timeoutMs: number;
    readonly signal: AbortSignal;
}
export interface HostClipboardWriteOptions {
    readonly selection: ClipboardSelection;
    readonly timeoutMs: number;
    readonly signal: AbortSignal;
}
export type HostClipboardWriteResult = {
    readonly status: "written" | "unsupported" | "cancelled" | "timed-out";
} | {
    readonly status: "failed";
    readonly error: Error;
};
export type HostClipboardClearResult = {
    readonly status: "cleared" | "unsupported" | "cancelled" | "timed-out";
} | {
    readonly status: "failed";
    readonly error: Error;
};
export interface HostClipboardBackend {
    read(options: HostClipboardReadOptions): Promise<ClipboardReadResult>;
    writeText(text: string, options: HostClipboardWriteOptions): Promise<HostClipboardWriteResult>;
    clear(options: HostClipboardWriteOptions): Promise<HostClipboardClearResult>;
    dispose(): Promise<void>;
}
export type ClipboardWriteDestination = "terminal-only" | "host-only" | "best-available" | "all-available";
export interface ClipboardWriteOptions {
    readonly destination: ClipboardWriteDestination;
    readonly selection?: ClipboardSelection;
    readonly allowRemoteHost?: boolean;
    readonly signal?: AbortSignal;
}
export interface TerminalClipboardOperationResult {
    readonly status: "attempted" | "local-failure" | "not-attempted";
    readonly capability: "supported" | "unsupported" | "unknown";
}
export interface ClipboardWriteResult {
    readonly host: HostClipboardWriteResult | {
        readonly status: "not-attempted";
    };
    readonly terminal: TerminalClipboardOperationResult;
}
export interface ClipboardClearResult {
    readonly host: HostClipboardClearResult | {
        readonly status: "not-attempted";
    };
    readonly terminal: TerminalClipboardOperationResult;
}
export interface ClipboardService {
    read(options: ClipboardReadOptions): Promise<ClipboardReadResult>;
    writeText(text: string, options: ClipboardWriteOptions): Promise<ClipboardWriteResult>;
    clear(options: ClipboardWriteOptions): Promise<ClipboardClearResult>;
    dispose(): Promise<void>;
}
export interface HostClipboardOperationOptions {
    readonly selection?: ClipboardSelection;
    readonly signal?: AbortSignal;
}
export interface HostClipboardService {
    readonly maxWriteBytes: number;
    read(options: ClipboardReadOptions): Promise<ClipboardReadResult>;
    writeText(text: string, options?: HostClipboardOperationOptions): Promise<HostClipboardWriteResult>;
    clear(options?: HostClipboardOperationOptions): Promise<HostClipboardClearResult>;
    dispose(): Promise<void>;
}
export interface HostClipboardOptions {
    readonly timeoutMs?: number;
    readonly maxReadBytes?: number;
    readonly maxWriteBytes?: number;
    readonly maxImagePixels?: number;
    readonly maxConversionBytes?: number;
    readonly maxConcurrentOperations?: number;
    readonly maxProviderTransfers?: number;
    readonly waylandSeat?: string;
}
export interface TerminalClipboardAdapter {
    readonly remote: boolean;
    writeText(text: string, selection: ClipboardSelection): TerminalClipboardOperationResult;
    clear(selection: ClipboardSelection): TerminalClipboardOperationResult;
}
export interface ClipboardOptions {
    readonly host: HostClipboardService;
    readonly terminal: TerminalClipboardAdapter;
}
export declare const createHostClipboard: (options?: HostClipboardOptions) => HostClipboardService;
export declare const createClipboard: ({ host, terminal }: ClipboardOptions) => ClipboardService;
export declare enum ClipboardTarget {
    Clipboard = 0,
    Primary = 1,
    Select = 2,
    Secondary = 3
}
export interface RendererClipboardBoundary {
    readonly capabilities: {
        readonly remote: boolean;
        readonly osc52_support: "supported" | "unsupported" | "unknown";
    } | null;
    copyToClipboardOSC52(text: string, target?: ClipboardTarget): boolean;
    clearClipboardOSC52(target?: ClipboardTarget): boolean;
}
export declare const createRendererClipboardAdapter: (renderer: RendererClipboardBoundary) => TerminalClipboardAdapter;
