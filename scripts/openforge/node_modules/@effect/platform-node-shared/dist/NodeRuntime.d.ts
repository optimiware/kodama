/**
 * Node-compatible process runner for Effect programs.
 *
 * This module provides the shared `runMain` implementation used by
 * Node-compatible platform packages. It runs one Effect as the main process
 * fiber, interrupts that fiber on `SIGINT` or `SIGTERM`, and delegates final
 * exit-code handling to the configured teardown.
 *
 * @since 4.0.0
 */
import type { Effect } from "effect/Effect";
import * as Runtime from "effect/Runtime";
/**
 * Runs an Effect as the Node process main program, interrupting the fiber on
 * `SIGINT` or `SIGTERM` and invoking the configured teardown to determine the
 * process exit code.
 *
 * @category running
 * @since 4.0.0
 */
export declare const runMain: {
    /**
     * Runs an Effect as the Node process main program, interrupting the fiber on
     * `SIGINT` or `SIGTERM` and invoking the configured teardown to determine the
     * process exit code.
     *
     * @category running
     * @since 4.0.0
     */
    (options?: {
        readonly disableErrorReporting?: boolean | undefined;
        readonly teardown?: Runtime.Teardown | undefined;
    }): <E, A>(effect: Effect<A, E>) => void;
    /**
     * Runs an Effect as the Node process main program, interrupting the fiber on
     * `SIGINT` or `SIGTERM` and invoking the configured teardown to determine the
     * process exit code.
     *
     * @category running
     * @since 4.0.0
     */
    <E, A>(effect: Effect<A, E>, options?: {
        readonly disableErrorReporting?: boolean | undefined;
        readonly teardown?: Runtime.Teardown | undefined;
    }): void;
};
//# sourceMappingURL=NodeRuntime.d.ts.map