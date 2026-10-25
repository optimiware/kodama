import * as Context from "../Context.ts";
import * as Data from "../Data.ts";
import * as Effect from "../Effect.ts";
import * as FileSystem from "../FileSystem.ts";
import * as Path from "../Path.ts";
import * as Queue from "../Queue.ts";
import * as Redacted from "../Redacted.ts";
import * as Terminal from "../Terminal.ts";
import type { Covariant } from "../Types.ts";
import type * as Primitive from "./Primitive.ts";
declare const TypeId = "~effect/cli/Prompt";
/**
 * Represents an interactive terminal prompt that produces an `Output` value.
 *
 * **Details**
 *
 * A `Prompt` is an `Effect` that may fail with `Terminal.QuitError` and
 * requires the prompt environment needed to render frames, read input, and
 * access files or paths when a prompt uses them.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Prompt<Output> extends Effect.Effect<Output, Terminal.QuitError, Environment> {
    readonly [TypeId]: {
        readonly _Output: Covariant<Output>;
    };
}
/**
 * Returns `true` if the provided value is a `Prompt`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export declare const isPrompt: (u: unknown) => u is Prompt<unknown>;
/**
 * Represents the services available to a custom `Prompt`.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Environment = FileSystem.FileSystem | Path.Path | Terminal.Terminal;
/**
 * Represents the action that should be taken by a `Prompt` based upon user
 * input or an external event received during the current frame.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type Action<State, Output> = Data.TaggedEnum<{
    readonly Beep: {};
    readonly NextFrame: {
        readonly state: State;
    };
    readonly Submit: {
        readonly value: Output;
    };
}>;
/**
 * Type-level definition for the tagged `Prompt.Action` variants.
 *
 * **Details**
 *
 * It connects the action state and output type parameters to the `Beep`,
 * `NextFrame`, and `Submit` action cases.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface ActionDefinition extends Data.TaggedEnum.WithGenerics<2> {
    readonly taggedEnum: Action<this["A"], this["B"]>;
}
/**
 * Represents the input that should be processed by a `Prompt` based upon user
 * input or an external event received during the current frame.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export type ProcessInput<A> = Data.TaggedEnum<{
    readonly Input: {
        readonly input: Terminal.UserInput;
    };
    readonly Event: {
        readonly value: A;
    };
}>;
/**
 * Represents the set of handlers used by a `Prompt`.
 *
 * **Details**
 *
 * The handlers render the current frame, process user input into the next
 * `Prompt.Action`, and clear the terminal screen before the next frame.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Handlers<State, Output, Input = Terminal.UserInput> {
    /**
     * A function that is called to render the current frame of the `Prompt`.
     */
    readonly render: (state: State, action: Action<State, Output>) => Effect.Effect<string, never, Environment>;
    /**
     * A function that is called to process user input and determine the next
     * `Prompt.Action` that should be taken.
     */
    readonly process: (input: Input, state: State) => Effect.Effect<Action<State, Output>, never, Environment>;
    /**
     * A function that is called to clear the terminal screen before rendering
     * the next frame of the `Prompt`.
     */
    readonly clear: (state: State, action: Action<State, Output>) => Effect.Effect<string, never, Environment>;
}
/**
 * Defines the symbols used to render built-in prompts.
 *
 * **Details**
 *
 * Set a symbol to an empty string to omit both the symbol and its adjacent
 * spacing.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface Theme {
    /** The leading symbol on an active prompt line. */
    readonly prefix: string;
    /** The marker for the highlighted choice. */
    readonly pointer: string;
    /** The trailing prompt and validation marker. */
    readonly pointerSmall: string;
    /** The marker shown when a prompt has been submitted. */
    readonly ellipsis: string;
    /** The leading symbol on a completed prompt line. */
    readonly tick: string;
    /** The marker indicating choices above the current page. */
    readonly arrowUp: string;
    /** The marker indicating choices below the current page. */
    readonly arrowDown: string;
    /** The marker for a selected multi-select choice. */
    readonly checkboxOn: string;
    /** The marker for an unselected multi-select choice. */
    readonly checkboxOff: string;
    /** The separator between a choice and its description. */
    readonly descriptionSeparator: string;
    /** The character used to mask password input. */
    readonly passwordMask: string;
    /** The separator between toggle labels. */
    readonly toggleSeparator: string;
    /** The color used for active prompt elements. */
    readonly primaryColor: string;
    /** The color used for secondary prompt elements. */
    readonly mutedColor: string;
    /** The color used for completed prompt markers. */
    readonly successColor: string;
    /** The color used for validation errors. */
    readonly errorColor: string;
    /** The color used for submitted values. */
    readonly submittedColor: string;
}
/**
 * Options shared by built-in prompts that support theme overrides.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface ThemeOptions {
    /** Overrides the context theme for this prompt. */
    readonly theme?: Partial<Theme>;
}
/**
 * Options for a confirmation prompt that asks the user to choose a boolean
 * yes/no value.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface ConfirmOptions extends ThemeOptions {
    /**
     * The message to display in the prompt.
     */
    readonly message: string;
    /**
     * The initial value of the confirm prompt (defaults to `false`).
     */
    readonly initial?: boolean;
    /**
     * The label to display after a user has responded to the prompt.
     */
    readonly label?: {
        /**
         * The label used if the prompt is confirmed (defaults to `"yes"`).
         */
        readonly confirm: string;
        /**
         * The label used if the prompt is not confirmed (defaults to `"no"`).
         */
        readonly deny: string;
    };
    /**
     * The placeholder to display when a user is responding to the prompt.
     */
    readonly placeholder?: {
        /**
         * The placeholder to use if the `initial` value of the prompt is `true`
         * (defaults to `"(Y/n)"`).
         */
        readonly defaultConfirm?: string;
        /**
         * The placeholder to use if the `initial` value of the prompt is `false`
         * (defaults to `"(y/N)"`).
         */
        readonly defaultDeny?: string;
    };
}
/**
 * Options for a date prompt, including the displayed message, initial value,
 * format mask, validation, and locale labels.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface DateOptions extends ThemeOptions {
    /**
     * The message to display in the prompt.
     */
    readonly message: string;
    /**
     * The initial date value to display in the prompt (defaults to the current
     * date).
     */
    readonly initial?: globalThis.Date;
    /**
     * The format mask of the date (defaults to `YYYY-MM-DD HH:mm:ss`).
     */
    readonly dateMask?: string;
    /**
     * An effectful function that can be used to validate the value entered into
     * the prompt before final submission.
     */
    readonly validate?: (value: globalThis.Date) => Effect.Effect<globalThis.Date, string>;
    /**
     * Custom locales that can be used in place of the defaults.
     */
    readonly locales?: {
        /**
         * The full names of each month of the year.
         */
        readonly months: [
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string
        ];
        /**
         * The short names of each month of the year.
         */
        readonly monthsShort: [
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string,
            string
        ];
        /**
         * The full names of each day of the week.
         */
        readonly weekdays: [string, string, string, string, string, string, string];
        /**
         * The short names of each day of the week.
         */
        readonly weekdaysShort: [string, string, string, string, string, string, string];
    };
}
/**
 * Options for an integer prompt, including bounds, keyboard step sizes, and
 * additional validation.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface IntOptions extends ThemeOptions {
    /**
     * The message to display in the prompt.
     */
    readonly message: string;
    /**
     * The default value of the integer prompt.
     */
    readonly default?: number;
    /**
     * The minimum value that can be entered by the user (defaults to `-Infinity`).
     */
    readonly min?: number;
    /**
     * The maximum value that can be entered by the user (defaults to `Infinity`).
     */
    readonly max?: number;
    /**
     * The value that will be used to increment the prompt value when using the
     * up arrow key (defaults to `1`).
     */
    readonly incrementBy?: number;
    /**
     * The value that will be used to decrement the prompt value when using the
     * down arrow key (defaults to `1`).
     */
    readonly decrementBy?: number;
    /**
     * An effectful function that can be used to validate the value entered into
     * the prompt before final submission.
     */
    readonly validate?: (value: number) => Effect.Effect<number, string>;
}
/**
 * Options for `Number`, extending `IntOptions` with display precision.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface NumberOptions extends IntOptions {
    /**
     * The precision to use for the floating point value (defaults to `2`).
     */
    readonly precision?: number;
}
/**
 * Options for a text prompt that returns a list of strings by splitting the
 * input on a delimiter.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface ListOptions extends TextOptions {
    /**
     * The delimiter that separates list entries.
     */
    readonly delimiter?: string;
}
/**
 * Options for a file-system selection prompt.
 *
 * **Details**
 *
 * They control which path type can be selected, the starting directory, paging,
 * and filtering of displayed entries.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface FileOptions extends ThemeOptions {
    /**
     * The path type that will be selected, defaulting to `"file"`.
     */
    readonly type?: Primitive.PathType;
    /**
     * The message to display in the prompt, defaulting to `"Choose a file"`.
     */
    readonly message?: string;
    /**
     * Where the user will initially be prompted to select files from, defaulting
     * to the current working directory.
     */
    readonly startingPath?: string;
    /**
     * The default path to select when the prompt is first displayed.
     */
    readonly default?: string;
    /**
     * The number of choices to display at one time, defaulting to `10`.
     */
    readonly maxPerPage?: number;
    /**
     * A predicate or effect that keeps a file in the prompt when it returns
     * `true`, defaulting to returning all files.
     */
    readonly filter?: (file: string) => boolean | Effect.Effect<boolean, never, Environment>;
}
/**
 * Options for a prompt that asks the user to select one value from a list of
 * choices.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface SelectOptions<A> extends ThemeOptions {
    /**
     * The prompt message. Omit to display only the choices.
     */
    readonly message?: string | undefined;
    /**
     * The choices to display to the user.
     */
    readonly choices: ReadonlyArray<SelectChoice<A>>;
    /**
     * The number of choices to display at one time (defaults to `10`).
     */
    readonly maxPerPage?: number;
}
/**
 * Options for an autocomplete prompt that lets the user filter selectable
 * choices by typing.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface AutoCompleteOptions<A> extends SelectOptions<A> {
    /**
     * The required prompt message.
     */
    readonly message: string;
    /**
     * The label used for the filter display (defaults to "filter").
     */
    readonly filterLabel?: string;
    /**
     * The placeholder shown when the filter is empty (defaults to "type to filter").
     */
    readonly filterPlaceholder?: string;
    /**
     * The message displayed when no choices match (defaults to "No matches").
     */
    readonly emptyMessage?: string;
}
/**
 * Options for a multi-select prompt, including bulk-selection labels and
 * minimum or maximum selection counts.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface MultiSelectOptions {
    /**
     * Text for the "Select All" option (defaults to "Select All").
     */
    readonly selectAll?: string;
    /**
     * Text for the "Select None" option (defaults to "Select None").
     */
    readonly selectNone?: string;
    /**
     * Text for the "Inverse Selection" option (defaults to "Inverse Selection").
     */
    readonly inverseSelection?: string;
    /**
     * The minimum number of choices that must be selected.
     */
    readonly min?: number;
    /**
     * The maximum number of choices that can be selected.
     */
    readonly max?: number;
}
/**
 * Represents one choice displayed by select, autocomplete, and multi-select
 * prompts.
 *
 * @stability unstable
 * @category models
 * @since 4.0.0
 */
