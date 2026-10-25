type NativeSymbol = (...args: any[]) => any;
export declare const nativeSymbols: Record<string, NativeSymbol>;
/** Replaces native symbols while `fn` runs and records each call's arguments by symbol name. */
export declare function withStubbedSymbols(replacements: Record<string, NativeSymbol>, fn: (calls: Record<string, any[][]>) => void): void;
export {};
