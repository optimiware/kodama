import { type JSX } from "solid-js";
import type { Context } from "./context.js";
export declare function PluginContextProvider(props: {
    readonly value: Context;
    readonly children: JSX.Element;
}): JSX.Element;
export declare function usePlugin(): Context;