export interface SelectChoice<A> {
    /**
     * The name of the select option that is displayed to the user.
     */
    readonly title: string;
    /**
     * The underlying value of the select option.
     */
    readonly value: A;
    /**
     * An optional description for the select option which will be displayed
     * to the user.
     */
    readonly description?: string;
    /**
     * Whether or not this select option is disabled.
     */
    readonly disabled?: boolean;
    /**
     * Whether this option should be selected by default (only used by MultiSelect).
     */
    readonly selected?: boolean;
}
/**
 * Options for text-entry prompts, including the displayed message, default
 * text, and effectful validation before submission.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface TextOptions extends ThemeOptions {
    /**
     * The message to display in the prompt.
     */
    readonly message: string;
    /**
     * The default value of the text option.
     */
    readonly default?: string;
    /**
     * An effectful function that can be used to validate the value entered into
     * the prompt before final submission.
     */
    readonly validate?: (value: string) => Effect.Effect<string, string>;
}
/**
 * Options for a toggle prompt that lets the user switch between active and
 * inactive boolean states.
 *
 * @stability unstable
 * @category options
 * @since 4.0.0
 */
export interface ToggleOptions extends ThemeOptions {
    /**
     * The message to display in the prompt.
     */
    readonly message: string;
    /**
     * The intitial value of the toggle prompt (defaults to `false`).
     */
    readonly initial?: boolean;
    /**
     * The text to display when the toggle is in the active state (defaults to
     * `on`).
     */
    readonly active?: string;
    /**
     * The text to display when the toggle is in the inactive state (defaults to
     * `off`).
     */
    readonly inactive?: string;
}
/**
 * Creates a prompt theme using the current platform defaults.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const makeTheme: (options?: Partial<Theme>) => Theme;
/**
 * Context reference for the theme used by built-in prompts.
 *
 * **Details**
 *
 * Provide this reference once to theme every prompt in an application. A
 * prompt's `theme` option takes precedence over the context value.
 *
 * @stability unstable
 * @category services
 * @since 4.0.0
 */
