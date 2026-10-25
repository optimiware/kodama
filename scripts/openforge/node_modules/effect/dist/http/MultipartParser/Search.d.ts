/**
 * Creates an incremental byte search for a string boundary.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (needle: string, callback: (index: number, chunk: Uint8Array) => void) => {
    readonly write: (chunk: Uint8Array) => void;
    readonly end: () => void;
};
//# sourceMappingURL=Search.d.ts.map