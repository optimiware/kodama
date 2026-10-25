import type { Extension } from "../extension.js";
import type { Interpreter } from "./interpreter.js";
import { type Value } from "./objects.js";
/**
 * The global bindings of one run's extensions. Everything crossing the boundary is converted: plain data and
 * built-in wrappers are copied, a host function becomes a program function whose calls cross the same way, and a
 * host Promise becomes a program promise.
 */
export declare const extensionGlobals: <R>(ctx: Interpreter<R>, extensions: ReadonlyArray<Extension>) => ReadonlyArray<readonly [string, Value]>;