export declare const Theme: Context.Reference<Theme>;
/**
 * Type alias for any `Prompt`, regardless of its output type.
 *
 * @stability unstable
 * @category utility types
 * @since 4.0.0
 */
export type Any = Prompt<unknown>;
/**
 * Namespace containing return-type helpers for `Prompt.all`.
 *
 * @stability unstable
 * @since 4.0.0
 */
export declare namespace All {
    /**
     * Computes the prompt returned by `Prompt.all` for an iterable of prompts.
     *
     * **Details**
     *
     * The resulting prompt produces an array of each prompt's output value.
     *
     * @stability unstable
     * @category utility types
     * @since 4.0.0
     */
    type ReturnIterable<T extends Iterable<Any>> = [T] extends [Iterable<Prompt<infer A>>] ? Prompt<Array<A>> : never;
    /**
     * Computes the prompt returned by `Prompt.all` for a readonly tuple or array
     * of prompts, preserving tuple positions in the output type.
     *
     * @stability unstable
     * @category utility types
     * @since 4.0.0
     */
    type ReturnTuple<T extends ReadonlyArray<unknown>> = Prompt<T[number] extends never ? [] : {
        -readonly [K in keyof T]: [T[K]] extends [Prompt<infer _A>] ? _A : never;
    }> extends infer X ? X : never;
    /**
     * Computes the prompt returned by `Prompt.all` for a record of prompts,
     * preserving the record keys and replacing each prompt with its output type.
     *
     * @stability unstable
     * @category utility types
     * @since 4.0.0
     */
    type ReturnObject<T> = [T] extends [{
        [K: string]: Any;
    }] ? Prompt<{
        -readonly [K in keyof T]: [T[K]] extends [Prompt<infer _A>] ? _A : never;
    }> : never;
    /**
     * Computes the return prompt type for `Prompt.all` based on the input
     * structure.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    type Return<Arg extends Iterable<Any> | Record<string, Any>> = [Arg] extends [ReadonlyArray<Any>] ? ReturnTuple<Arg> : [Arg] extends [Iterable<Any>] ? ReturnIterable<Arg> : [Arg] extends [Record<string, Any>] ? ReturnObject<Arg> : never;
}
/**
 * Runs all the provided prompts in sequence respecting the structure provided
 * in input.
 *
 * **Details**
 *
 * Supports either a tuple / iterable of prompts or a record / struct of prompts
 * as an argument.
 *
 * **Example** (Collecting prompt results)
 *
 * ```ts import.meta.vitest
 * import { Effect, FileSystem, Layer, Path, Terminal } from "effect"
 * import { Prompt } from "effect/cli"
 *
 * const terminal = Terminal.make({
 *   columns: Effect.succeed(80),
 *   rows: Effect.succeed(24),
 *   readInput: Effect.succeed({} as never),
 *   readLine: Effect.die("unused"),
 *   display: () => Effect.void
 * })
 * const services = Layer.mergeAll(
 *   FileSystem.layerNoop({}),
 *   Path.layer,
 *   Layer.succeed(Terminal.Terminal, terminal)
 * )
 *
 * const username = Prompt.succeed("alice")
 * const password = Prompt.succeed("secret")
 *
 * const allWithTuple = Prompt.all([username, password])
 *
 * const allWithRecord = Prompt.all({ username, password })
 *
 * await Effect.runPromise(Effect.provide(allWithTuple, services)) // => ["alice", "secret"]
 * await Effect.runPromise(Effect.provide(allWithRecord, services)) // => { username: "alice", password: "secret" }
 * ```
 *
 * @stability unstable
 * @category combining
 * @since 4.0.0
 */
export declare const all: <const Arg extends Iterable<Prompt<any>> | Record<string, Prompt<any>>>(arg: Arg) => All.Return<Arg>;
/**
 * Creates a confirmation prompt that asks the user to choose a boolean yes/no
 * value.
 *
 * **When to use**
 *
 * Use to ask for a yes/no answer that can be submitted directly.
 *
 * **Details**
 *
 * `initial` defaults to `false`. Enter submits the current default, yes-style
 * input submits `true`, no-style input submits `false`, and other input beeps.
 *
 * @see {@link Toggle} for an interactive switch-before-submit boolean prompt
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Confirm: (options: ConfirmOptions) => Prompt<boolean>;
/**
 * Creates a custom `Prompt` from the specified initial state and handlers.
 *
 * **Details**
 *
 * The initial state can either be a pure value or an `Effect`. This is
 * particularly useful when the initial state of the `Prompt` must be computed
 * by performing an effectful computation, such as reading data from the file
 * system. A `Prompt` runs as a render loop: `render` returns ANSI output for
 * the current frame, the `Terminal` obtains user input, `process` returns the
 * next prompt action, and `clear` returns ANSI output used to clear the previous
 * frame.
 *
 * Optionally, an external `events` dequeue can be provided as the third
 * argument. When present, the render loop will race user input against events
 * from the dequeue, allowing background events to trigger re-renders without
 * waiting for a keypress. When an event is received from the dequeue, the
 * `receive` handler is called instead of `process`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Custom: {
    /**
     * Creates a custom `Prompt` from the specified initial state and handlers.
     *
     * **Details**
     *
     * The initial state can either be a pure value or an `Effect`. This is
     * particularly useful when the initial state of the `Prompt` must be computed
     * by performing an effectful computation, such as reading data from the file
     * system. A `Prompt` runs as a render loop: `render` returns ANSI output for
     * the current frame, the `Terminal` obtains user input, `process` returns the
     * next prompt action, and `clear` returns ANSI output used to clear the previous
     * frame.
     *
     * Optionally, an external `events` dequeue can be provided as the third
     * argument. When present, the render loop will race user input against events
     * from the dequeue, allowing background events to trigger re-renders without
     * waiting for a keypress. When an event is received from the dequeue, the
     * `receive` handler is called instead of `process`.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <State, Output>(initialState: State | Effect.Effect<State, never, Environment>, handlers: Handlers<State, Output>): Prompt<Output>;
    /**
     * Creates a custom `Prompt` from the specified initial state and handlers.
     *
     * **Details**
     *
     * The initial state can either be a pure value or an `Effect`. This is
     * particularly useful when the initial state of the `Prompt` must be computed
     * by performing an effectful computation, such as reading data from the file
     * system. A `Prompt` runs as a render loop: `render` returns ANSI output for
     * the current frame, the `Terminal` obtains user input, `process` returns the
     * next prompt action, and `clear` returns ANSI output used to clear the previous
     * frame.
     *
     * Optionally, an external `events` dequeue can be provided as the third
     * argument. When present, the render loop will race user input against events
     * from the dequeue, allowing background events to trigger re-renders without
     * waiting for a keypress. When an event is received from the dequeue, the
     * `receive` handler is called instead of `process`.
     *
     * @stability unstable
     * @category constructors
     * @since 4.0.0
     */
    <State, Output, A>(initialState: State | Effect.Effect<State, never, Environment>, events: Queue.Dequeue<A, never>, handlers: Handlers<State, Output, ProcessInput<A>>): Prompt<Output>;
};
/**
 * Creates a date prompt that lets the user edit a formatted date value and
 * validates the final `Date` before submission.
 *
 * **Details**
 *
 * `initial` defaults to the current `Date`, `dateMask` defaults to
 * `YYYY-MM-DD HH:mm:ss`, mask parsing creates editable date parts plus literal
 * tokens, `locales` customizes month and weekday labels, and `validate` runs on
 * submission.
 *
 * **Gotchas**
 *
 * A supplied `initial` `Date` is edited in place during prompt interaction.
 * Date edits use JavaScript `Date` setters, so out-of-range typed values can
 * normalize before validation. If the prompt is meant to be editable,
 * `dateMask` should contain at least one editable date token.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Date: (options: DateOptions) => Prompt<globalThis.Date>;
/**
 * Creates a file-system selection prompt and returns the selected path.
 *
 * **Details**
 *
 * The prompt can be configured to select files, directories, or either path
 * type.
 *
 * You can also type to filter the listed entries. Every printable character is
 * appended to the filter query, so navigation is bound to the arrow keys,
 * `tab`, and the `Ctrl+P` / `Ctrl+N` chords used by `readline` and `fzf`
 * (`Ctrl+K` also moves up). `Ctrl+U` clears the query.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const File: (options?: FileOptions) => Prompt<string>;
/**
 * Composes prompts by using the output of this prompt to create the next prompt.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const flatMap: {
    /**
     * Composes prompts by using the output of this prompt to create the next prompt.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Output, Output2>(f: (output: Output) => Prompt<Output2>): (self: Prompt<Output>) => Prompt<Output2>;
    /**
     * Composes prompts by using the output of this prompt to create the next prompt.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Output, Output2>(self: Prompt<Output>, f: (output: Output) => Prompt<Output2>): Prompt<Output2>;
};
/**
 * Creates a floating-point number prompt.
 *
 * **Details**
 *
 * The prompt supports minimum and maximum bounds, keyboard step sizes, display
 * precision, and additional validation before submission.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Number: (options: NumberOptions) => Prompt<number>;
/**
 * Creates a text prompt that does not echo typed input and returns the
 * submitted value wrapped in `Redacted`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Hidden: (options: TextOptions) => Prompt<Redacted.Redacted>;
/**
 * Creates an integer prompt.
 *
 * **Details**
 *
 * The prompt supports minimum and maximum bounds, keyboard step sizes, and
 * additional validation before submission.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Int: (options: IntOptions) => Prompt<number>;
/**
 * Creates a text prompt that returns an array of strings by splitting the
 * submitted input on the configured delimiter.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const List: (options: ListOptions) => Prompt<Array<string>>;
/**
 * Transforms the output value produced by a prompt.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export declare const map: {
    /**
     * Transforms the output value produced by a prompt.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Output, Output2>(f: (output: Output) => Output2): (self: Prompt<Output>) => Prompt<Output2>;
    /**
     * Transforms the output value produced by a prompt.
     *
     * @stability unstable
     * @category combinators
     * @since 4.0.0
     */
    <Output, Output2>(self: Prompt<Output>, f: (output: Output) => Output2): Prompt<Output2>;
};
/**
 * Creates a password prompt that masks typed input and returns the submitted
 * value wrapped in `Redacted`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Password: (options: TextOptions) => Prompt<Redacted.Redacted>;
/**
 * Runs a prompt by reading terminal input and rendering prompt frames until the
 * prompt submits a value.
 *
 * **Gotchas**
 *
 * The returned effect may fail with `Terminal.QuitError` if terminal input ends
 * or the prompt is quit.
 *
 * @stability unstable
 * @category running
 * @since 4.0.0
 */
