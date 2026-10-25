/**
 * Tools for standalone-executable native library embedding.
 *
 * `bun build --compile` only bundles files referenced by statically analyzable
 * imports. Runtime resolution (./download.ts) is invisible to the bundler, so
 * we additionally reference the platform's native lib through a `type: "file"`
 * import. Bun then embeds it and returns a `$bunfs` path inside the compiled
 * binary (and the real on-disk path under `bun run`).
 *
 * Linux libc cannot be detected at build time, so it is supplied via the
 * FFF_LIBC build constant (`bun build --define FFF_LIBC='"musl"'`), defaulting
 * to glibc. macOS/Windows need no define.
 */
export declare const embeddedLibPath: string | null;
//# sourceMappingURL=embedded.d.ts.map