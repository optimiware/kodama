package cli

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// hasHelpFlag reports whether args asks for help (-h or --help) before a "--"
// argument terminator.
func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// argSpec declares the accepted flags of one subcommand.
type argSpec struct {
	strings map[string]*string
	lists   map[string]*[]string
	bools   map[string]*bool
	aliases map[string]string
}

func newArgSpec() *argSpec {
	return &argSpec{
		strings: make(map[string]*string),
		lists:   make(map[string]*[]string),
		bools:   make(map[string]*bool),
		aliases: make(map[string]string),
	}
}

func (spec *argSpec) stringFlag(target *string, canonical string, aliases ...string) {
	spec.strings[canonical] = target
	for _, alias := range aliases {
		spec.aliases[alias] = canonical
	}
}

func (spec *argSpec) listFlag(target *[]string, canonical string, aliases ...string) {
	spec.lists[canonical] = target
	for _, alias := range aliases {
		spec.aliases[alias] = canonical
	}
}

func (spec *argSpec) boolFlag(target *bool, canonical string, aliases ...string) {
	spec.bools[canonical] = target
	for _, alias := range aliases {
		spec.aliases[alias] = canonical
	}
}

func (spec *argSpec) resolve(raw string) string {
	if mapped, ok := spec.aliases[raw]; ok {
		return mapped
	}
	return raw
}

// parse collects positional arguments and populates registered flags regardless
// of whether flags precede or follow positional arguments.
func (spec *argSpec) parse(args []string) ([]string, map[string]bool, error) {
	var positionals []string
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}

		name, inlineValue, hasInline := strings.Cut(arg, "=")
		canonical := spec.resolve(name)

		if target, ok := spec.bools[canonical]; ok {
			if seen[canonical] {
				return nil, nil, fmt.Errorf("flag %q is repeated", name)
			}
			seen[canonical] = true
			if !hasInline {
				*target = true
				continue
			}
			parsed, err := strconv.ParseBool(inlineValue)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid boolean value %q for %s", inlineValue, name)
			}
			*target = parsed
			continue
		}

		if target, ok := spec.strings[canonical]; ok {
			if seen[canonical] {
				return nil, nil, fmt.Errorf("flag %q is repeated", name)
			}
			seen[canonical] = true
			if hasInline {
				*target = inlineValue
				continue
			}
			if index+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %q requires a value", name)
			}
			index++
			*target = args[index]
			continue
		}

		if target, ok := spec.lists[canonical]; ok {
			seen[canonical] = true
			if hasInline {
				*target = append(*target, inlineValue)
				continue
			}
			if index+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %q requires a value", name)
			}
			index++
			*target = append(*target, args[index])
			continue
		}

		return nil, nil, fmt.Errorf("unknown flag %q", name)
	}
	return positionals, seen, nil
}

// resolveProject resolves the active project from an explicit subcommand flag,
// the loaded configuration, or the KODAMA_PROJECT environment variable.
func resolveProject(flagValue string, config Config, deps CommandDeps) string {
	if trimmed := strings.TrimSpace(flagValue); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(config.Project); trimmed != "" {
		return trimmed
	}
	getenv := deps.Login.Environment
	if getenv == nil {
		getenv = os.Getenv
	}
	return strings.TrimSpace(getenv("KODAMA_PROJECT"))
}

// resolveOutputFormat resolves an optional per-command --output flag or falls
// back to the configured output format (defaulting to OutputTable).
func resolveOutputFormat(flagValue string, config Config) (OutputFormat, error) {
	if flagValue != "" {
		format := OutputFormat(flagValue)
		if !format.Valid() {
			return "", fmt.Errorf("unknown output format %q", flagValue)
		}
		return format, nil
	}
	if config.Output != "" {
		if !config.Output.Valid() {
			return "", fmt.Errorf("unknown output format %q", config.Output)
		}
		return config.Output, nil
	}
	return OutputTable, nil
}

// parsePositiveVersion parses an optional positive uint64 version flag.
func parsePositiveVersion(raw, flagName string) (*uint64, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || parsed == 0 {
		return nil, fmt.Errorf("%s %q must be a positive integer", flagName, raw)
	}
	return &parsed, nil
}

// parseKeyValues decodes repeatable key=value flags into a map.
func parseKeyValues(entries []string, label string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%s %q must be key=value", label, entry)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("%s key %q is repeated", label, key)
		}
		values[key] = val
	}
	return values, nil
}

// makeIdempotencyKey returns an explicit idempotency key or mints a fresh valid
// caller key for one mutating CLI command.
func makeIdempotencyKey(explicit, operation string, random Random) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	var reader io.Reader = cryptorand.Reader
	if random != nil {
		reader = random
	}
	var buf [16]byte
	if _, err := io.ReadFull(reader, buf[:]); err != nil {
		return "", fmt.Errorf("cannot generate idempotency key: %w", err)
	}
	return "cli:" + operation + ":" + hex.EncodeToString(buf[:]), nil
}
