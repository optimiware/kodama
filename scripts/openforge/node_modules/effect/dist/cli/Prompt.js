/**
 * Builds interactive terminal prompts for CLI applications.
 *
 * A `Prompt<A>` describes a small terminal UI that renders frames, reads
 * keyboard input, validates responses, and eventually produces an `A`. Prompts
 * can ask for simple values, selections, lists, files, hidden text, or custom
 * interactions. This module includes prompt constructors, tools for combining
 * and transforming prompt output, and support for running prompts through the
 * `Terminal` service.
 *
 * @stability unstable
 * @since 4.0.0
 */
import * as Arr from "../Array.js";
import * as Context from "../Context.js";
import * as Data from "../Data.js";
import * as Effect from "../Effect.js";
import * as Effectable from "../Effectable.js";
import * as FileSystem from "../FileSystem.js";
import { dual, pipe } from "../Function.js";
import * as EffectNumber from "../Number.js";
import * as Option from "../Option.js";
import * as Path from "../Path.js";
import * as Predicate from "../Predicate.js";
import * as Queue from "../Queue.js";
import * as Redacted from "../Redacted.js";
import * as Terminal from "../Terminal.js";
import * as Ansi from "./internal/ansi.js";
const TypeId = "~effect/cli/Prompt";
/**
 * Returns `true` if the provided value is a `Prompt`.
 *
 * @stability unstable
 * @category guards
 * @since 4.0.0
 */
