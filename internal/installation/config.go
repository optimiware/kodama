package installation

// Explicit installation configuration parsing. ParseConfig reads exactly the
// document it is given: it never consults the environment, a file, a socket or
// an adapter probe, and it never infers a missing adapter, endpoint or key
// reference. Config.Preflight then decides whether the declared composition is
// complete.

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/optimiware/kodama/internal/fault"
)

// OptionalCapability names one optional companion. Every optional capability is
// off unless the installation document explicitly enables it.
type OptionalCapability string

const (
	// OptionalGit enables the Git service companion.
	OptionalGit OptionalCapability = "git"
	// OptionalGateways enables model gateways.
	OptionalGateways OptionalCapability = "gateways"
	// OptionalAgents enables agent companions.
	OptionalAgents OptionalCapability = "agents"
	// OptionalServices enables external service companions.
	OptionalServices OptionalCapability = "services"
)

// OptionalCapabilities returns every optional capability in declaration order.
func OptionalCapabilities() []OptionalCapability {
	return []OptionalCapability{
		OptionalGit,
		OptionalGateways,
		OptionalAgents,
		OptionalServices,
	}
}

// Valid reports whether the capability is one of the declared companions.
func (capability OptionalCapability) Valid() bool {
	return slices.Contains(OptionalCapabilities(), capability)
}

// Key-reference families adopted from the secret contract. Each adapter family
// serves exactly one prefix.
const (
	// KeyRefEphemeralPrefix is the development ephemeral key reference prefix.
	KeyRefEphemeralPrefix = "ephemeral:"
	// KeyRefKeyfilePrefix is the test keyfile key reference prefix.
	KeyRefKeyfilePrefix = "keyfile:"
	// KeyRefAWSKMSPrefix is the AWS KMS key reference prefix.
	KeyRefAWSKMSPrefix = "aws-kms:"
	// KeyRefGCPKMSPrefix is the GCP KMS key reference prefix.
	KeyRefGCPKMSPrefix = "gcp-kms:"
	// KeyRefScalewayKMSPrefix is the Scaleway KMS key reference prefix.
	KeyRefScalewayKMSPrefix = "scw-kms:"
)

// keyRefPrefix returns the one reference prefix the key adapter serves.
func keyRefPrefix(adapter KeyAdapter) (string, bool) {
	switch adapter {
	case KeyEphemeral:
		return KeyRefEphemeralPrefix, true
	case KeyKeyfile:
		return KeyRefKeyfilePrefix, true
	case KeyAWSKMS:
		return KeyRefAWSKMSPrefix, true
	case KeyGCPKMS:
		return KeyRefGCPKMSPrefix, true
	case KeyScalewayKMS:
		return KeyRefScalewayKMSPrefix, true
	default:
		return "", false
	}
}

