import { Context, Layer } from "effect";
export type LogLevel = "trace" | "debug" | "info" | "warn" | "error" | "fatal";
export type LogEntry = {
    readonly level: LogLevel;
    readonly message: string;
    readonly attributes?: Readonly<Record<string, unknown>>;
    readonly cause?: unknown;
};
export type LogWriter = (entry: LogEntry) => void;
export type LogOptions = {
    readonly level?: LogLevel;
    readonly emit: LogWriter;
};
export declare function layer(log?: LogOptions): Layer.Layer<never, never, never>;
export declare function context(source: Context.Context<never>): Context.Context<never>;