export const isPrompt = u => Predicate.hasProperty(u, TypeId);
const defaultTheme = {
  prefix: "?",
  arrowUp: "↑",
  arrowDown: "↓",
  checkboxOn: "☒",
  checkboxOff: "☐",
  tick: "✔",
  ellipsis: "…",
  pointerSmall: "›",
  pointer: "❯",
  descriptionSeparator: "- ",
  passwordMask: "*",
  toggleSeparator: "/",
  primaryColor: Ansi.cyanBright,
  mutedColor: Ansi.blackBright,
  successColor: Ansi.green,
  errorColor: Ansi.red,
  submittedColor: Ansi.white
};
const windowsTheme = {
  ...defaultTheme,
  checkboxOn: "[*]",
  checkboxOff: "[ ]",
  tick: "√",
  ellipsis: "...",
  pointerSmall: "»",
  pointer: ">"
};
/**
 * Creates a prompt theme using the current platform defaults.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const makeTheme = options => ({
  ...(process.platform === "win32" ? windowsTheme : defaultTheme),
  ...options
});
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
export const Theme = /*#__PURE__*/Context.Reference("effect/cli/Prompt/Theme", {
  defaultValue: makeTheme
});
const getTheme = options => Effect.map(Theme, theme => ({
  ...theme,
  ...options.theme
}));
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
export const all = function () {
  if (arguments.length === 1) {
    if (isPrompt(arguments[0])) {
      return map(arguments[0], x => [x]);
    } else if (Predicate.isIterable(arguments[0])) {
      return allTupled(Arr.fromIterable(arguments[0]));
    } else {
      const entries = Object.entries(arguments[0]);
      if (entries.length === 0) {
        return succeed({});
      }
      let result = map(entries[0][1], value => ({
        [entries[0][0]]: value
      }));
      if (entries.length === 1) {
        return result;
      }
      const rest = entries.slice(1);
      for (const [key, prompt] of rest) {
        result = result.pipe(flatMap(record => prompt.pipe(map(value => ({
          ...record,
          [key]: value
        })))));
      }
      return result;
    }
  }
  return allTupled(arguments[0]);
};
const annotateLine = line => Ansi.annotate(line, Ansi.bold);
const annotateErrorLine = (line, color) => Ansi.annotate(line, Ansi.combine(Ansi.italicized, color));
const annotateSymbol = (symbol, ...styles) => symbol.length === 0 ? "" : Ansi.annotate(symbol, ...styles);
const separateSymbol = (symbol, text) => symbol.length === 0 ? text : symbol + " " + text;
const renderPagingPrefix = (theme, showArrowUp, showArrowDown) => {
  const width = Math.max(theme.arrowUp.length, theme.arrowDown.length);
  if (showArrowUp) {
    return theme.arrowUp.padEnd(width);
  }
  if (showArrowDown) {
    return theme.arrowDown.padEnd(width);
  }
  return " ".repeat(width);
};
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
export const Confirm = options => {
  const opts = {
    initial: false,
    ...options,
    label: {
      confirm: "yes",
      deny: "no",
      ...options.label
    },
    placeholder: {
      defaultConfirm: "(Y/n)",
      defaultDeny: "(y/N)",
      ...options.placeholder
    }
  };
  const initialState = {
    value: opts.initial
  };
  return Custom(initialState, {
    render: handleConfirmRender(opts),
    process: input => handleConfirmProcess(input, opts.initial),
    clear: handleConfirmClear(opts)
  });
};
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
export const Custom = (initialState, ...args) => {
  const [events, handlers] = args.length === 1 ? [undefined, args[0]] : [args[0], args[1]];
  const op = Object.create(proto);
  op._tag = "Loop";
  op.initialState = initialState;
  op.render = handlers.render;
  op.process = handlers.process;
  op.clear = handlers.clear;
  op.events = events;
  return op;
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
export const Date = options => {
  const opts = {
    initial: new globalThis.Date(),
    dateMask: "YYYY-MM-DD HH:mm:ss",
    validate: Effect.succeed,
    ...options,
    locales: {
      ...defaultLocales,
      ...options.locales
    }
  };
  const dateParts = makeDateParts(opts.dateMask, opts.initial, opts.locales);
  const initialCursorPosition = dateParts.findIndex(part => !part.isToken());
  const initialState = {
    dateParts,
    typed: "",
    cursor: initialCursorPosition,
    value: opts.initial,
    error: Option.none()
  };
  return Custom(initialState, {
    render: handleDateRender(opts),
    process: handleDateProcess(opts),
    clear: handleDateClear(opts)
  });
};
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
export const File = (options = {}) => {
  const opts = {
    ...options,
    type: options.type ?? "file",
    message: options.message ?? `Choose a file`,
    startingPath: Option.fromUndefinedOr(options.startingPath),
    default: Option.fromUndefinedOr(options.default),
    maxPerPage: options.maxPerPage ?? 10,
    filter: options.filter ?? (() => Effect.succeed(true))
  };
  const initialState = Effect.gen(function* () {
    const currentPath = yield* resolveCurrentPath(Option.none(), opts);
    const path = yield* Path.Path;
    const defaultPath = Option.map(opts.default, defaultValue => path.resolve(currentPath, defaultValue));
    const initialPath = Option.match(defaultPath, {
      onNone: () => currentPath,
      onSome: defaultPath => path.dirname(defaultPath)
    });
    const files = yield* getFileList(initialPath, opts);
    const cursor = Option.match(defaultPath, {
      onNone: () => 0,
      onSome: defaultPath => {
        const index = files.indexOf(path.basename(defaultPath));
        return index === -1 ? 0 : index;
      }
    });
    const confirm = FileConfirm.Hide();
    return {
      cursor,
      files,
      allFiles: files,
      query: "",
      path: Option.map(defaultPath, path.dirname),
      confirm
    };
  });
  return Custom(initialState, {
    render: handleFileRender(opts),
    process: handleFileProcess(opts),
    clear: handleFileClear(opts)
  });
};
/**
 * Composes prompts by using the output of this prompt to create the next prompt.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const flatMap = /*#__PURE__*/dual(2, (self, f) => {
  const op = Object.create(proto);
  op._tag = "OnSuccess";
  op.prompt = self;
  op.onSuccess = f;
  return op;
});
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
export const Number = options => {
  const opts = {
    default: 0,
    min: globalThis.Number.NEGATIVE_INFINITY,
    max: globalThis.Number.POSITIVE_INFINITY,
    incrementBy: 1,
    decrementBy: 1,
    precision: 2,
    validate: n => {
      if (n < opts.min) {
        return Effect.fail(`${n} must be greater than or equal to ${opts.min}`);
      }
      if (n > opts.max) {
        return Effect.fail(`${n} must be less than or equal to ${opts.max}`);
      }
      return Effect.succeed(n);
    },
    ...options
  };
  const initialValue = options.default === undefined ? "" : `${opts.default}`;
  const initialState = {
    cursor: initialValue.length,
    value: initialValue,
    error: Option.none()
  };
  return Custom(initialState, {
    render: handleRenderFloat(opts),
    process: handleProcessFloat(opts),
    clear: handleNumberClear(opts)
  });
};
/**
 * Creates a text prompt that does not echo typed input and returns the
 * submitted value wrapped in `Redacted`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Hidden = options => basePrompt(options, "hidden").pipe(map(Redacted.make));
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
export const Int = options => {
  const opts = {
    default: 0,
    min: globalThis.Number.NEGATIVE_INFINITY,
    max: globalThis.Number.POSITIVE_INFINITY,
    incrementBy: 1,
    decrementBy: 1,
    validate: n => {
      if (n < opts.min) {
        return Effect.fail(`${n} must be greater than or equal to ${opts.min}`);
      }
      if (n > opts.max) {
        return Effect.fail(`${n} must be less than or equal to ${opts.max}`);
      }
      return Effect.succeed(n);
    },
    ...options
  };
  const initialValue = options.default === undefined ? "" : `${opts.default}`;
  const initialState = {
    cursor: initialValue.length,
    value: initialValue,
    error: Option.none()
  };
  return Custom(initialState, {
    render: handleRenderInteger(opts),
    process: handleProcessInteger(opts),
    clear: handleNumberClear(opts)
  });
};
/**
 * Creates a text prompt that returns an array of strings by splitting the
 * submitted input on the configured delimiter.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const List = options => String(options).pipe(map(output => output.split(options.delimiter || ",")));
/**
 * Transforms the output value produced by a prompt.
 *
 * @stability unstable
 * @category combinators
 * @since 4.0.0
 */
export const map = /*#__PURE__*/dual(2, (self, f) => flatMap(self, a => succeed(f(a))));
/**
 * Creates a password prompt that masks typed input and returns the submitted
 * value wrapped in `Redacted`.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Password = options => basePrompt(options, "password").pipe(map(Redacted.make));
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
export const run = /*#__PURE__*/Effect.fnUntraced(function* (self) {
  const terminal = yield* Terminal.Terminal;
  const input = yield* terminal.readInput;
  return yield* runWithInput(self, terminal, input);
}, /*#__PURE__*/Effect.mapError(() => new Terminal.QuitError({})), Effect.scoped);
const getSelectInitialIndex = choices => {
  let initialIndex = 0;
  let seenSelected = -1;
  for (let i = 0; i < choices.length; i++) {
    const choice = choices[i];
    if (choice.selected === true) {
      if (seenSelected !== -1) {
        throw new Error("InvalidArgumentException: only a single choice can be selected by default for Prompt.Select");
      }
      seenSelected = i;
    }
  }
  if (seenSelected !== -1) {
    initialIndex = seenSelected;
  }
  return initialIndex;
};
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
export const Select = options => {
  const opts = {
    maxPerPage: 10,
    ...options
  };
  const initialIndex = getSelectInitialIndex(opts.choices);
  return Custom(initialIndex, {
    render: handleSelectRender(opts),
    process: handleSelectProcess(opts),
    clear: handleSelectClear(opts)
  });
};
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
export const AutoComplete = options => {
  const opts = {
    maxPerPage: 10,
    filterLabel: "filter",
    filterPlaceholder: "type to filter",
    emptyMessage: "No matches",
    ...options
  };
  const initialIndex = getSelectInitialIndex(opts.choices);
  const filtered = filterAutoCompleteChoices(opts.choices, "");
  const index = filtered.length === 0 ? 0 : filtered.includes(initialIndex) ? initialIndex : filtered[0];
  const initialState = {
    query: "",
    index,
    filtered
  };
  return Custom(initialState, {
    render: handleAutoCompleteRender(opts),
    process: handleAutoCompleteProcess(opts),
    clear: handleAutoCompleteClear(opts)
  });
};
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
export const MultiSelect = options => {
  const opts = {
    maxPerPage: 10,
    ...options
  };
  // Seed initial selection from choices marked as selected: true
  const initialSelected = new Set();
  for (let i = 0; i < opts.choices.length; i++) {
    const choice = opts.choices[i];
    if (choice.selected === true && !choice.disabled) {
      initialSelected.add(i);
    }
  }
  const initialState = {
    index: 0,
    selectedIndices: initialSelected,
    error: Option.none()
  };
  return Custom(initialState, {
    render: handleMultiSelectRender(opts),
    process: handleMultiSelectProcess(opts),
    clear: handleMultiSelectClear(opts)
  });
};
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
export const succeed = value => {
  const op = Object.create(proto);
  op._tag = "Succeed";
  op.value = value;
  return op;
};
/**
 * Creates a text-entry prompt that echoes input and returns the submitted
 * string after validation.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const String = options => basePrompt(options, "text");
/**
 * Creates a toggle prompt that lets the user switch between active and inactive
 * states and returns the selected boolean value.
 *
 * @stability unstable
 * @category constructors
 * @since 4.0.0
 */
export const Toggle = options => {
  const opts = {
    initial: false,
    active: "on",
    inactive: "off",
    ...options
  };
  return Custom(opts.initial, {
    render: handleToggleRender(opts),
    process: handleToggleProcess,
    clear: () => handleToggleClear(opts)
  });
};
const proto = {
  ... /*#__PURE__*/Effectable.Prototype({
    label: "Prompt",
    evaluate() {
      return run(this);
    }
  }),
  [TypeId]: {
    _Output: _ => _
  }
};
const allTupled = arg => {
  if (arg.length === 0) {
    return succeed([]);
  }
  if (arg.length === 1) {
    return map(arg[0], x => [x]);
  }
  let result = map(arg[0], x => [x]);
  for (let i = 1; i < arg.length; i++) {
    const curr = arg[i];
    result = flatMap(result, tuple => map(curr, a => [...tuple, a]));
  }
  return result;
};
const runWithInput = (prompt, terminal, input) => Effect.suspend(() => {
  const op = prompt;
  switch (op._tag) {
    case "Loop":
      {
        return runLoop(op, terminal, input);
      }
    case "OnSuccess":
      {
        return Effect.flatMap(runWithInput(op.prompt, terminal, input), a => runWithInput(op.onSuccess(a), terminal, input));
      }
    case "Succeed":
      {
        return Effect.succeed(op.value);
      }
  }
});
const runLoop = /*#__PURE__*/Effect.fnUntraced(function* (loop, terminal, input) {
  let state = Effect.isEffect(loop.initialState) ? yield* loop.initialState : loop.initialState;
  let action = Action.NextFrame({
    state
  });
  let clear = "";
  while (true) {
    const msg = yield* loop.render(state, action);
    yield* Effect.orDie(terminal.display(clear + msg));
    clear = "";
    if (loop.events) {
      const takeInput = Queue.take(input).pipe(Effect.map(input => ({
        _tag: "Input",
        input
      })));
      const result = yield* Effect.raceFirst(takeInput, Queue.take(loop.events).pipe(Effect.map(value => ({
        _tag: "Event",
        value
      }))));
      action = yield* loop.process(result, state);
    } else {
      const result = yield* Queue.take(input);
      action = yield* loop.process(result, state);
    }
    switch (action._tag) {
      case "Beep":
        continue;
      case "NextFrame":
        {
          clear = yield* loop.clear(state, action);
          state = action.state;
          continue;
        }
      case "Submit":
        {
          clear = yield* loop.clear(state, action);
          const msg = yield* loop.render(state, action);
          yield* Effect.orDie(terminal.display(clear + msg));
          return action.value;
        }
    }
  }
}, (effect, _, terminal) => Effect.ensuring(effect, Effect.orDie(terminal.display(Ansi.cursorShow))));
const Action = /*#__PURE__*/Data.taggedEnum();
/**
 * Clears all lines taken up by the specified `text`.
 */
const eraseText = (text, columns) => {
  if (columns === 0) {
    return Ansi.eraseLine + Ansi.cursorTo(0);
  }
  let rows = 0;
  const lines = text.split(NEWLINE_REGEXP);
  for (const line of lines) {
    rows += 1 + Math.floor(Math.max(line.length - 1, 0) / columns);
  }
  return Ansi.eraseLines(rows);
};
const lines = (prompt, columns) => {
  const lines = prompt.split(NEWLINE_REGEXP);
  return columns === 0 ? lines.length : pipe(Arr.map(lines, line => Math.ceil(line.length / columns)), Arr.reduce(0, (left, right) => left + right));
};
const clearOutputWithError = (outputText, columns, errorText) => {
  if (errorText !== undefined && errorText.length > 0) {
    return Ansi.cursorDown(lines(errorText, columns)) + eraseText(`\n${errorText}`, columns) + eraseText(outputText, columns);
  }
  return eraseText(outputText, columns);
};
const renderBeep = Ansi.beep;
const NEWLINE_REGEXP = /\r?\n/;
const handleConfirmClear = options => {
  return Effect.fnUntraced(function* (state, _) {
    const terminal = yield* Terminal.Terminal;
    const columns = yield* terminal.columns;
    const figures = yield* getTheme(options);
    const confirmMessage = state.value ? options.placeholder.defaultConfirm : options.placeholder.defaultDeny;
    const promptText = renderConfirmOutput(confirmMessage, figures.prefix, figures.pointerSmall, options, {
      plain: true
    });
    const clearOutput = eraseText(promptText, columns);
    const resetCurrentLine = Ansi.eraseLine + Ansi.cursorLeft;
    return clearOutput + resetCurrentLine;
  });
};
const renderConfirmOutput = (confirm, leadingSymbol, trailingSymbol, options, renderOptions) => renderPrompt(confirm, options.message, leadingSymbol, trailingSymbol, renderOptions);
const renderConfirmNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  // Marking these explicitly as present with `!` because they always will be
  // and there is really no value in adding a `DeepRequired` type helper just
  // for these internal cases
  const confirmMessage = state.value ? options.placeholder.defaultConfirm : options.placeholder.defaultDeny;
  const confirm = Ansi.annotate(confirmMessage, figures.mutedColor);
  const promptMsg = renderConfirmOutput(confirm, leadingSymbol, trailingSymbol, options);
  return Ansi.cursorHide + promptMsg;
});
const renderConfirmSubmission = /*#__PURE__*/Effect.fnUntraced(function* (value, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const confirmMessage = value ? options.label.confirm : options.label.deny;
  const promptMsg = renderConfirmOutput(confirmMessage, leadingSymbol, trailingSymbol, options);
  return promptMsg + "\n";
});
const handleConfirmRender = options => {
  return (_, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderConfirmNextFrame(state, options),
      Submit: ({
        value
      }) => renderConfirmSubmission(value, options)
    });
  };
};
const TRUE_VALUE_REGEXP = /^y|t$/;
const FALSE_VALUE_REGEXP = /^n|f$/;
const handleConfirmProcess = (input, defaultValue) => {
  const value = Option.getOrElse(input.input, () => "");
  if (input.key.name === "enter" || input.key.name === "return") {
    return Effect.succeed(Action.Submit({
      value: defaultValue
    }));
  }
  if (TRUE_VALUE_REGEXP.test(value.toLowerCase())) {
    return Effect.succeed(Action.Submit({
      value: true
    }));
  }
  if (FALSE_VALUE_REGEXP.test(value.toLowerCase())) {
    return Effect.succeed(Action.Submit({
      value: false
    }));
  }
  return Effect.succeed(Action.Beep());
};
const handleDateClear = options => {
  return Effect.fnUntraced(function* (state, _) {
    const terminal = yield* Terminal.Terminal;
    const columns = yield* terminal.columns;
    const figures = yield* getTheme(options);
    const resetCurrentLine = Ansi.eraseLine + Ansi.cursorLeft;
    const parts = Arr.reduce(state.dateParts, "", (doc, part) => doc + part.toString());
    const promptText = renderDateOutput(figures.prefix, figures.pointerSmall, parts, options, {
      plain: true
    });
    const errorText = Option.isSome(state.error) ? Arr.match(state.error.value.split(NEWLINE_REGEXP), {
      onEmpty: () => "",
      onNonEmpty: errorLines => separateSymbol(figures.pointerSmall, errorLines.join("\n"))
    }) : "";
    const clearOutput = clearOutputWithError(promptText, columns, errorText);
    return clearOutput + resetCurrentLine;
  });
};
const renderDateError = (state, pointer, theme) => {
  if (Option.isSome(state.error)) {
    const errorLines = state.error.value.split(NEWLINE_REGEXP);
    if (Arr.isReadonlyArrayNonEmpty(errorLines)) {
      const prefix = annotateSymbol(pointer, theme.errorColor);
      const lines = Arr.map(errorLines, str => annotateErrorLine(str, theme.errorColor));
      return Ansi.cursorSavePosition + "\n" + separateSymbol(prefix, lines.join("\n")) + Ansi.cursorRestorePosition;
    }
  }
  return "";
};
const renderParts = (state, theme, submitted = false) => {
  return Arr.reduce(state.dateParts, "", (doc, part, currentIndex) => {
    const partDoc = part.toString();
    if (currentIndex === state.cursor && !submitted) {
      const annotation = Ansi.combine(Ansi.underlined, theme.primaryColor);
      return doc + Ansi.annotate(partDoc, annotation);
    }
    return doc + partDoc;
  });
};
const renderDateOutput = (leadingSymbol, trailingSymbol, parts, options, renderOptions) => renderPrompt(parts, options.message, leadingSymbol, trailingSymbol, renderOptions);
const renderDateNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const parts = renderParts(state, figures);
  const promptMsg = renderDateOutput(leadingSymbol, trailingSymbol, parts, options);
  const errorMsg = renderDateError(state, figures.pointerSmall, figures);
  return Ansi.cursorHide + promptMsg + errorMsg;
});
const renderDateSubmission = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const parts = renderParts(state, figures, true);
  const promptMsg = renderDateOutput(leadingSymbol, trailingSymbol, parts, options);
  return promptMsg + "\n";
});
const processUp = state => {
  state.dateParts[state.cursor].increment();
  return Action.NextFrame({
    state: {
      ...state,
      typed: ""
    }
  });
};
const processDown = state => {
  state.dateParts[state.cursor].decrement();
  return Action.NextFrame({
    state: {
      ...state,
      typed: ""
    }
  });
};
const processDateCursorLeft = state => {
  const previous = state.dateParts[state.cursor].previousPart();
  if (Option.isSome(previous)) {
    return Action.NextFrame({
      state: {
        ...state,
        typed: "",
        cursor: state.dateParts.indexOf(previous.value)
      }
    });
  }
  return Action.Beep();
};
const processDateCursorRight = state => {
  const next = state.dateParts[state.cursor].nextPart();
  if (Option.isSome(next)) {
    return Action.NextFrame({
      state: {
        ...state,
        typed: "",
        cursor: state.dateParts.indexOf(next.value)
      }
    });
  }
  return Action.Beep();
};
const processDateNext = state => {
  const next = state.dateParts[state.cursor].nextPart();
  const cursor = Option.match(next, {
    onNone: () => state.dateParts.findIndex(part => !part.isToken()),
    onSome: next => state.dateParts.indexOf(next)
  });
  return Action.NextFrame({
    state: {
      ...state,
      typed: "",
      cursor
    }
  });
};
const defaultDateProcessor = (value, state) => {
  if (/\d/.test(value)) {
    const typed = state.typed + value;
    state.dateParts[state.cursor].setValue(typed);
    return Action.NextFrame({
      state: {
        ...state,
        typed
      }
    });
  }
  return Action.Beep();
};
const defaultLocales = {
  months: ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"],
  monthsShort: ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"],
  weekdays: ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"],
  weekdaysShort: ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]
};
const handleDateRender = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderDateNextFrame(state, options),
      Submit: () => renderDateSubmission(state, options)
    });
  };
};
const handleDateProcess = options => {
  return (input, state) => {
    switch (input.key.name) {
      case "left":
        {
          return Effect.succeed(processDateCursorLeft(state));
        }
      case "right":
        {
          return Effect.succeed(processDateCursorRight(state));
        }
      case "k":
      case "up":
        {
          return Effect.succeed(processUp(state));
        }
      case "j":
      case "down":
        {
          return Effect.succeed(processDown(state));
        }
      case "tab":
        {
          return Effect.succeed(processDateNext(state));
        }
      case "enter":
      case "return":
        {
          return Effect.match(options.validate(state.value), {
            onFailure: error => Action.NextFrame({
              state: {
                ...state,
                error: Option.some(error)
              }
            }),
            onSuccess: value => Action.Submit({
              value
            })
          });
        }
      default:
        {
          return Effect.succeed(defaultDateProcessor(Option.getOrElse(input.input, () => ""), state));
        }
    }
  };
};
const DATE_PART_REGEXP = /\\(.)|"((?:\\["\\]|[^"])+)"|(D[Do]?|d{3,4}|d)|(M{1,4})|(YY(?:YY)?)|([aA])|([Hh]{1,2})|(m{1,2})|(s{1,2})|(S{1,4})|./g;
const regExpGroups = {
  1: ({
    token,
    ...opts
  }) => new Token({
    token: token.replace(/\\(.)/g, "$1"),
    ...opts
  }),
  2: opts => new Day(opts),
  3: opts => new Month(opts),
  4: opts => new Year(opts),
  5: opts => new Meridiem(opts),
  6: opts => new Hours(opts),
  7: opts => new Minutes(opts),
  8: opts => new Seconds(opts),
  9: opts => new Milliseconds(opts)
};
const makeDateParts = (dateMask, date, locales) => {
  const parts = [];
  let result = null;
  // oxlint-disable-next-line no-cond-assign
  while (result = DATE_PART_REGEXP.exec(dateMask)) {
    const match = result.shift();
    const index = result.findIndex(group => group !== undefined);
    if (index in regExpGroups) {
      const token = result[index] || match;
      parts.push(regExpGroups[index]({
        token,
        date,
        parts,
        locales
      }));
    } else {
      parts.push(new Token({
        token: result[index] || match,
        date,
        parts,
        locales
      }));
    }
  }
  const orderedParts = parts.reduce((array, element) => {
    const lastElement = array[array.length - 1];
    if (element.isToken() && lastElement !== undefined && lastElement.isToken()) {
      lastElement.setValue(element.token);
    } else {
      array.push(element);
    }
    return array;
  }, Arr.empty());
  parts.splice(0, parts.length, ...orderedParts);
  return parts;
};
class DatePart {
  token;
  date;
  parts;
  locales;
  constructor(params) {
    this.token = params.token;
    this.locales = params.locales;
    this.date = params.date || new globalThis.Date();
    this.parts = params.parts || [this];
  }
  /**
   * Returns `true` if this `DatePart` is a `Token`, `false` otherwise.
   */
  isToken() {
    return false;
  }
  /**
   * Retrieves the next date part in the list of parts.
   */
  nextPart() {
    const currentPartIndex = Option.getOrElse(Arr.findFirstIndex(this.parts, part => part === this), () => 0);
    return Arr.findFirst(this.parts.slice(currentPartIndex + 1), part => !part.isToken());
  }
  /**
   * Retrieves the previous date part in the list of parts.
   */
  previousPart() {
    const currentPartIndex = Arr.findFirstIndex(this.parts, part => part === this);
    if (Option.isSome(currentPartIndex)) {
      return Arr.findLast(this.parts.slice(0, currentPartIndex.value), part => !part.isToken());
    }
    return Option.none();
  }
  toString() {
    return globalThis.String(this.date);
  }
}
class Token extends DatePart {
  increment() {}
  decrement() {}
  setValue(value) {
    this.token = this.token + value;
  }
  isToken() {
    return true;
  }
  toString() {
    return this.token;
  }
}
class Milliseconds extends DatePart {
  increment() {
    this.date.setMilliseconds(this.date.getMilliseconds() + 1);
  }
  decrement() {
    this.date.setMilliseconds(this.date.getMilliseconds() - 1);
  }
  setValue(value) {
    this.date.setMilliseconds(globalThis.Number.parseInt(value.slice(-this.token.length)));
  }
  toString() {
    const millis = `${this.date.getMilliseconds()}`;
    return millis.padStart(4, "0").substring(0, this.token.length);
  }
}
class Seconds extends DatePart {
  increment() {
    this.date.setSeconds(this.date.getSeconds() + 1);
  }
  decrement() {
    this.date.setSeconds(this.date.getSeconds() - 1);
  }
  setValue(value) {
    this.date.setSeconds(globalThis.Number.parseInt(value.slice(-2)));
  }
  toString() {
    const seconds = `${this.date.getSeconds()}`;
    return this.token.length > 1 ? seconds.padStart(2, "0") : seconds;
  }
}
class Minutes extends DatePart {
  increment() {
    this.date.setMinutes(this.date.getMinutes() + 1);
  }
  decrement() {
    this.date.setMinutes(this.date.getMinutes() - 1);
  }
  setValue(value) {
    this.date.setMinutes(globalThis.Number.parseInt(value.slice(-2)));
  }
  toString() {
    const minutes = `${this.date.getMinutes()}`;
    return this.token.length > 1 ? minutes.padStart(2, "0") : minutes;
  }
}
class Hours extends DatePart {
  increment() {
    this.date.setHours(this.date.getHours() + 1);
  }
  decrement() {
    this.date.setHours(this.date.getHours() - 1);
  }
  setValue(value) {
    this.date.setHours(globalThis.Number.parseInt(value.slice(-2)));
  }
  toString() {
    const hours = /h/.test(this.token) ? this.date.getHours() % 12 || 12 : this.date.getHours();
    return this.token.length > 1 ? `${hours}`.padStart(2, "0") : `${hours}`;
  }
}
class Day extends DatePart {
  increment() {
    this.date.setDate(this.date.getDate() + 1);
  }
  decrement() {
    this.date.setDate(this.date.getDate() - 1);
  }
  setValue(value) {
    this.date.setDate(globalThis.Number.parseInt(value.slice(-2)));
  }
  toString() {
    const date = this.date.getDate();
    const day = this.date.getDay();
    switch (this.token) {
      case "DD":
        return `${date}`.padStart(2, "0");
      case "Do":
        return `${date}${this.ordinalIndicator(date)}`;
      case "d":
        return `${day + 1}`;
      case "ddd":
        return this.locales.weekdaysShort[day];
      case "dddd":
        return this.locales.weekdays[day];
      default:
        return `${date}`;
    }
  }
  ordinalIndicator(day) {
    if (day >= 11 && day <= 13) {
      return "th";
    }
    switch (day % 10) {
      case 1:
        return "st";
      case 2:
        return "nd";
      case 3:
        return "rd";
      default:
        return "th";
    }
  }
}
class Month extends DatePart {
  increment() {
    this.date.setMonth(this.date.getMonth() + 1);
  }
  decrement() {
    this.date.setMonth(this.date.getMonth() - 1);
  }
  setValue(value) {
    const month = globalThis.Number.parseInt(value.slice(-2)) - 1;
    this.date.setMonth(month < 0 ? 0 : month);
  }
  toString() {
    const month = this.date.getMonth();
    switch (this.token.length) {
      case 2:
        return `${month + 1}`.padStart(2, "0");
      case 3:
        return this.locales.monthsShort[month];
      case 4:
        return this.locales.months[month];
      default:
        return `${month + 1}`;
    }
  }
}
class Year extends DatePart {
  increment() {
    this.date.setFullYear(this.date.getFullYear() + 1);
  }
  decrement() {
    this.date.setFullYear(this.date.getFullYear() - 1);
  }
  setValue(value) {
    this.date.setFullYear(globalThis.Number.parseInt(value.slice(-4)));
  }
  toString() {
    const year = `${this.date.getFullYear()}`.padStart(4, "0");
    return this.token.length === 2 ? year.slice(-2) : year;
  }
}
class Meridiem extends DatePart {
  increment() {
    this.date.setHours((this.date.getHours() + 12) % 24);
  }
  decrement() {
    this.increment();
  }
  setValue(_value) {}
  toString() {
    const meridiem = this.date.getHours() >= 12 ? "pm" : "am";
    return /A/.test(this.token) ? meridiem.toUpperCase() : meridiem;
  }
}
const CONFIRM_MESSAGE = "The selected directory contains files. Would you like to traverse the selected directory?";
const FILE_FILTER_LABEL = "filter";
const FILE_FILTER_PLACEHOLDER = "type to filter";
const FILE_EMPTY_MESSAGE = "No matches";
const FileConfirm = /*#__PURE__*/Data.taggedEnum();
const showConfirmation = /*#__PURE__*/FileConfirm.$is("Show");
const resolveCurrentPath = (path, options) => {
  if (Option.isSome(path)) {
    return Effect.succeed(path.value);
  }
  if (Option.isSome(options.startingPath)) {
    const startingPath = options.startingPath.value;
    return Effect.flatMap(FileSystem.FileSystem, fs =>
    // Ensure the user provided starting path exists
    Effect.orDie(fs.exists(startingPath)).pipe(Effect.flatMap(exists => exists ? Effect.void : Effect.die(`The provided starting path '${startingPath}' does not exist`)), Effect.as(startingPath)));
  }
  return Effect.sync(() => process.cwd());
};
const getFileList = /*#__PURE__*/Effect.fnUntraced(function* (directory, options) {
  const fs = yield* FileSystem.FileSystem;
  const path = yield* Path.Path;
  const files = yield* Effect.orDie(fs.readDirectory(directory)).pipe(
  // Always prepend the `".."` option to the file list but allow it
  // to be filtered out if the user so desires
  Effect.map(files => ["..", ...files]));
  return yield* Effect.filter(files, file => {
    const result = options.filter(file);
    const userDefinedFilter = Effect.isEffect(result) ? result : Effect.succeed(result);
    const directoryFilter = options.type === "directory" ? Effect.map(Effect.orDie(fs.stat(path.join(directory, file))), info => info.type === "Directory") : Effect.succeed(true);
    return Effect.zipWith(userDefinedFilter, directoryFilter, (a, b) => a && b);
  }, {
    concurrency: files.length
  });
});
const filterFiles = (files, query) => {
  if (query.length === 0) {
    return files;
  }
  const normalizedQuery = query.toLowerCase();
  const filtered = [];
  for (let index = 0; index < files.length; index++) {
    if (files[index].toLowerCase().includes(normalizedQuery)) {
      filtered.push(files[index]);
    }
  }
  return filtered;
};
const updateFileState = (state, query, allFiles = state.allFiles) => {
  const files = filterFiles(allFiles, query);
  if (files.length === 0) {
    return {
      ...state,
      query,
      allFiles,
      files,
      cursor: 0
    };
  }
  const selected = state.files[state.cursor];
  const cursor = selected === undefined ? 0 : files.indexOf(selected);
  return {
    ...state,
    query,
    allFiles,
    files,
    cursor: cursor === -1 ? 0 : cursor
  };
};
const handleFileClear = options => {
  return Effect.fnUntraced(function* (state, _) {
    const terminal = yield* Terminal.Terminal;
    const columns = yield* terminal.columns;
    const path = yield* Path.Path;
    const figures = yield* getTheme(options);
    const currentPath = yield* resolveCurrentPath(state.path, options);
    const selectedPath = state.files[state.cursor];
    const resolvedPath = selectedPath === undefined ? currentPath : path.resolve(currentPath, selectedPath);
    const resolvedPathText = separateSymbol(figures.pointerSmall, resolvedPath);
    const isConfirming = showConfirmation(state.confirm);
    const promptText = isConfirming ? renderPrompt("(Y/n)", CONFIRM_MESSAGE, figures.prefix, figures.pointerSmall, {
      plain: true
    }) : renderPrompt(renderFileFilter(state, figures, {
      plain: true
    }), options.message, figures.tick, figures.ellipsis, {
      plain: true
    });
    const filesText = isConfirming ? "" : renderFiles(state, state.files, figures, options, {
      plain: true
    });
    const outputText = isConfirming ? `${promptText}\n${resolvedPathText}` : `${promptText}\n${resolvedPathText}\n${filesText}`;
    const clearOutput = eraseText(outputText, columns);
    const resetCurrentLine = Ansi.eraseLine + Ansi.cursorLeft;
    return clearOutput + resetCurrentLine;
  });
};
const renderPrompt = (confirm, message, leadingSymbol, trailingSymbol, options) => {
  const prefix = leadingSymbol.length === 0 ? "" : leadingSymbol + " ";
  const renderLine = line => {
    let output = prefix + line;
    if (trailingSymbol.length > 0) {
      output += " " + trailingSymbol;
    }
    if (confirm.length > 0) {
      output += " " + confirm;
    } else if (trailingSymbol.length > 0) {
      output += " ";
    }
    return output;
  };
  const annotate = options?.plain === true ? line => line : annotateLine;
  return Arr.match(message.split(NEWLINE_REGEXP), {
    onEmpty: () => renderLine(""),
    onNonEmpty: promptLines => {
      const lines = Arr.map(promptLines, line => annotate(line));
      return renderLine(lines.join("\n"));
    }
  });
};
const renderPrefix = (state, toDisplay, currentIndex, length, figures, renderOptions) => {
  const prefix = renderPagingPrefix(figures, currentIndex === toDisplay.startIndex && toDisplay.startIndex > 0, currentIndex === toDisplay.endIndex - 1 && toDisplay.endIndex < length);
  if (state.cursor === currentIndex) {
    return renderOptions?.plain === true ? figures.pointer + prefix : annotateSymbol(figures.pointer, figures.primaryColor) + prefix;
  }
  return prefix + " ".repeat(figures.pointer.length);
};
const renderFileName = (file, isSelected, theme, renderOptions) => {
  if (renderOptions?.plain === true) {
    return file;
  }
  return isSelected ? Ansi.annotate(file, Ansi.combine(Ansi.underlined, theme.primaryColor)) : file;
};
const renderFileFilter = (state, theme, renderOptions) => {
  const filterValue = state.query.length === 0 ? renderOptions?.plain === true ? FILE_FILTER_PLACEHOLDER : Ansi.annotate(FILE_FILTER_PLACEHOLDER, theme.mutedColor) : renderOptions?.plain === true ? state.query : Ansi.annotate(state.query, Ansi.combine(Ansi.underlined, theme.primaryColor));
  return `[${FILE_FILTER_LABEL}: ${filterValue}]`;
};
const renderFiles = (state, files, figures, options, renderOptions) => {
  const length = files.length;
  if (length === 0) {
    return renderOptions?.plain === true ? FILE_EMPTY_MESSAGE : Ansi.annotate(FILE_EMPTY_MESSAGE, figures.mutedColor);
  }
  const toDisplay = entriesToDisplay(state.cursor, length, options.maxPerPage);
  const documents = [];
  for (let index = toDisplay.startIndex; index < toDisplay.endIndex; index++) {
    const isSelected = state.cursor === index;
    const prefix = renderPrefix(state, toDisplay, index, length, figures, renderOptions);
    const fileName = renderFileName(files[index], isSelected, figures, renderOptions);
    documents.push(prefix + fileName);
  }
  return documents.join("\n");
};
const renderFileNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const path = yield* Path.Path;
  const figures = yield* getTheme(options);
  const currentPath = yield* resolveCurrentPath(state.path, options);
  const selectedPath = state.files[state.cursor];
  const resolvedPath = selectedPath === undefined ? currentPath : path.resolve(currentPath, selectedPath);
  const resolvedPathMsg = Ansi.annotate(separateSymbol(figures.pointerSmall, resolvedPath), figures.mutedColor);
  if (showConfirmation(state.confirm)) {
    const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
    const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
    const confirm = Ansi.annotate("(Y/n)", figures.mutedColor);
    const promptMsg = renderPrompt(confirm, CONFIRM_MESSAGE, leadingSymbol, trailingSymbol);
    return Ansi.cursorHide + promptMsg + "\n" + resolvedPathMsg;
  }
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderPrompt(renderFileFilter(state, figures), options.message, leadingSymbol, trailingSymbol);
  const files = renderFiles(state, state.files, figures, options);
  return Ansi.cursorHide + promptMsg + "\n" + resolvedPathMsg + "\n" + files;
});
const renderFileSubmission = /*#__PURE__*/Effect.fnUntraced(function* (state, value, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderPrompt(renderFileFilter(state, figures), options.message, leadingSymbol, trailingSymbol);
  return promptMsg + " " + Ansi.annotate(value, figures.submittedColor) + "\n";
});
const handleFileRender = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderFileNextFrame(state, options),
      Submit: ({
        value
      }) => renderFileSubmission(state, value, options)
    });
  };
};
const processFileCursorUp = state => {
  if (state.files.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const cursor = state.cursor - 1;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      cursor: cursor < 0 ? state.files.length - 1 : cursor
    }
  }));
};
const processFileCursorDown = state => {
  if (state.files.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      cursor: (state.cursor + 1) % state.files.length
    }
  }));
};
const processFileBackspace = state => {
  if (state.query.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const query = state.query.slice(0, state.query.length - 1);
  return Effect.succeed(Action.NextFrame({
    state: updateFileState(state, query)
  }));
};
const processFileClear = state => Effect.succeed(Action.NextFrame({
  state: updateFileState(state, "")
}));
const processFileInput = (input, state) => {
  if (input.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const query = state.query + input;
  return Effect.succeed(Action.NextFrame({
    state: updateFileState(state, query)
  }));
};
const processSelection = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  if (state.files.length === 0) {
    return Action.Beep();
  }
  const fs = yield* FileSystem.FileSystem;
  const path = yield* Path.Path;
  const currentPath = yield* resolveCurrentPath(state.path, options);
  const selectedPath = state.files[state.cursor];
  const resolvedPath = path.resolve(currentPath, selectedPath);
  const info = yield* Effect.orDie(fs.stat(resolvedPath));
  if (info.type === "Directory") {
    const files = yield* getFileList(resolvedPath, options);
    const filesWithoutParent = files.filter(file => file !== "..");
    // If the user selected a directory AND the prompt type can result with
    // a directory, we must confirm:
    //  - If the selected directory has any files
    //  - Confirm whether or not the user wants to traverse those files
    if (options.type === "directory" || options.type === "either") {
      return filesWithoutParent.length === 0
      // Directory is empty so it's safe to select it
      ? Action.Submit({
        value: resolvedPath
      })
      // Directory has contents - show confirmation to user
      : Action.NextFrame({
        state: {
          ...state,
          confirm: FileConfirm.Show()
        }
      });
    }
    return Action.NextFrame({
      state: {
        cursor: 0,
        files,
        allFiles: files,
        query: "",
        path: Option.some(resolvedPath),
        confirm: FileConfirm.Hide()
      }
    });
  }
  return Action.Submit({
    value: resolvedPath
  });
});
const handleFileProcess = options => {
  return Effect.fnUntraced(function* (input, state) {
    // Navigation is bound to ctrl chords rather than bare `j`/`k`, so that every
    // printable character reaches the filter input.
    if (input.key.ctrl) {
      switch (input.key.name) {
        case "u":
          {
            if (showConfirmation(state.confirm)) {
              return Action.Beep();
            }
            return yield* processFileClear(state);
          }
        case "p":
        case "k":
          {
            return yield* processFileCursorUp(state);
          }
        case "n":
          {
            return yield* processFileCursorDown(state);
          }
        default:
          {
            return Action.Beep();
          }
      }
    }
    switch (input.key.name) {
      case "up":
        {
          return yield* processFileCursorUp(state);
        }
      case "down":
      case "tab":
        {
          return yield* processFileCursorDown(state);
        }
      case "backspace":
        {
          if (showConfirmation(state.confirm)) {
            return Action.Beep();
          }
          return yield* processFileBackspace(state);
        }
      case "enter":
      case "return":
        {
          return yield* processSelection(state, options);
        }
      case "y":
      case "t":
        {
          if (showConfirmation(state.confirm)) {
            const path = yield* Path.Path;
            const currentPath = yield* resolveCurrentPath(state.path, options);
            const selectedPath = state.files[state.cursor];
            const resolvedPath = path.resolve(currentPath, selectedPath);
            const files = yield* getFileList(resolvedPath, options);
            return Action.NextFrame({
              state: {
                cursor: 0,
                files,
                allFiles: files,
                query: "",
                path: Option.some(resolvedPath),
                confirm: FileConfirm.Hide()
              }
            });
          }
          return yield* processFileInput(Option.getOrElse(input.input, () => ""), state);
        }
      case "n":
      case "f":
        {
          if (showConfirmation(state.confirm)) {
            const path = yield* Path.Path;
            const currentPath = yield* resolveCurrentPath(state.path, options);
            const selectedPath = state.files[state.cursor];
            const resolvedPath = path.resolve(currentPath, selectedPath);
            return Action.Submit({
              value: resolvedPath
            });
          }
          return yield* processFileInput(Option.getOrElse(input.input, () => ""), state);
        }
      default:
        {
          if (showConfirmation(state.confirm)) {
            return Action.Beep();
          }
          return yield* processFileInput(Option.getOrElse(input.input, () => ""), state);
        }
    }
  });
};
const renderMultiSelectError = (state, pointer, theme, renderOptions) => {
  if (Option.isSome(state.error)) {
    return Arr.match(state.error.value.split(NEWLINE_REGEXP), {
      onEmpty: () => "",
      onNonEmpty: errorLines => {
        if (renderOptions?.plain === true) {
          return separateSymbol(pointer, errorLines.join("\n"));
        }
        const prefix = annotateSymbol(pointer, theme.errorColor);
        const lines = Arr.map(errorLines, str => annotateErrorLine(str, theme.errorColor));
        return Ansi.cursorSavePosition + "\n" + separateSymbol(prefix, lines.join("\n")) + Ansi.cursorRestorePosition;
      }
    });
  }
  return "";
};
const renderChoiceDescription = (choice, isActive, theme, renderOptions) => {
  if (!choice.disabled && choice.description && isActive) {
    const text = theme.descriptionSeparator + choice.description;
    return renderOptions?.plain === true ? text : Ansi.annotate(text, theme.mutedColor);
  }
  return "";
};
const metaOptionsCount = 2;
const renderMultiSelectTitle = (title, isHighlighted, theme, renderOptions) => {
  if (renderOptions?.plain === true || !isHighlighted) {
    return title;
  }
  return Ansi.annotate(title, Ansi.combine(Ansi.underlined, theme.primaryColor));
};
const renderMultiSelectChoices = (state, options, figures, renderOptions) => {
  const choices = options.choices;
  const checkboxWidth = Math.max(figures.checkboxOn.length, figures.checkboxOff.length);
  const selectableCount = choices.filter(choice => !choice.disabled).length;
  const selectedCount = Array.from(state.selectedIndices).filter(index => !choices[index].disabled).length;
  const allSelected = selectedCount === selectableCount;
  const selectAllText = allSelected ? options?.selectNone ?? "Select None" : options?.selectAll ?? "Select All";
  const inverseSelectionText = options?.inverseSelection ?? "Inverse Selection";
  const metaOptions = [{
    title: selectAllText
  }, {
    title: inverseSelectionText
  }];
  const allChoices = [...metaOptions, ...choices];
  const toDisplay = entriesToDisplay(state.index, allChoices.length, options.maxPerPage);
  const documents = [];
  for (let index = toDisplay.startIndex; index < toDisplay.endIndex; index++) {
    const choice = allChoices[index];
    const isHighlighted = state.index === index;
    const prefix = renderPagingPrefix(figures, index === toDisplay.startIndex && toDisplay.startIndex > 0, index === toDisplay.endIndex - 1 && toDisplay.endIndex < allChoices.length);
    if (index < metaOptions.length) {
      // Meta options
      const title = renderMultiSelectTitle(choice.title, isHighlighted, figures, renderOptions);
      documents.push(prefix + (prefix.length === 0 ? "" : " ") + title);
    } else {
      // Regular choices
      const choiceIndex = index - metaOptions.length;
      const isSelected = state.selectedIndices.has(choiceIndex);
      const checkbox = isSelected ? figures.checkboxOn : figures.checkboxOff;
      const annotatedCheckbox = isHighlighted && renderOptions?.plain !== true ? Ansi.annotate(checkbox, figures.primaryColor) : checkbox;
      const selectChoice = choice;
      const title = renderChoiceTitle(selectChoice, isHighlighted, figures, renderOptions);
      const description = renderChoiceDescription(selectChoice, isHighlighted, figures, renderOptions);
      const checkboxPrefix = checkboxWidth === 0 ? "" : annotatedCheckbox + " ".repeat(checkboxWidth - checkbox.length + 1);
      documents.push(prefix + (prefix.length === 0 ? "" : " ") + checkboxPrefix + title + " " + description);
    }
  }
  return documents.join("\n");
};
const renderMultiSelectNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const choices = renderMultiSelectChoices(state, options, figures);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const promptMsg = renderSelectOutput(leadingSymbol, trailingSymbol, options);
  const error = renderMultiSelectError(state, figures.pointer, figures);
  return Ansi.cursorHide + withSelectHeader(promptMsg, choices) + error;
});
const renderMultiSelectSubmission = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const selectedChoices = Array.from(state.selectedIndices).sort(EffectNumber.Order).map(index => options.choices[index].title);
  const selectedText = selectedChoices.join(", ");
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderSelectOutput(leadingSymbol, trailingSymbol, options) ?? leadingSymbol;
  return promptMsg + " " + Ansi.annotate(selectedText, figures.submittedColor) + "\n";
});
const processMultiSelectCursorUp = (state, totalChoices) => {
  const newIndex = state.index === 0 ? totalChoices - 1 : state.index - 1;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      index: newIndex
    }
  }));
};
const processMultiSelectCursorDown = (state, totalChoices) => {
  const newIndex = (state.index + 1) % totalChoices;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      index: newIndex
    }
  }));
};
const processSpace = (state, options) => {
  const selectedIndices = new Set(state.selectedIndices);
  if (state.index === 0) {
    const selectableCount = options.choices.filter(choice => !choice.disabled).length;
    const selectedCount = Array.from(state.selectedIndices).filter(index => !options.choices[index].disabled).length;
    if (selectedCount === selectableCount) {
      selectedIndices.clear();
    } else {
      for (let i = 0; i < options.choices.length; i++) {
        if (!options.choices[i].disabled) {
          selectedIndices.add(i);
        }
      }
    }
  } else if (state.index === 1) {
    for (let i = 0; i < options.choices.length; i++) {
      if (options.choices[i].disabled || state.selectedIndices.has(i)) {
        selectedIndices.delete(i);
      } else {
        selectedIndices.add(i);
      }
    }
  } else {
    const choiceIndex = state.index - metaOptionsCount;
    if (options.choices[choiceIndex].disabled) {
      return Effect.succeed(Action.Beep());
    } else if (selectedIndices.has(choiceIndex)) {
      selectedIndices.delete(choiceIndex);
    } else {
      selectedIndices.add(choiceIndex);
    }
  }
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      selectedIndices
    }
  }));
};
const handleMultiSelectClear = options => Effect.fnUntraced(function* (state, _) {
  const terminal = yield* Terminal.Terminal;
  const columns = yield* terminal.columns;
  const figures = yield* getTheme(options);
  const clearPrompt = Ansi.eraseLine + Ansi.cursorLeft;
  const promptText = renderSelectOutput(figures.prefix, figures.pointerSmall, options, {
    plain: true
  });
  const choicesText = renderMultiSelectChoices(state, options, figures, {
    plain: true
  });
  const errorText = renderMultiSelectError(state, figures.pointer, figures, {
    plain: true
  });
  const clearOutput = clearOutputWithError(withSelectHeader(promptText, choicesText), columns, errorText);
  return clearOutput + clearPrompt;
});
const handleMultiSelectProcess = options => {
  return (input, state) => {
    const totalChoices = options.choices.length + metaOptionsCount;
    switch (input.key.name) {
      case "k":
      case "up":
        {
          return processMultiSelectCursorUp({
            ...state,
            error: Option.none()
          }, totalChoices);
        }
      case "j":
      case "down":
      case "tab":
        {
          return processMultiSelectCursorDown({
            ...state,
            error: Option.none()
          }, totalChoices);
        }
      case "space":
        {
          return processSpace(state, options);
        }
      case "enter":
      case "return":
        {
          const selectedIndices = Array.from(state.selectedIndices).filter(index => !options.choices[index].disabled);
          const selectedCount = selectedIndices.length;
          if (options.min !== undefined && selectedCount < options.min) {
            return Effect.succeed(Action.NextFrame({
              state: {
                ...state,
                error: Option.some(`At least ${options.min} are required`)
              }
            }));
          }
          if (options.max !== undefined && selectedCount > options.max) {
            return Effect.succeed(Action.NextFrame({
              state: {
                ...state,
                error: Option.some(`At most ${options.max} choices are allowed`)
              }
            }));
          }
          const selectedValues = selectedIndices.sort(EffectNumber.Order).map(index => options.choices[index].value);
          return Effect.succeed(Action.Submit({
            value: selectedValues
          }));
        }
      default:
        {
          return Effect.succeed(Action.Beep());
        }
    }
  };
};
const handleMultiSelectRender = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderMultiSelectNextFrame(state, options),
      Submit: () => renderMultiSelectSubmission(state, options)
    });
  };
};
const handleNumberClear = options => {
  return Effect.fnUntraced(function* (state, _) {
    const terminal = yield* Terminal.Terminal;
    const columns = yield* terminal.columns;
    const figures = yield* getTheme(options);
    const resetCurrentLine = Ansi.eraseLine + Ansi.cursorLeft;
    const errorText = renderNumberError(state, figures.pointerSmall, figures, {
      plain: true
    });
    const promptText = renderNumberOutput(state, figures.prefix, figures.pointerSmall, options, figures, {
      plain: true
    });
    const clearOutput = clearOutputWithError(promptText, columns, errorText);
    return clearOutput + resetCurrentLine;
  });
};
const renderNumberInput = (state, submitted, theme, renderOptions) => {
  const value = state.value === "" ? "" : `${state.value}`;
  if (submitted || renderOptions?.plain === true) {
    return value;
  }
  const annotation = Option.isSome(state.error) ? theme.errorColor : Ansi.combine(Ansi.underlined, theme.primaryColor);
  return Ansi.annotate(value, annotation);
};
const renderNumberError = (state, pointer, theme, renderOptions) => {
  if (Option.isSome(state.error)) {
    return Arr.match(state.error.value.split(NEWLINE_REGEXP), {
      onEmpty: () => "",
      onNonEmpty: errorLines => {
        if (renderOptions?.plain === true) {
          return separateSymbol(pointer, errorLines.join("\n"));
        }
        const prefix = annotateSymbol(pointer, theme.errorColor);
        const lines = Arr.map(errorLines, str => annotateErrorLine(str, theme.errorColor));
        return Ansi.cursorSavePosition + "\n" + separateSymbol(prefix, lines.join("\n")) + Ansi.cursorRestorePosition;
      }
    });
  }
  return "";
};
const renderNumberOutput = (state, leadingSymbol, trailingSymbol, options, theme, renderOptions, submitted = false) => {
  const value = renderNumberInput(state, submitted, theme, renderOptions);
  return renderPrompt(value, options.message, leadingSymbol, trailingSymbol, renderOptions);
};
const renderNumberNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const errorMsg = renderNumberError(state, figures.pointerSmall, figures);
  const promptMsg = renderNumberOutput(state, leadingSymbol, trailingSymbol, options, figures);
  return promptMsg + errorMsg;
});
const renderNumberSubmission = /*#__PURE__*/Effect.fnUntraced(function* (nextState, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderNumberOutput(nextState, leadingSymbol, trailingSymbol, options, figures, undefined, true);
  return promptMsg + "\n";
});
const processNumberBackspace = state => {
  if (state.value.length <= 0) {
    return Effect.succeed(Action.Beep());
  }
  const value = state.value.slice(0, state.value.length - 1);
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      value,
      error: Option.none()
    }
  }));
};
const processNumberClear = state => Effect.succeed(Action.NextFrame({
  state: {
    ...state,
    cursor: 0,
    value: "",
    error: Option.none()
  }
}));
const defaultIntProcessor = (input, state) => {
  if (state.value.length === 0 && input === "-") {
    return Effect.succeed(Action.NextFrame({
      state: {
        ...state,
        value: "-",
        error: Option.none()
      }
    }));
  }
  const parsed = globalThis.Number.parseInt(state.value + input);
  if (globalThis.Number.isNaN(parsed)) {
    return Effect.succeed(Action.Beep());
  } else {
    return Effect.succeed(Action.NextFrame({
      state: {
        ...state,
        value: `${parsed}`,
        error: Option.none()
      }
    }));
  }
};
const defaultFloatProcessor = (input, state) => {
  if (input === "." && state.value.includes(".")) {
    return Effect.succeed(Action.Beep());
  }
  if (state.value.length === 0 && input === "-") {
    return Effect.succeed(Action.NextFrame({
      state: {
        ...state,
        value: "-",
        error: Option.none()
      }
    }));
  }
  const parsed = globalThis.Number.parseFloat(state.value + input);
  if (globalThis.Number.isNaN(parsed)) {
    return Effect.succeed(Action.Beep());
  } else {
    return Effect.succeed(Action.NextFrame({
      state: {
        ...state,
        value: input === "." ? `${parsed}.` : state.value.includes(".") && /^\d$/.test(input) ? state.value + input : `${parsed}`,
        error: Option.none()
      }
    }));
  }
};
const handleRenderInteger = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderNumberNextFrame(state, options),
      Submit: () => renderNumberSubmission(state, options)
    });
  };
};
const handleProcessInteger = options => {
  return (input, state) => {
    if (input.key.ctrl && input.key.name === "u") {
      return processNumberClear(state);
    }
    switch (input.key.name) {
      case "backspace":
        {
          return processNumberBackspace(state);
        }
      case "k":
      case "up":
        {
          return Effect.succeed(Action.NextFrame({
            state: {
              ...state,
              value: state.value === "" || state.value === "-" ? `${options.incrementBy}` : `${globalThis.Number.parseInt(state.value) + options.incrementBy}`,
              error: Option.none()
            }
          }));
        }
      case "j":
      case "down":
        {
          return Effect.succeed(Action.NextFrame({
            state: {
              ...state,
              value: state.value === "" || state.value === "-" ? `-${options.decrementBy}` : `${globalThis.Number.parseInt(state.value) - options.decrementBy}`,
              error: Option.none()
            }
          }));
        }
      case "enter":
      case "return":
        {
          const parsed = globalThis.Number.parseInt(state.value);
          if (globalThis.Number.isNaN(parsed)) {
            return Effect.succeed(Action.NextFrame({
              state: {
                ...state,
                error: Option.some("Must provide an integer value")
              }
            }));
          } else {
            return Effect.match(options.validate(parsed), {
              onFailure: error => Action.NextFrame({
                state: {
                  ...state,
                  error: Option.some(error)
                }
              }),
              onSuccess: value => Action.Submit({
                value
              })
            });
          }
        }
      default:
        {
          return defaultIntProcessor(Option.getOrElse(input.input, () => ""), state);
        }
    }
  };
};
const handleRenderFloat = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderNumberNextFrame(state, options),
      Submit: () => renderNumberSubmission(state, options)
    });
  };
};
const handleProcessFloat = options => {
  return (input, state) => {
    if (input.key.ctrl && input.key.name === "u") {
      return processNumberClear(state);
    }
    switch (input.key.name) {
      case "backspace":
        {
          return processNumberBackspace(state);
        }
      case "k":
      case "up":
        {
          return Effect.succeed(Action.NextFrame({
            state: {
              ...state,
              value: state.value === "" || state.value === "-" ? `${options.incrementBy}` : `${globalThis.Number.parseFloat(state.value) + options.incrementBy}`,
              error: Option.none()
            }
          }));
        }
      case "j":
      case "down":
        {
          return Effect.succeed(Action.NextFrame({
            state: {
              ...state,
              value: state.value === "" || state.value === "-" ? `-${options.decrementBy}` : `${globalThis.Number.parseFloat(state.value) - options.decrementBy}`,
              error: Option.none()
            }
          }));
        }
      case "enter":
      case "return":
        {
          const parsed = globalThis.Number.parseFloat(state.value);
          if (globalThis.Number.isNaN(parsed)) {
            return Effect.succeed(Action.NextFrame({
              state: {
                ...state,
                error: Option.some("Must provide a floating point value")
              }
            }));
          } else {
            return Effect.flatMap(Effect.sync(() => EffectNumber.round(parsed, options.precision)), rounded => Effect.match(options.validate(rounded), {
              onFailure: error => Action.NextFrame({
                state: {
                  ...state,
                  error: Option.some(error)
                }
              }),
              onSuccess: value => Action.Submit({
                value
              })
            }));
          }
        }
      default:
        {
          return defaultFloatProcessor(Option.getOrElse(input.input, () => ""), state);
        }
    }
  };
};
const filterAutoCompleteChoices = (choices, query) => {
  const normalizedQuery = query.toLowerCase();
  const indices = [];
  for (let i = 0; i < choices.length; i++) {
    if (choices[i].title.toLowerCase().includes(normalizedQuery)) {
      indices.push(i);
    }
  }
  return indices;
};
const updateAutoCompleteState = (state, options, query) => {
  const filtered = filterAutoCompleteChoices(options.choices, query);
  if (filtered.length === 0) {
    return {
      ...state,
      query,
      filtered,
      index: 0
    };
  }
  if (filtered.includes(state.index)) {
    return {
      ...state,
      query,
      filtered
    };
  }
  return {
    ...state,
    query,
    filtered,
    index: filtered[0]
  };
};
const autoCompleteCursor = state => Option.getOrElse(Arr.findFirstIndex(state.filtered, index => index === state.index), () => 0);
const renderSelectOutput = (leadingSymbol, trailingSymbol, options, renderOptions) => options.message === undefined ? undefined : renderPrompt("", options.message, leadingSymbol, trailingSymbol, renderOptions);
const withSelectHeader = (header, body) => header === undefined ? body : header + "\n" + body;
const renderAutoCompleteFilter = (state, options, theme, renderOptions) => {
  const filterValue = state.query.length === 0 ? renderOptions?.plain === true ? options.filterPlaceholder : Ansi.annotate(options.filterPlaceholder, theme.mutedColor) : renderOptions?.plain === true ? state.query : Ansi.annotate(state.query, Ansi.combine(Ansi.underlined, theme.primaryColor));
  return `[${options.filterLabel}: ${filterValue}]`;
};
const renderAutoCompleteOutput = (state, leadingSymbol, trailingSymbol, options, theme, renderOptions) => {
  const filter = renderAutoCompleteFilter(state, options, theme, renderOptions);
  return renderPrompt(filter, options.message, leadingSymbol, trailingSymbol, renderOptions);
};
const renderChoicePrefix = (state, choices, toDisplay, currentIndex, figures, renderOptions) => {
  const prefix = renderPagingPrefix(figures, currentIndex === toDisplay.startIndex && toDisplay.startIndex > 0, currentIndex === toDisplay.endIndex - 1 && toDisplay.endIndex < choices.length);
  if (renderOptions?.plain === true) {
    return state === currentIndex ? figures.pointer + prefix : prefix + " ".repeat(figures.pointer.length);
  }
  if (choices[currentIndex].disabled) {
    const annotation = Ansi.combine(Ansi.bold, figures.mutedColor);
    return state === currentIndex ? annotateSymbol(figures.pointer, annotation) + prefix : prefix + " ".repeat(figures.pointer.length);
  }
  return state === currentIndex ? annotateSymbol(figures.pointer, figures.primaryColor) + prefix : prefix + " ".repeat(figures.pointer.length);
};
const renderAutoCompleteChoicePrefix = (state, options, toDisplay, currentIndex, figures, renderOptions) => {
  const prefix = renderPagingPrefix(figures, currentIndex === toDisplay.startIndex && toDisplay.startIndex > 0, currentIndex === toDisplay.endIndex - 1 && toDisplay.endIndex < state.filtered.length);
  const choiceIndex = state.filtered[currentIndex];
  if (renderOptions?.plain === true) {
    return state.index === choiceIndex ? figures.pointer + prefix : prefix + " ".repeat(figures.pointer.length);
  }
  const choice = options.choices[choiceIndex];
  if (choice.disabled) {
    const annotation = Ansi.combine(Ansi.bold, figures.mutedColor);
    return state.index === choiceIndex ? annotateSymbol(figures.pointer, annotation) + prefix : prefix + " ".repeat(figures.pointer.length);
  }
  return state.index === choiceIndex ? annotateSymbol(figures.pointer, figures.primaryColor) + prefix : prefix + " ".repeat(figures.pointer.length);
};
const renderChoiceTitle = (choice, isSelected, theme, renderOptions) => {
  if (renderOptions?.plain === true) {
    return choice.title;
  }
  const title = choice.title;
  if (isSelected) {
    return choice.disabled ? Ansi.annotate(title, Ansi.combine(Ansi.underlined, theme.mutedColor)) : Ansi.annotate(title, Ansi.combine(Ansi.underlined, theme.primaryColor));
  }
  return choice.disabled ? Ansi.annotate(title, Ansi.combine(Ansi.strikethrough, theme.mutedColor)) : title;
};
const renderSelectChoices = (state, options, figures, renderOptions) => {
  const choices = options.choices;
  const toDisplay = entriesToDisplay(state, choices.length, options.maxPerPage);
  const documents = [];
  for (let index = toDisplay.startIndex; index < toDisplay.endIndex; index++) {
    const choice = choices[index];
    const isSelected = state === index;
    const prefix = renderChoicePrefix(state, choices, toDisplay, index, figures, renderOptions);
    const title = renderChoiceTitle(choice, isSelected, figures, renderOptions);
    const description = renderChoiceDescription(choice, isSelected, figures, renderOptions);
    documents.push(prefix + title + " " + description);
  }
  return documents.join("\n");
};
const renderAutoCompleteChoices = (state, options, figures, renderOptions) => {
  if (state.filtered.length === 0) {
    return renderOptions?.plain === true ? options.emptyMessage : Ansi.annotate(options.emptyMessage, figures.mutedColor);
  }
  const cursor = autoCompleteCursor(state);
  const toDisplay = entriesToDisplay(cursor, state.filtered.length, options.maxPerPage);
  const documents = [];
  for (let index = toDisplay.startIndex; index < toDisplay.endIndex; index++) {
    const choiceIndex = state.filtered[index];
    const choice = options.choices[choiceIndex];
    const isSelected = state.index === choiceIndex;
    const prefix = renderAutoCompleteChoicePrefix(state, options, toDisplay, index, figures, renderOptions);
    const title = renderChoiceTitle(choice, isSelected, figures, renderOptions);
    const description = renderChoiceDescription(choice, isSelected, figures, renderOptions);
    documents.push(prefix + title + " " + description);
  }
  return documents.join("\n");
};
const renderSelectNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const choices = renderSelectChoices(state, options, figures);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const promptMsg = renderSelectOutput(leadingSymbol, trailingSymbol, options);
  return Ansi.cursorHide + withSelectHeader(promptMsg, choices);
});
const renderAutoCompleteNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const choices = renderAutoCompleteChoices(state, options, figures);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const promptMsg = renderAutoCompleteOutput(state, leadingSymbol, trailingSymbol, options, figures);
  return Ansi.cursorHide + promptMsg + "\n" + choices;
});
const renderSelectSubmission = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const selected = options.choices[state].title;
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderSelectOutput(leadingSymbol, trailingSymbol, options) ?? leadingSymbol;
  return promptMsg + " " + Ansi.annotate(selected, figures.submittedColor) + "\n";
});
const renderAutoCompleteSubmission = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const selected = options.choices[state.index].title;
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderAutoCompleteOutput(state, leadingSymbol, trailingSymbol, options, figures);
  return promptMsg + " " + Ansi.annotate(selected, figures.submittedColor) + "\n";
});
const processSelectCursorUp = (state, choices) => {
  if (state === 0) {
    return Effect.succeed(Action.NextFrame({
      state: choices.length - 1
    }));
  }
  return Effect.succeed(Action.NextFrame({
    state: state - 1
  }));
};
const processSelectCursorDown = (state, choices) => {
  if (state === choices.length - 1) {
    return Effect.succeed(Action.NextFrame({
      state: 0
    }));
  }
  return Effect.succeed(Action.NextFrame({
    state: state + 1
  }));
};
const processSelectNext = (state, choices) => {
  return Effect.succeed(Action.NextFrame({
    state: (state + 1) % choices.length
  }));
};
const processAutoCompleteCursorUp = state => {
  if (state.filtered.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const cursor = autoCompleteCursor(state);
  const nextCursor = cursor === 0 ? state.filtered.length - 1 : cursor - 1;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      index: state.filtered[nextCursor]
    }
  }));
};
const processAutoCompleteCursorDown = state => {
  if (state.filtered.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const cursor = autoCompleteCursor(state);
  const nextCursor = (cursor + 1) % state.filtered.length;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      index: state.filtered[nextCursor]
    }
  }));
};
const processAutoCompleteNext = state => processAutoCompleteCursorDown(state);
const processAutoCompleteBackspace = (state, options) => {
  if (state.query.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const query = state.query.slice(0, state.query.length - 1);
  return Effect.succeed(Action.NextFrame({
    state: updateAutoCompleteState(state, options, query)
  }));
};
const processAutoCompleteClear = (state, options) => Effect.succeed(Action.NextFrame({
  state: updateAutoCompleteState(state, options, "")
}));
const processAutoCompleteInput = (input, state, options) => {
  if (input.length === 0) {
    return Effect.succeed(Action.Beep());
  }
  const query = state.query + input;
  return Effect.succeed(Action.NextFrame({
    state: updateAutoCompleteState(state, options, query)
  }));
};
const handleSelectRender = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderSelectNextFrame(state, options),
      Submit: () => renderSelectSubmission(state, options)
    });
  };
};
const handleAutoCompleteRender = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderAutoCompleteNextFrame(state, options),
      Submit: () => renderAutoCompleteSubmission(state, options)
    });
  };
};
const handleSelectClear = options => Effect.fnUntraced(function* (state, _) {
  const terminal = yield* Terminal.Terminal;
  const columns = yield* terminal.columns;
  const figures = yield* getTheme(options);
  const clearPrompt = Ansi.eraseLine + Ansi.cursorLeft;
  const promptText = renderSelectOutput(figures.prefix, figures.pointerSmall, options, {
    plain: true
  });
  const choicesText = renderSelectChoices(state, options, figures, {
    plain: true
  });
  const clearOutput = eraseText(withSelectHeader(promptText, choicesText), columns);
  return clearOutput + clearPrompt;
});
const handleAutoCompleteClear = options => Effect.fnUntraced(function* (state, _) {
  const terminal = yield* Terminal.Terminal;
  const columns = yield* terminal.columns;
  const figures = yield* getTheme(options);
  const clearPrompt = Ansi.eraseLine + Ansi.cursorLeft;
  const promptText = renderAutoCompleteOutput(state, figures.prefix, figures.pointerSmall, options, figures, {
    plain: true
  });
  const choicesText = renderAutoCompleteChoices(state, options, figures, {
    plain: true
  });
  const clearOutput = eraseText(`${promptText}\n${choicesText}`, columns);
  return clearOutput + clearPrompt;
});
const handleSelectProcess = options => {
  return (input, state) => {
    switch (input.key.name) {
      case "k":
      case "up":
        {
          return processSelectCursorUp(state, options.choices);
        }
      case "j":
      case "down":
        {
          return processSelectCursorDown(state, options.choices);
        }
      case "tab":
        {
          return processSelectNext(state, options.choices);
        }
      case "enter":
      case "return":
        {
          const selected = options.choices[state];
          if (selected.disabled) {
            return Effect.succeed(Action.Beep());
          }
          return Effect.succeed(Action.Submit({
            value: selected.value
          }));
        }
      default:
        {
          return Effect.succeed(Action.Beep());
        }
    }
  };
};
const handleAutoCompleteProcess = options => {
  return (input, state) => {
    // Navigation is bound to ctrl chords rather than bare `j`/`k`, so that every
    // printable character reaches the filter input.
    if (input.key.ctrl) {
      switch (input.key.name) {
        case "u":
          {
            return processAutoCompleteClear(state, options);
          }
        case "p":
        case "k":
          {
            return processAutoCompleteCursorUp(state);
          }
        case "n":
          {
            return processAutoCompleteCursorDown(state);
          }
        default:
          {
            return Effect.succeed(Action.Beep());
          }
      }
    }
    switch (input.key.name) {
      case "up":
        {
          return processAutoCompleteCursorUp(state);
        }
      case "down":
        {
          return processAutoCompleteCursorDown(state);
        }
      case "tab":
        {
          return processAutoCompleteNext(state);
        }
      case "backspace":
        {
          return processAutoCompleteBackspace(state, options);
        }
      case "enter":
      case "return":
        {
          if (state.filtered.length === 0) {
            return Effect.succeed(Action.Beep());
          }
          const selected = options.choices[state.index];
          if (selected.disabled) {
            return Effect.succeed(Action.Beep());
          }
          return Effect.succeed(Action.Submit({
            value: selected.value
          }));
        }
      default:
        {
          return processAutoCompleteInput(Option.getOrElse(input.input, () => ""), state, options);
        }
    }
  };
};
const renderClearScreen = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const terminal = yield* Terminal.Terminal;
  const columns = yield* terminal.columns;
  const figures = yield* getTheme(options);
  const resetCurrentLine = Ansi.eraseLine + Ansi.cursorLeft;
  const errorText = renderTextError(state, figures.pointerSmall, figures, {
    plain: true
  });
  const clearOutput = clearOutputWithError(renderTextOutput(state, figures.prefix, figures.pointerSmall, options, figures, {
    plain: true
  }), columns, errorText);
  return clearOutput + resetCurrentLine;
});
const renderTextInput = (nextState, options, theme, submitted, renderOptions) => {
  const text = nextState.value;
  if (renderOptions?.plain === true) {
    switch (options.type) {
      case "hidden":
        {
          return "";
        }
      case "password":
        {
          return theme.passwordMask.repeat(text.length);
        }
      case "text":
        {
          return text;
        }
    }
  }
  if (text.length === 0) {
    // Avoid wrapping an empty value in ANSI codes, which would make renderPrompt
    // add spacing for content that occupies no terminal columns.
    return "";
  }
  const annotation = Option.isSome(nextState.error) ? theme.errorColor : submitted ? theme.submittedColor : Ansi.combine(Ansi.underlined, theme.primaryColor);
  switch (options.type) {
    case "hidden":
      {
        return "";
      }
    case "password":
      {
        return annotateSymbol(theme.passwordMask.repeat(text.length), annotation);
      }
    case "text":
      {
        return Ansi.annotate(text, annotation);
      }
  }
};
const renderTextError = (nextState, pointer, theme, renderOptions) => {
  if (Option.isSome(nextState.error)) {
    return Arr.match(nextState.error.value.split(NEWLINE_REGEXP), {
      onEmpty: () => "",
      onNonEmpty: errorLines => {
        if (renderOptions?.plain === true) {
          return separateSymbol(pointer, errorLines.join("\n"));
        }
        const prefix = annotateSymbol(pointer, theme.errorColor);
        const lines = Arr.map(errorLines, str => annotateErrorLine(str, theme.errorColor));
        return Ansi.cursorSavePosition + "\n" + separateSymbol(prefix, lines.join("\n")) + Ansi.cursorRestorePosition;
      }
    });
  }
  return "";
};
const renderTextOutput = (nextState, leadingSymbol, trailingSymbol, options, theme, renderOptions, submitted = false) => {
  const value = renderTextInput(nextState, options, theme, submitted, renderOptions);
  return renderPrompt(value, options.message, leadingSymbol, trailingSymbol, renderOptions);
};
const renderTextNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const promptMsg = renderTextOutput(state, leadingSymbol, trailingSymbol, options, figures);
  const errorMsg = renderTextError(state, figures.pointerSmall, figures);
  const cursorWidth = options.type === "password" ? figures.passwordMask.length : 1;
  const offset = (state.cursor - state.value.length) * cursorWidth;
  return promptMsg + errorMsg + Ansi.cursorMove(offset);
});
const renderTextSubmission = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const promptMsg = renderTextOutput(state, leadingSymbol, trailingSymbol, options, figures, undefined, true);
  return promptMsg + "\n";
});
const processTextBackspace = state => {
  if (state.cursor <= 0) {
    return Effect.succeed(Action.Beep());
  }
  const beforeCursor = state.value.slice(0, state.cursor - 1);
  const afterCursor = state.value.slice(state.cursor);
  const cursor = state.cursor - 1;
  const value = `${beforeCursor}${afterCursor}`;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      cursor,
      value,
      error: Option.none()
    }
  }));
};
const processTextClear = state => Effect.succeed(Action.NextFrame({
  state: {
    ...state,
    cursor: 0,
    value: "",
    error: Option.none()
  }
}));
const processTextCursorLeft = state => {
  if (state.cursor <= 0) {
    return Effect.succeed(Action.Beep());
  }
  const cursor = state.cursor - 1;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      cursor,
      error: Option.none()
    }
  }));
};
const processTextCursorRight = state => {
  if (state.cursor >= state.value.length) {
    return Effect.succeed(Action.Beep());
  }
  const cursor = Math.min(state.cursor + 1, state.value.length);
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      cursor,
      error: Option.none()
    }
  }));
};
const processTextCursorStart = state => Effect.succeed(Action.NextFrame({
  state: {
    ...state,
    cursor: 0,
    error: Option.none()
  }
}));
const processTextCursorEnd = state => Effect.succeed(Action.NextFrame({
  state: {
    ...state,
    cursor: state.value.length,
    error: Option.none()
  }
}));
const processTab = (state, options) => {
  if (state.value === options.default) {
    return Effect.succeed(Action.Beep());
  }
  const value = state.value.length === 0 ? options.default : state.value;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      value,
      cursor: value.length,
      error: Option.none()
    }
  }));
};
const defaultTextProcessor = (input, state) => {
  const beforeCursor = state.value.slice(0, state.cursor);
  const afterCursor = state.value.slice(state.cursor);
  const value = `${beforeCursor}${input}${afterCursor}`;
  const cursor = state.cursor + input.length;
  return Effect.succeed(Action.NextFrame({
    state: {
      ...state,
      cursor,
      value,
      error: Option.none()
    }
  }));
};
const handleTextRender = options => {
  return (state, action) => {
    return Action.$match(action, {
      Beep: () => Effect.succeed(renderBeep),
      NextFrame: ({
        state
      }) => renderTextNextFrame(state, options),
      Submit: () => renderTextSubmission(state, options)
    });
  };
};
const handleTextProcess = options => {
  return (input, state) => {
    if (input.key.ctrl) {
      switch (input.key.name) {
        case "u":
          {
            return processTextClear(state);
          }
        case "a":
          {
            return processTextCursorStart(state);
          }
        case "e":
          {
            return processTextCursorEnd(state);
          }
        default:
          {
            return Effect.succeed(Action.Beep());
          }
      }
    }
    switch (input.key.name) {
      case "backspace":
        {
          return processTextBackspace(state);
        }
      case "left":
        {
          return processTextCursorLeft(state);
        }
      case "right":
        {
          return processTextCursorRight(state);
        }
      case "home":
        {
          return processTextCursorStart(state);
        }
      case "end":
        {
          return processTextCursorEnd(state);
        }
      case "enter":
      case "return":
        {
          const value = state.value;
          return Effect.match(options.validate(value), {
            onFailure: error => Action.NextFrame({
              state: {
                ...state,
                value,
                error: Option.some(error)
              }
            }),
            onSuccess: value => Action.Submit({
              value
            })
          });
        }
      case "tab":
        {
          return processTab(state, options);
        }
      default:
        {
          return defaultTextProcessor(Option.getOrElse(input.input, () => ""), state);
        }
    }
  };
};
const handleTextClear = options => {
  return (state, _) => {
    return renderClearScreen(state, options);
  };
};
const basePrompt = (options, type) => {
  const opts = {
    default: "",
    type,
    validate: Effect.succeed,
    ...options
  };
  const initialState = {
    cursor: opts.default.length,
    value: opts.default,
    error: Option.none()
  };
  return Custom(initialState, {
    render: handleTextRender(opts),
    process: handleTextProcess(opts),
    clear: handleTextClear(opts)
  });
};
const handleToggleClear = /*#__PURE__*/Effect.fnUntraced(function* (options) {
  const terminal = yield* Terminal.Terminal;
  const columns = yield* terminal.columns;
  const figures = yield* getTheme(options);
  const clearPrompt = Ansi.eraseLine + Ansi.cursorLeft;
  const toggleText = options.active + " " + separateSymbol(figures.toggleSeparator, options.inactive);
  const promptText = renderPrompt(toggleText, options.message, figures.prefix, figures.pointerSmall, {
    plain: true
  });
  const clearOutput = eraseText(promptText, columns);
  return clearOutput + clearPrompt;
});
const renderToggle = (value, options, theme, submitted = false) => {
  const separator = annotateSymbol(theme.toggleSeparator, theme.mutedColor);
  const selectedAnnotation = Ansi.combine(Ansi.underlined, submitted ? theme.submittedColor : theme.primaryColor);
  const inactive = value ? options.inactive : Ansi.annotate(options.inactive, selectedAnnotation);
  const active = value ? Ansi.annotate(options.active, selectedAnnotation) : options.active;
  return active + " " + separateSymbol(separator, inactive);
};
const renderToggleOutput = (toggle, leadingSymbol, trailingSymbol, options) => {
  return renderPrompt(toggle, options.message, leadingSymbol, trailingSymbol);
};
const renderToggleNextFrame = /*#__PURE__*/Effect.fnUntraced(function* (state, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.prefix, figures.primaryColor);
  const trailingSymbol = annotateSymbol(figures.pointerSmall, figures.mutedColor);
  const toggle = renderToggle(state, options, figures);
  const promptMsg = renderToggleOutput(toggle, leadingSymbol, trailingSymbol, options);
  return Ansi.cursorHide + promptMsg;
});
const renderToggleSubmission = /*#__PURE__*/Effect.fnUntraced(function* (value, options) {
  const figures = yield* getTheme(options);
  const leadingSymbol = annotateSymbol(figures.tick, figures.successColor);
  const trailingSymbol = annotateSymbol(figures.ellipsis, figures.mutedColor);
  const toggle = renderToggle(value, options, figures, true);
  const promptMsg = renderToggleOutput(toggle, leadingSymbol, trailingSymbol, options);
  return promptMsg + "\n";
});
const activate = /*#__PURE__*/Effect.succeed(/*#__PURE__*/Action.NextFrame({
  state: true
}));
const deactivate = /*#__PURE__*/Effect.succeed(/*#__PURE__*/Action.NextFrame({
  state: false
}));
const handleToggleRender = options => {
  return (state, action) => {
    switch (action._tag) {
      case "Beep":
        {
          return Effect.succeed(renderBeep);
        }
      case "NextFrame":
        {
          return renderToggleNextFrame(state, options);
        }
      case "Submit":
        {
          return renderToggleSubmission(state, options);
        }
    }
  };
};
const handleToggleProcess = (input, state) => {
  switch (input.key.name) {
    case "0":
    case "j":
    case "delete":
    case "right":
    case "down":
      {
        return deactivate;
      }
    case "1":
    case "k":
    case "left":
    case "up":
      {
        return activate;
      }
    case " ":
    case "tab":
      {
        return state ? deactivate : activate;
      }
    case "enter":
    case "return":
      {
        return Effect.succeed(Action.Submit({
          value: state
        }));
      }
    default:
      {
        return Effect.succeed(Action.Beep());
      }
  }
};
const entriesToDisplay = (cursor, total, maxVisible) => {
  const max = maxVisible === undefined ? total : maxVisible;
  let startIndex = Math.min(total - max, cursor - Math.floor(max / 2));
  if (startIndex < 0) {
    startIndex = 0;
  }
  const endIndex = Math.min(startIndex + max, total);
  return {
    startIndex,
    endIndex
  };
};
//# sourceMappingURL=Prompt.js.map