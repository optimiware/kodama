import { Effect } from "effect";
import { FSUtil } from "@opencode/util/fs-util";
import { Npm } from "@opencode/util/npm";
import { AppProcess } from "@opencode/util/process";
export interface Info {
    readonly name: string;
    readonly environment?: Record<string, string>;
    readonly extensions: readonly string[];
    readonly enabled: Effect.Effect<string[] | false>;
}
export declare function make(input: {
    readonly directory: string;
    readonly worktree: string;
    readonly fs: FSUtil.Interface;
    readonly npm: Npm.Interface;
    readonly processes: AppProcess.Interface;
    readonly bin: string;
}): Info[];
