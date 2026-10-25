/**
 * Bun FFI bindings for the fff-c native library
 *
 * This module uses Bun's native FFI to call into the Rust C library.
 * All functions follow the Result pattern for error handling.
 *
 * The API is instance-based: `ffiCreate` returns an opaque handle that must
 * be passed to all subsequent calls and freed with `ffiDestroy`.
 */
import { type JSCallback, type Pointer } from "bun:ffi";
import type { DirSearchResult, GrepResult, MixedSearchResult, Result, ScanProgress, SearchResult, WatchEvent } from "./fff-api";
/**
 * Opaque native handle type. Callers must not inspect or modify this value.
 */
export type NativeHandle = Pointer;
/**
 * Create a new file finder instance.
 *
 * Hand-encodes a [`FffCreateOptions`] struct (88 bytes, locked offsets — see
 * `crates/fff-c/src/ffi_types.rs::options_layout_tests`) into a Buffer and
 * passes its pointer to `fff_create_instance_with`. Inner cstring addresses
 * come from Bun's native `ptr(buffer)` primitive — no round-trip helpers,
 * no struct support gaps.
 *
 * Adding new options later means: (1) appending the field to
 * `FffCreateOptions` in Rust, (2) bumping `FFF_CREATE_OPTIONS_VERSION`,
 * (3) extending `FFF_CREATE_OPTIONS_SIZE` + offsets here. The C entry point
 * never changes.
 */
export declare function ffiCreate(basePath: string, frecencyDbPath: string, historyDbPath: string, _useUnsafeNoLock: boolean, enableMmapCache: boolean, enableContentIndexing: boolean, watch: boolean, aiMode: boolean, logFilePath: string, logLevel: string, cacheBudgetMaxFiles: bigint, cacheBudgetMaxBytes: bigint, cacheBudgetMaxFileSize: bigint, enableFsRootScanning: boolean, enableHomeDirScanning: boolean): Result<NativeHandle>;
/**
 * Destroy and clean up an instance.
 */
export declare function ffiDestroy(handle: NativeHandle): void;
/**
 * Perform fuzzy search.
 */
export declare function ffiSearch(handle: NativeHandle, query: string, currentFile: string, maxThreads: number, pageIndex: number, pageSize: number, comboBoostMultiplier: number, minComboCount: number): Result<SearchResult>;
/**
 * Glob-only search. Bypasses the regular query parser, applies the pattern
 * as a single `Constraint::Glob`, ranks by frecency, paginates.
 */
export declare function ffiGlob(handle: NativeHandle, pattern: string, currentFile: string, maxThreads: number, pageIndex: number, pageSize: number): Result<SearchResult>;
/**
 * Perform fuzzy directory search.
 */
export declare function ffiSearchDirectories(handle: NativeHandle, query: string, currentFile: string | null, maxThreads: number, pageIndex: number, pageSize: number): Result<DirSearchResult>;
/**
 * Perform mixed (files + directories) fuzzy search.
 */
export declare function ffiSearchMixed(handle: NativeHandle, query: string, currentFile: string, maxThreads: number, pageIndex: number, pageSize: number, comboBoostMultiplier: number, minComboCount: number): Result<MixedSearchResult>;
/**
 * Live grep - search file contents.
 */
export declare function ffiLiveGrep(handle: NativeHandle, query: string, mode: string, maxFileSize: number, maxMatchesPerFile: number, smartCase: boolean, fileOffset: number, pageLimit: number, timeBudgetMs: number, beforeContext: number, afterContext: number, classifyDefinitions: boolean): Result<GrepResult>;
/**
 * Multi-pattern grep - Aho-Corasick multi-needle search.
 */
export declare function ffiMultiGrep(handle: NativeHandle, patternsJoined: string, constraints: string, maxFileSize: number, maxMatchesPerFile: number, smartCase: boolean, fileOffset: number, pageLimit: number, timeBudgetMs: number, beforeContext: number, afterContext: number, classifyDefinitions: boolean): Result<GrepResult>;
/**
 * Trigger file scan.
 */
export declare function ffiScanFiles(handle: NativeHandle): Result<void>;
/**
 * Check if scanning.
 */
export declare function ffiIsScanning(handle: NativeHandle): boolean;
/**
 * Get the base path of the file picker.
 */
export declare function ffiGetBasePath(handle: NativeHandle): Result<string | null>;
/**
 * Get scan progress.
 */
export declare function ffiGetScanProgress(handle: NativeHandle): Result<ScanProgress>;
/**
 * Wait for scan to complete.
 */
export declare function ffiWaitForScan(handle: NativeHandle, timeoutMs: number): Result<boolean>;
/**
 * Restart index in new path.
 */
export declare function ffiRestartIndex(handle: NativeHandle, newPath: string): Result<void>;
/**
 * Parse an FffWatchEventBatch delivered to a watch callback, then free it.
 * Ownership of the batch transfers to JS at callback time, so this MUST be
 * called exactly once per delivered batch pointer.
 */
export declare function readWatchEventBatch(batchPtr: Pointer | number | null): WatchEvent[];
/**
 * Register the instance-wide watch callback. Must be called before the
 * first `ffiWatch`. The caller owns `callback` (a threadsafe `JSCallback`
 * built in finder.ts) and must keep it alive until after `ffiDestroy`
 * returns for this handle — that call is the delivery quiescence barrier.
 */
export declare function ffiSetWatchCallback(handle: NativeHandle, callback: JSCallback): Result<void>;
/**
 * Subscribe to filesystem changes; batches are delivered through the
 * instance callback registered with `ffiSetWatchCallback`, tagged with the
 * watch id this function returns.
 *
 * @returns The native watch id carried in `FffResult.int_value`.
 */
export declare function ffiWatch(handle: NativeHandle, pattern: string, ignore?: string[]): Result<number>;
/**
 * Remove a watch subscription. Returns true if the id was found.
 */
export declare function ffiUnwatch(handle: NativeHandle, watchId: number): Result<boolean>;
/**
 * Refresh git status.
 */
export declare function ffiRefreshGitStatus(handle: NativeHandle): Result<number>;
/**
 * Track query completion.
 */
export declare function ffiTrackQuery(handle: NativeHandle, query: string, filePath: string): Result<boolean>;
/**
 * Get historical query.
 */
export declare function ffiGetHistoricalQuery(handle: NativeHandle, offset: number): Result<string | null>;
/**
 * Health check.
 *
 * `handle` can be null for a limited check (version + git only).
 */
export declare function ffiHealthCheck(handle: NativeHandle | null, testPath: string): Result<unknown>;
/**
 * Ensure the library is loaded.
 *
 * Loads the native library from the platform-specific npm package
 * or a local dev build. Throws if the binary is not found.
 */
export declare function ensureLoaded(): void;
/**
 * Check if the library is available.
 */
export declare function isAvailable(): boolean;
//# sourceMappingURL=ffi.d.ts.map