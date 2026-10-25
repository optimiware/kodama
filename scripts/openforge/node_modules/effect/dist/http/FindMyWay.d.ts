/**
 * Configuration for router path matching.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface RouterConfig {
    readonly ignoreTrailingSlash: boolean;
    readonly ignoreDuplicateSlashes: boolean;
    readonly caseSensitive: boolean;
    readonly maxParamLength: number;
}
/**
 * Route path accepted by the router.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type PathInput = `/${string}` | "*";
/**
 * Mutable router used to register and resolve route handlers.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Router<A> {
    readonly on: (method: string | Iterable<string>, path: PathInput, handler: A) => void;
    readonly all: (path: PathInput, handler: A) => void;
    readonly find: (method: string, url: string) => FindResult<A> | undefined;
    readonly has: (method: string, url: string) => boolean;
}
/**
 * Result of a successful route lookup.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface FindResult<A> {
    readonly handler: A;
    readonly params: Record<string, string | undefined>;
    readonly searchParams: Record<string, string | Array<string>>;
}
/**
 * Creates an empty mutable router.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const make: <A>(options?: Partial<RouterConfig>) => Router<A>;
//# sourceMappingURL=FindMyWay.d.ts.map