// ParseConfig parses one explicit installation document.
//
// The document is UTF-8 text with one `key = value` per line. Blank lines and
// whole-line `#` comments are ignored and a trailing `\r` is accepted. The
// first `=` separates key from value; both are trimmed. Unknown keys, duplicate
// keys, lines without `=`, empty keys and empty values are refused. The parsed
// Config is exactly what the document declares and is then checked by
// Preflight, including the key reference family.
func ParseConfig(reader io.Reader) (Config, error) {
	body, err := io.ReadAll(reader)
	if err != nil {
		return Config{}, err
	}

	config := Config{}
	seen := make(map[string]bool)
	for index, rawLine := range strings.Split(string(body), "\n") {
		lineNumber := index + 1
		line := strings.TrimSuffix(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		before, after, ok := strings.Cut(trimmed, "=")
		if !ok {
			// Never echo the offending line: it can carry a credential such as
			// a database URL. A declared key name is safe and helps the operator
			// find the malformed line.
			if fields := strings.Fields(trimmed); len(fields) > 0 && knownConfigKey(fields[0]) {
				return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("installation line %d for key %q has no key/value separator", lineNumber, fields[0]))
			}
			return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("installation line %d has no key/value separator", lineNumber))
		}
		key := strings.TrimSpace(before)
		value := strings.TrimSpace(after)
		if key == "" {
			return Config{}, fault.New(fault.InvalidInput, "installation line has an empty key")
		}
		// Only a declared key name is safe to echo. A mistyped line can push
		// arbitrary text, including a credential, into the key position (for
		// example a `database_url: postgres://user:pw@host/db` line whose URL
		// carries `=`), so an unrecognised key is reported by line number only.
		known := knownConfigKey(key)
		if value == "" {
			if !known {
				return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("installation line %d has an unknown key", lineNumber))
			}
			return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("installation key %q has an empty value", key))
		}
		if !known {
			return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("installation line %d has an unknown key", lineNumber))
		}
		if seen[key] {
			return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("duplicate installation key %q", key))
		}
		seen[key] = true

		switch key {
		case "environment":
			config.Environment = Environment(value)
		case "state":
			config.State = StateAdapter(value)
		case "messaging":
			config.Messaging = MessagingAdapter(value)
		case "runtime":
			config.Runtime = RuntimeAdapter(value)
		case "keys":
			config.Keys = KeyAdapter(value)
		case "database_url":
			config.DatabaseURL = value
		case "nats_url":
			config.NATSURL = value
		case "public_url":
			config.PublicURL = value
		case "key_ref":
			config.KeyRef = value
		case "optional":
			optional, err := parseOptionalCapabilities(value)
			if err != nil {
				return Config{}, err
			}
			config.Optional = optional
		}
	}

	for _, key := range []string{"environment", "state", "messaging", "runtime", "keys"} {
		if !seen[key] {
			return Config{}, fault.New(fault.InvalidInput, fmt.Sprintf("installation document omits required key %q", key))
		}
	}

	if err := config.Preflight(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// knownConfigKey reports whether key is one of the declared document keys.
func knownConfigKey(key string) bool {
	switch key {
	case "environment", "state", "messaging", "runtime", "keys",
		"database_url", "nats_url", "public_url", "key_ref", "optional":
		return true
	default:
		return false
	}
}

// parseOptionalCapabilities parses one comma-separated capability list.
func parseOptionalCapabilities(value string) (map[OptionalCapability]bool, error) {
	enabled := make(map[OptionalCapability]bool)
	for position, item := range strings.Split(value, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("optional capability list item %d is empty", position+1))
		}
		capability := OptionalCapability(name)
		if !capability.Valid() {
			// The item may be a mistyped credential; report its position, not
			// its text.
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("optional capability list item %d is not a declared capability", position+1))
		}
		if enabled[capability] {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("optional capability list item %d repeats an earlier capability", position+1))
		}
		enabled[capability] = true
	}
	return enabled, nil
}

// validateKeyReference refuses a key reference outside the selected key adapter
// family. Development may omit the reference; test and production require one
// and Preflight already refuses an empty value.
func (config Config) validateKeyReference() error {
	if config.KeyRef == "" {
		if config.Environment == EnvironmentDevelopment {
			return nil
		}
		return fault.New(fault.InvalidInput, "key reference is empty")
	}
	prefix, ok := keyRefPrefix(config.Keys)
	if !ok {
		return fault.New(fault.InvalidInput, "key adapter is not one of the declared adapters")
	}
	if !strings.HasPrefix(config.KeyRef, prefix) {
		// The reference itself may be a mistyped credential; name only the
		// declared adapter family.
		return fault.New(fault.InvalidInput, fmt.Sprintf("key reference does not belong to the %s key adapter family", config.Keys))
	}
	name := strings.TrimPrefix(config.KeyRef, prefix)
	if name == "" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("key reference has no key name after the %q prefix", prefix))
	}
	// The family prefix must be an exact prefix of the whole reference, and
	// the key name must not carry whitespace or control bytes: otherwise a
	// lower-cased/trimmed or Contains-style match could attribute a reference
	// to the wrong family.
	if strings.IndexFunc(name, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) >= 0 || strings.TrimSpace(name) != name {
		return fault.New(fault.InvalidInput, fmt.Sprintf("key reference has an invalid key name after the %q prefix", prefix))
	}
	// The AWS and GCP families are provider resources, not an opaque name.
	// Development, test and scw-kms references keep the opaque-name rule
	// above.
	switch config.Keys {
	case KeyAWSKMS:
		return validateAWSKeyReference(name)
	case KeyGCPKMS:
		return validateGCPKeyReference(name)
	default:
		return nil
	}
}
