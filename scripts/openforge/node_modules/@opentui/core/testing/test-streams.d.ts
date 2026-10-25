import { Writable, type WritableOptions } from "stream";
export declare class TestWriteStream extends Writable {
    readonly isTTY = true;
    readonly columns: number;
    readonly rows: number;
    constructor(columns?: number, rows?: number, options?: WritableOptions);
    _write(_chunk: any, _encoding: BufferEncoding, callback: (error?: Error | null) => void): void;
    getColorDepth(): number;
}
/**
 * Test stdout that copies every write. `hold()` parks the next write's completion until `release()`, which models a
 * slow terminal or socket: the Session keeps the frame pending and later output queued.
 */
export declare class RecordingWriteStream extends TestWriteStream {
    readonly writes: Buffer[];
    private held;
    private parked;
    _write(chunk: Uint8Array, _encoding: BufferEncoding, callback: (error?: Error | null) => void): void;
    /** True while a write waits for `release()`. */
    get pendingWrite(): boolean;
    hold(): void;
    /** Stops holding and completes the parked write, if any. */
    release(): void;
    bytes(): Buffer;
    text(): string;
    clear(): void;
}
export type TestStdout = TestWriteStream & NodeJS.WriteStream;
export declare function createTestStdin(): NodeJS.ReadStream;
export declare function createTestStdout(columns?: number, rows?: number): NodeJS.WriteStream;
