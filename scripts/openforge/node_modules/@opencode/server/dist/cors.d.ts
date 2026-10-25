import { Context } from "effect";
export type CorsOptions = {
    readonly cors?: ReadonlyArray<string>;
};
export declare const CorsConfig: Context.Reference<CorsOptions | undefined>;
export declare function isAllowedCorsOrigin(input: string | undefined, opts?: CorsOptions): boolean;
export declare function isAllowedRequestOrigin(input: string | undefined, host: string | undefined, opts?: CorsOptions): boolean;
