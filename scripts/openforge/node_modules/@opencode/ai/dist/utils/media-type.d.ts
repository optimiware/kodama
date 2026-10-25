/**
 * Sniff a media type from leading magic bytes. Covers the containers media routes commonly return; anything else is
 * `undefined` so callers can fall back to a provider-declared type or `application/octet-stream`.
 */
export declare const detectMediaType: (bytes: Uint8Array) => string | undefined;
/** Media type of a file's contents: sniffed magic bytes, then the path's extension. */
export declare const fileMediaType: (bytes: Uint8Array, path: string) => string | undefined;
export declare const mediaTypeExtension: (mediaType: string) => string | undefined;