export declare const run: <Output>(self: Prompt<Output>) => Effect.Effect<Output, Terminal.QuitError, Environment>;
/**
 * Creates a prompt that lets the user select a single value from a list of
 * choices.
 *
 * **Gotchas**
 *
 * At most one choice may be marked as selected by default.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Select: <const A>(options: SelectOptions<A>) => Prompt<A>;
/**
 * Creates a prompt that lets users filter select choices by typing.
 *
 * **Details**
 *
 * Every printable character is appended to the filter query, so navigation is
 * bound to the arrow keys, `tab`, and the `Ctrl+P` / `Ctrl+N` chords used by
 * `readline` and `fzf` (`Ctrl+K` also moves up). `Ctrl+U` clears the query.
 *
 * **Example** (Filtering choices with autocomplete)
 *
 * ```ts import.meta.vitest
 * import { Prompt } from "effect/cli"
 *
 * const language = Prompt.AutoComplete({
 *   message: "Choose a language",
 *   choices: [
 *     { title: "TypeScript", value: "ts" },
 *     { title: "Rust", value: "rs" },
 *     { title: "Kotlin", value: "kt" }
 *   ]
 * })
 *
 * Prompt.isPrompt(language) // => true
 * ```
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const AutoComplete: <const A>(options: AutoCompleteOptions<A>) => Prompt<A>;
/**
 * Creates a prompt that lets the user select multiple choices and returns their
 * values as an array.
 *
 * **Details**
 *
 * The prompt supports default selected choices, bulk-selection commands, and
 * minimum or maximum selection counts.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const MultiSelect: <const A>(options: SelectOptions<A> & MultiSelectOptions) => Prompt<Array<A>>;
/**
 * Creates a `Prompt` which immediately succeeds with the specified value.
 *
 * **Details**
 *
 * This prompt does not attempt to obtain user input or render anything to the
 * screen.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const succeed: <A>(value: A) => Prompt<A>;
/**
 * Creates a text-entry prompt that echoes input and returns the submitted
 * string after validation.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const String: (options: TextOptions) => Prompt<string>;
/**
 * Creates a toggle prompt that lets the user switch between active and inactive
 * states and returns the selected boolean value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export declare const Toggle: (options: ToggleOptions) => Prompt<boolean>;
export {};
//# sourceMappingURL=Prompt.d.ts.map