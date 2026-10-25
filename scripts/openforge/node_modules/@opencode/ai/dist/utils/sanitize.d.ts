/**
 * Replace lone UTF-16 surrogates in every string, key and nested value. Unchanged
 * subtrees are returned by reference so requests with large histories are not copied.
 */
export declare const sanitizeSurrogates: <T>(value: T) => T;
