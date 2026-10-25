import { Writable } from "node:stream";
import { ResourceContext } from "./buffer.js";
import { NativeSessionRenderStatus, type NativeContextHandle, type NativeContextOptions, type NativeSceneFrameRequest, type NativeSessionCapabilities, type NativeSessionControl, type NativeSessionKittyImageTransportStatus, type NativeSessionOptions, type NativeSessionRendererOptions, type NativeSessionTerminalOptions, type RenderLib, type SessionHandle, type NativeSplitSnapshot } from "./zig.js";
import type { ProgramStatus } from "./types.js";
export interface NativeSessionScheduler {
    /** Monotonic nanoseconds. */
    now(): bigint;
    /** Schedule a later event-loop turn, never an inline callback or microtask. */
    schedule(callback: () => void, delayMs?: number): () => void;
}
export interface NativeSessionDriverOptions {
    context?: NativeContextOptions;
    output?: NativeSessionOptions;
    /** Copy-buffer size for custom Writable destinations. Native stdout needs no buffer. */
    outputBufferSize?: number;
    /** Cancel if graceful close has not completed within this finite interval. */
    closeTimeoutMs?: number;
    scheduler?: NativeSessionScheduler;
}
/**
 * Owns one checked Session and optionally its Context. Borrows a Writable without
 * ending or destroying it. If emitClose is false, the owner must call dispose() again
 * after silent asynchronous destruction completes; no notification is available.
 */
export declare class NativeSession {
    private readonly sink;
    private readonly owner?;
    private readonly lib;
    private attachmentCleanup?;
    readonly resourceContext: ResourceContext;
    private readonly nativeSession;
    private buffer;
    private readonly outputBufferSize;
    private readonly sinkWrite;
    /** @internal Shared later-turn scheduler; frame and output cancellations remain independent. */
    readonly scheduler: NativeSessionScheduler;
    /** Configured output capacity, including reserved control storage; raw admission can fail earlier. */
    readonly maxWriteBytes: bigint;
    /** Maximum raw write in an empty queue, excluding reserved control storage and span slots. */
    readonly maxAtomicWriteBytes: bigint;
    private readonly closeTimeoutNs;
    private readonly closeWait;
    private idleWait;
    private presentationWait;
    private transition;
    private scheduled;
    private output;
    private blocked;
    private sinkErrorSeen;
    private detached;
    private closeDeadlineNs;
    private closeRequested;
    private closeInterruption;
    private stopped;
    private exiting;
    private finishing;
    private _disposed;
    private _error;
    private readonly detachedSessions;
    /** Resolves after graceful native close and owned resource cleanup; failure or cancellation rejects. */
    readonly closed: Promise<void>;
    constructor(sink: Writable, options?: NativeSessionDriverOptions, owner?: NativeSession | undefined);
    get context(): NativeContextHandle;
    get renderLib(): RenderLib;
    get session(): Readonly<SessionHandle>;
    /** Native Session teardown is complete; pending sink I/O can still retain listeners. */
    get disposed(): boolean;
    get contextDisposed(): boolean;
    get error(): Error | null;
    usesOutput(sink: Writable): boolean;
    /** The optional attachment cleanup is installed only after checked native acceptance. */
    attachRenderer(options: NativeSessionRendererOptions, beforeDispose?: () => void): void;
    /** @internal Independent scene/frame ownership, sharing only the parent's Context resources. */
    createDetached(options: NativeSessionRendererOptions, beforeDispose: () => void): NativeSession;
    /** Repeated calls share pending setup; the first call supplies its options. */
    setupTerminal(options?: NativeSessionTerminalOptions): Promise<void>;
    suspend(): Promise<void>;
    resume(): Promise<void>;
    /** The painted frame is consumed on every returned status. */
    render(force: boolean, frame: NativeSceneFrameRequest): NativeSessionRenderStatus;
    /** @internal One checked admission for a complete split frame. */
    renderSplit(frame: NativeSceneFrameRequest | null, commits: readonly NativeSplitSnapshot[], pinnedRenderOffset: number, force: boolean): {
        status: NativeSessionRenderStatus;
    };
    /** @internal Screen changes join the same ordered output queue as frames and controls. */
    setScreen(alternate: boolean, width: number, height: number, trailingOutput?: Uint8Array): void;
    /** Waits for the current frame's endpoint, not later output or sink drain. Calls share one pending wait. */
    whenPresented(): Promise<void>;
    resize(width: number, height: number): void;
    control(command: NativeSessionControl): void;
    writeClipboard(target: number, bytes: Uint8Array): boolean;
    triggerNotification(message: string, title?: string): boolean;
    /** Returns false while the terminal is inactive or output is full. Invalid reports throw. */
    setProgramStatus(status: ProgramStatus): boolean;
    setKittyImageTransport(mode: number): void;
    getKittyImageTransport(): NativeSessionKittyImageTransportStatus;
    pollKittyImageTransport(): boolean;
    cancelKittyImageTransport(failed: boolean): void;
    processKittyImageReply(bytes: Uint8Array): number;
    startKittyFileProbe(): void;
    getCapabilities(): NativeSessionCapabilities;
    /** Copies all bytes or returns false under queue pressure. Capacity-exceeding writes throw RangeError;
     * lifecycle errors take precedence while writing is unavailable. */
    write(bytes: Uint8Array): boolean;
    /** Waits for accepted output, terminal work, and sink pressure to settle. Calls share one wait. */
    idle(): Promise<void>;
    /** Release external Context leases first. A busy close rejects; dispose() can retry cleanup after release. */
    close(): Promise<void>;
    /** @internal Bound required snapshot flushing by the same graceful-close deadline. */
    armCloseTimeout(): void;
    /** Identifies this driver's close interruption, not transport or restoration success. */
    isCloseInterruption(error: unknown): boolean;
    /**
     * Cancels immediately without claiming restoration. Release external Context
     * leases first; if disposed remains false, release them and call dispose again.
     */
    dispose(): void;
    /** @internal Restore before exit callbacks can terminate the process. The caller
     * must still dispose after wrapper cleanup; this does not release the Context.
     * Only real stdout supports direct synchronous delivery. In-flight writes cannot
     * be replayed, and arbitrary borrowed sinks cannot finish asynchronously at exit. */
    restoreOnExit(): boolean;
    private checkOpen;
    private changeTerminal;
    private onError;
    private onClose;
    private onFinish;
    private onDrain;
    private schedule;
    private turn;
    private writeOutput;
    private writeSink;
    private completePresentation;
    private detach;
    private canDetach;
    private finish;
}
