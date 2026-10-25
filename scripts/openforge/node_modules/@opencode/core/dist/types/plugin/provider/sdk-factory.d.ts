import { Effect } from "effect";
import { Npm } from "@opencode/util/npm";
export declare const loadSDKFactory: (npm: Npm.Interface, packageName: string) => Effect.Effect<unknown, never, never>;
