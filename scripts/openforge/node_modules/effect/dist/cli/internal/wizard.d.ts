import * as Effect from "../../Effect.ts";
import type * as Terminal from "../../Terminal.ts";
import type * as CliError from "../CliError.ts";
import type * as Command from "../Command.ts";
export interface Options {
    readonly commandPath?: ReadonlyArray<string> | undefined;
    readonly prefix?: ReadonlyArray<string> | undefined;
}
export interface Result {
    readonly args: Array<string>;
    readonly displayArgs: Array<string>;
}
export declare const run: (command: Command.Command.Any, options?: Options) => Effect.Effect<Result, CliError.CliError | Terminal.QuitError, Command.Environment>;
export declare const renderIntroduction: (name: string, version: string, summary: string | undefined) => string;
export declare const renderCompletion: (commandLine: ReadonlyArray<string>) => string;
export declare const renderQuit: () => string;
//# sourceMappingURL=wizard.d.ts.map