import type { HostClipboardBackend, HostClipboardOptions, HostClipboardService } from "./clipboard.js";
export declare const HOST_CLIPBOARD_MIME_PREFERENCE_COUNT_MAX = 64;
export declare const HOST_CLIPBOARD_MIME_ESSENCE_BYTES_MAX = 255;
export interface NormalizedHostClipboardOptions {
    readonly timeoutMs: number;
    readonly maxReadBytes: number;
    readonly maxWriteBytes: number;
    readonly maxImagePixels: number;
    readonly maxConversionBytes: number;
    readonly maxConcurrentOperations: number;
    readonly maxProviderTransfers: number;
    readonly waylandSeat?: string;
}
export type HostClipboardBackendFactory = (options: NormalizedHostClipboardOptions) => HostClipboardBackend;
export interface ActiveClipboardOperation {
    readonly controller: AbortController;
    readonly settled: Promise<void>;
    settle(): void;
}
export declare const validateClipboardText: (text: string, maxWriteBytes: number) => void;
export declare const runTrackedOperation: <T>(active: Set<ActiveClipboardOperation>, callerSignal: AbortSignal | undefined, operation: (signal: AbortSignal) => Promise<T>) => Promise<T>;
export declare const createHostClipboardWithBackend: (options: HostClipboardOptions, createBackend: HostClipboardBackendFactory) => HostClipboardService;
