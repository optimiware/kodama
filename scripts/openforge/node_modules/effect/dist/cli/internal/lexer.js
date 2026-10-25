/**
 * @internal
 */
export function lex(argv) {
  const endIndex = argv.indexOf("--");
  if (endIndex === -1) {
    // No -- delimiter, lex everything normally
    return {
      tokens: lexTokens(argv),
      trailingOperands: []
    };
  }
  // Split at -- delimiter
  return {
    tokens: lexTokens(argv.slice(0, endIndex)),
    trailingOperands: argv.slice(endIndex + 1)
  };
}
const negativeNumberRegex = /^-(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$/;
const lexTokens = args => {
  const tokens = [];
  for (const arg of args) {
    if (!arg.startsWith("-") || negativeNumberRegex.test(arg)) {
      tokens.push({
        _tag: "Value",
        value: arg
      });
    } else if (arg.startsWith("--")) {
      const equalIndex = arg.indexOf("=");
      if (equalIndex !== -1) {
        const name = arg.slice(2, equalIndex);
        const value = arg.slice(equalIndex + 1);
        tokens.push({
          _tag: "LongOption",
          name,
          raw: arg,
          value
        });
      } else {
        tokens.push({
          _tag: "LongOption",
          name: arg.slice(2),
          raw: arg
        });
      }
    } else if (arg.length > 1) {
      const flags = arg.slice(1);
      const equalIndex = flags.indexOf("=");
      if (equalIndex !== -1) {
        const flag = flags.slice(0, equalIndex);
        const value = flags.slice(equalIndex + 1);
        tokens.push({
          _tag: "ShortOption",
          flag,
          raw: `-${flag}`,
          value
        });
      } else {
        for (const ch of flags) {
          tokens.push({
            _tag: "ShortOption",
            flag: ch,
            raw: `-${ch}`
          });
        }
      }
    } else {
      tokens.push({
        _tag: "Value",
        value: arg
      });
    }
  }
  return tokens;
};
//# sourceMappingURL=lexer.js.map