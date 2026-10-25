import type { Effect } from "effect/Effect";
import type { Layer } from "effect/Layer";
import type { Scope } from "effect/Scope";
import type { Terminal, UserInput } from "effect/Terminal";
/**
 * Creates a scoped `Terminal` service backed by process stdin/stdout, using the
 * optional predicate to decide when key input should end the input stream.
 *
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (shouldQuit?: (input: UserInput) => boolean) => Effect<Terminal, never, Scope>;
/**
 * Provides the default process-backed `Terminal` service, ending key input on
 * the default quit keys.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer<Terminal>;
//# sourceMappingURL=NodeTerminal.d.ts.map