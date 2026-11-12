package id

import (
	"errors"
	"fmt"
)

const (
	// MaxNameLength bounds an action name in bytes.
	MaxNameLength = 64
	// MaxKeyLength bounds an environment, secret, input or output key in bytes.
	MaxKeyLength = 64
)

// ErrInvalidName reports a name or key that is unsafe to materialize or use as
// a stable declared key.
var ErrInvalidName = errors.New("invalid name")

// Reserved environment names are supplied by the Shell as unshadowable context
// and cannot be declared by a pipeline or action.
const (
	EnvURL       = "KODAMA_URL"
	EnvExecution = "KODAMA_EXECUTION"
	EnvAction    = "KODAMA_ACTION"
	EnvTask      = "KODAMA_TASK"
)

// SecretTokenName is the reserved execution-credential mount name. Stored
// secrets cannot shadow it.
const SecretTokenName = "KODAMA_TOKEN"

// ActionName is the lowercase slug that names an action inside a pipeline.
type ActionName string

// EnvName is an uppercase literal environment variable name.
type EnvName string

// SecretName is an uppercase declared secret name.
type SecretName string

// InputKey is a lowercase admitted input name.
type InputKey string

// OutputKey is a lowercase declared output name.
type OutputKey string

// ReservedEnvNames returns the unshadowable environment names.
func ReservedEnvNames() []string {
	return []string{EnvURL, EnvExecution, EnvAction, EnvTask}
}

func isLowerAlpha(character byte) bool {
	return character >= 'a' && character <= 'z'
}

func isUpperAlpha(character byte) bool {
	return character >= 'A' && character <= 'Z'
}

func isDigit(character byte) bool {
	return character >= '0' && character <= '9'
}

// validActionName reports whether value is a bounded lowercase slug made of
// alphanumeric segments separated by single hyphens.
func validActionName(value string) bool {
	if value == "" || len(value) > MaxNameLength {
		return false
	}
	if !isLowerAlpha(value[0]) {
		return false
	}
	previousHyphen := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case isLowerAlpha(character), isDigit(character):
			previousHyphen = false
		case character == '-':
			if previousHyphen {
				return false
			}
			previousHyphen = true
		default:
			return false
		}
	}
	return !previousHyphen
}

// validEnvName reports whether value is a bounded uppercase environment name.
func validEnvName(value string) bool {
	if value == "" || len(value) > MaxKeyLength {
		return false
	}
	if !isUpperAlpha(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !isUpperAlpha(character) && !isDigit(character) && character != '_' {
			return false
		}
	}
	return true
}

// validKey reports whether value is a bounded lowercase declared key.
func validKey(value string) bool {
	if value == "" || len(value) > MaxKeyLength {
		return false
	}
	if !isLowerAlpha(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if !isLowerAlpha(character) && !isDigit(character) && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func invalidName(kind, value string) error {
	return fmt.Errorf("%w: %s %q", ErrInvalidName, kind, value)
}

// ParseActionName validates an action slug.
func ParseActionName(value string) (ActionName, error) {
	if !validActionName(value) {
		return "", invalidName("action name", value)
	}
	return ActionName(value), nil
}

// ParseEnvName validates a literal environment variable name and rejects the
// reserved Kodama context names.
func ParseEnvName(value string) (EnvName, error) {
	if !validEnvName(value) {
		return "", invalidName("environment name", value)
	}
	for _, reserved := range ReservedEnvNames() {
		if value == reserved {
			return "", invalidName("reserved environment name", value)
		}
	}
	return EnvName(value), nil
}

// ParseSecretName validates a declared secret name and rejects the reserved
// execution-credential name.
func ParseSecretName(value string) (SecretName, error) {
	if !validEnvName(value) {
		return "", invalidName("secret name", value)
	}
	if value == SecretTokenName {
		return "", invalidName("reserved secret name", value)
	}
	return SecretName(value), nil
}

// ParseInputKey validates an admitted input key.
func ParseInputKey(value string) (InputKey, error) {
	if !validKey(value) {
		return "", invalidName("input key", value)
	}
	return InputKey(value), nil
}

// ParseOutputKey validates a declared output key.
func ParseOutputKey(value string) (OutputKey, error) {
	if !validKey(value) {
		return "", invalidName("output key", value)
	}
	return OutputKey(value), nil
}
