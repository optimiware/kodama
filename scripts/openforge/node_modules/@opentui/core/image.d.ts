import { ResourceContext } from "./buffer.js";
import { type ContextImageHandle, type NativeContextHandle, type ImageHandle, type RenderLib } from "./zig.js";
export type ImageFormat = "png" | "raw-rgba" | "jpeg" | "webp" | "gif";
export type ImageColorStatus = "assumed-srgb" | "explicit-srgb";
export type ResizeKernel = "default" | "area" | "triangle" | "cubic-bspline" | "catmull-rom" | "mitchell" | "nearest";
export type BlendMode = "source-over" | "source" | "destination-over";
export type PixelFormat = "rgba8" | "bgra8";
export type ImageSource = string | URL | Uint8Array | ArrayBuffer | Blob | Response;
export type ImageLoadErrorCode = "file-read" | "network" | "http-status" | "unsupported-url-scheme";
export declare class ImageLoadError extends Error {
    readonly code: ImageLoadErrorCode;
    readonly source: string;
    readonly status?: number;
    constructor(code: ImageLoadErrorCode, source: string, message: string, options?: {
        cause?: unknown;
        status?: number;
    });
}
export interface ImageCreateOptions {
    owner?: ResourceContext;
}
export interface ImageLoadOptions extends ImageCreateOptions {
    signal?: AbortSignal;
    fetch?: (input: URL, init?: RequestInit) => Promise<Response>;
}
export interface ImageInfo {
    width: number;
    height: number;
    sourceWidth: number;
    sourceHeight: number;
    format: ImageFormat;
    colorStatus: ImageColorStatus;
    orientation: number;
    hasAlpha: boolean;
}
export interface ResizeOptions {
    width?: number;
    height?: number;
    kernel?: ResizeKernel;
}
export interface ExtractOptions {
    left: number;
    top: number;
    width: number;
    height: number;
}
export interface ExtendOptions {
    top?: number;
    right?: number;
    bottom?: number;
    left?: number;
    background?: readonly [number, number, number, number];
}
export interface CompositeOptions {
    left?: number;
    top?: number;
    blend?: BlendMode;
    opacity?: number;
}
export interface RawImage {
    data: Uint8Array;
    width: number;
    height: number;
    stride: number;
    format: PixelFormat;
    colorSpace: "srgb";
    alpha: "straight";
}
export interface PixelImportOptions extends ImageCreateOptions {
    stride?: number;
    format?: PixelFormat;
    alpha?: "straight" | "opaque";
    colorSpace?: "srgb";
}
export interface OwnedRawImage extends RawImage {
    dispose(): void;
}
export type ImageErrorCode = "invalid-handle" | "unsupported-format" | "unsupported-color-space" | "malformed-data" | "dimension-limit" | "memory-limit" | "invalid-argument" | "out-of-memory" | "output-too-small" | "internal-error" | "unsupported-feature" | "busy";
export declare class ImageError extends Error {
    readonly code: ImageErrorCode;
    readonly status: number;
    constructor(status: number);
}
export declare function imageInfo(data: Uint8Array | ArrayBuffer, options?: ImageCreateOptions): ImageInfo;
export declare class NativeImage {
    private readonly owner;
    private readonly lib;
    private handle;
    private imageInfo;
    private contextImages;
    private constructor();
    private static create;
    static decode(data: Uint8Array | ArrayBuffer, options?: ImageCreateOptions): NativeImage;
    static load(source: ImageSource, options?: ImageLoadOptions): Promise<NativeImage>;
    static fromRgba(pixels: Uint8Array, width: number, height: number, stride?: number, options?: ImageCreateOptions): NativeImage;
    static fromPixels(pixels: Uint8Array, width: number, height: number, options?: PixelImportOptions): NativeImage;
    private static fromHandle;
    private guard;
    get ptr(): ImageHandle;
    /** @internal Shares same-Context resources; copies immutable storage across Contexts. */
    _getContextHandle(lib: RenderLib, context: NativeContextHandle): ContextImageHandle;
    private releaseContextImages;
    private wrap;
    info(): ImageInfo;
    get width(): number;
    get height(): number;
    clone(): NativeImage;
    retain(): NativeImage;
    resize(options: ResizeOptions): NativeImage;
    extract(options: ExtractOptions): NativeImage;
    extend(options?: ExtendOptions): NativeImage;
    rotate(angle: 90 | 180 | 270): NativeImage;
    flip(): NativeImage;
    flop(): NativeImage;
    composite(overlay: NativeImage, options?: CompositeOptions): NativeImage;
    ensureEncodedPng(): void;
    raw(format?: PixelFormat): RawImage;
    takeRaw(): OwnedRawImage;
    copyTo(destination: Uint8Array, options?: {
        stride?: number;
        format?: PixelFormat;
    }): void;
    dispose(): void;
}
export interface NativeImagePoolOptions extends ImageCreateOptions {
    width: number;
    height: number;
    capacity?: number;
}
export declare class NativeImagePool {
    private readonly lib;
    private readonly owner?;
    private readonly images;
    private disposed;
    readonly width: number;
    readonly height: number;
    readonly capacity: number;
    constructor(options: NativeImagePoolOptions);
    publishRgba(pixels: Uint8Array, stride?: number): NativeImage | null;
    publishPixels(pixels: Uint8Array, options?: PixelImportOptions): NativeImage | null;
    dispose(): void;
}
