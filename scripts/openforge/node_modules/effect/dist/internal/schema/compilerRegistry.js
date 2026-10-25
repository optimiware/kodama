import * as Effect from "../../Effect.js";
import * as Interpreter from "./interpreter.js";
import * as InternalParser from "./parser.js";
/** @internal */
export const invalid = /*#__PURE__*/Symbol();
const cache = /*#__PURE__*/new WeakMap();
let compiler;
/** @internal */
export let compilerAdaptersEnabled = false;
function activateCompilerAdapters() {
  compilerAdaptersEnabled = true;
}
const decodeChild = ast => compilerAdaptersEnabled ? lazyParser(resolve, ast, "parser") : resolve(ast).parser;
const makeChild = ast => compilerAdaptersEnabled ? lazyParser(resolve, ast, "makeEffect") : resolve(ast).makeEffect;
const makeField = ast => Interpreter.compileField(ast, makeChild);
class InterpretedEntry {
  ast;
  constructor(ast) {
    this.ast = ast;
  }
  get decodeEffect() {
    return this.cachedDecodeEffect ??= Interpreter.compile(this.ast, decodeChild);
  }
  get parser() {
    return this.decodeEffect;
  }
  get makeEffect() {
    return this.cachedMakeEffect ??= Interpreter.compile(this.ast, makeChild, makeField);
  }
}
class CompilerEntry extends InterpretedEntry {
  compiled;
  resolve;
  constructor(ast, compiled, resolve) {
    super(ast);
    this.compiled = compiled;
    this.resolve = resolve;
  }
  save(key, value) {
    Object.defineProperty(this, key, {
      value
    });
    return value;
  }
  operation(key) {
    const compiled = this.compiled;
    return typeof compiled === "function" ? compiled(this.ast, this.resolve, key) : compiled?.[key];
  }
  get is() {
    return this.save("is", this.operation("is"));
  }
  get decode() {
    return this.save("decode", this.operation("decode"));
  }
  get make() {
    return this.save("make", this.operation("make"));
  }
  get decodeEffect() {
    return this.save("decodeEffect", this.operation("decodeEffect") ?? Interpreter.compile(this.ast, ast => lazyParser(this.resolve, ast, "parser")));
  }
  get parser() {
    const decode = this.decode;
    return decode === undefined ? this.decodeEffect : this.save("parser", withDecode(decode, () => this.decodeEffect));
  }
  get makeEffect() {
    const makeEffect = this.operation("makeEffect");
    if (makeEffect !== undefined) return this.save("makeEffect", makeEffect);
    const child = ast => lazyParser(this.resolve, ast, "makeEffect");
    return this.save("makeEffect", Interpreter.compile(this.ast, child, ast => Interpreter.compileField(ast, child)));
  }
}
/** @internal */
export function withDecode(fastDecode, decodeEffect) {
  let detailed;
  return (input, options) => {
    if (input !== InternalParser.missing) {
      try {
        const value = fastDecode(input, options);
        if (value !== invalid) return value === input ? InternalParser.sameExit : InternalParser.succeed(value);
      } catch (error) {
        return Effect.die(error);
      }
    }
    return (detailed ??= decodeEffect())(input, options);
  };
}
/** @internal */
export function lazyParser(resolve, ast, operation) {
  const entry = resolve(ast);
  if (entry.compiled === undefined || Object.hasOwn(entry, operation)) {
    return entry[operation];
  }
  let parser;
  return (input, options) => (parser ??= entry[operation])(input, options);
}
/** @internal */
export function resolve(ast) {
  const cached = cache.get(ast);
  if (cached !== undefined) return cached;
  const entry = compiler === undefined ? new InterpretedEntry(ast) : new CompilerEntry(ast, compiler(ast, resolve), resolve);
  cache.set(ast, entry);
  return entry;
}
/** @internal */
export function set(ast, decoder, resolveChild = resolve) {
  if (decoder !== undefined) activateCompilerAdapters();
  const entry = new CompilerEntry(ast, decoder, resolveChild);
  cache.set(ast, entry);
  return entry;
}
/** @internal */
export function setCompiler(ast, compile, resolveChild = resolve) {
  activateCompilerAdapters();
  const entry = new CompilerEntry(ast, compile, resolveChild);
  cache.set(ast, entry);
  return entry;
}
/** @internal */
export function install(compile) {
  activateCompilerAdapters();
  compiler = compile;
}
/** @internal */
export function enable(ast, compile) {
  activateCompilerAdapters();
  const scoped = child => {
    const cached = cache.get(child);
    return cached !== undefined && (cached.compiled !== undefined || cached.resolve === scoped) ? cached : set(child, compile(child, scoped), scoped);
  };
  const decoder = compile(ast, scoped);
  const cached = cache.get(ast);
  if (decoder !== undefined || cached?.compiled === undefined) {
    set(ast, decoder, scoped);
  }
}
//# sourceMappingURL=compilerRegistry.js.map