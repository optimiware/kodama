import * as Effect from "effect/Effect";
import * as Layer from "effect/Layer";
import type * as Scope from "effect/Scope";
import * as Terminal from "effect/Terminal";
/**
 * Creates a scoped process-backed `Terminal` using Node `readline`, enabling
 * TTY raw mode while in scope and using the supplied predicate to decide when
 * key input should end.
 *
 * @category constructors
 * @since 4.0.0
 */
export declare const make: (shouldQuit?: (input: Terminal.UserInput) => boolean) => Effect.Effect<Terminal.Terminal, never, Scope.Scope>;
/**
 * Provides the default process-backed `Terminal` service, ending key input on
 * Ctrl+C or Ctrl+D.
 *
 * @category layers
 * @since 4.0.0
 */
export declare const layer: Layer.Layer<Terminal.Terminal>;
//# sourceMappingURL=NodeTerminal.d.ts.map