import type { BufferAccess } from "../buffer.js";
import type { NativeContextBufferLease, NativeContextHandle, RenderLib } from "../zig.js";
type BufferSnapshot = Omit<NativeContextBufferLease, "handle">;
export declare function withBufferAccess<T>(lib: RenderLib, context: NativeContextHandle, lease: NativeContextBufferLease, callback: (cells: BufferAccess) => T): T;
export declare function withLazyBufferAccess<T>(snapshot: BufferSnapshot | (() => BufferSnapshot), callback: (getCells: () => BufferAccess) => T, validate: () => void): T;
export {};
