package secrets

import (
	"strings"
)

// RefScheme and RefScope fix the local secret reference form
// secret://project/<name>. Only the project scope exists in this module:
// tenant and environment inheritance are out of scope.
const (
	RefScheme = "secret://"
	RefScope  = "project"
)

// FormatSecretRef returns the reference of a project secret name.
func FormatSecretRef(name string) string { return RefScheme + RefScope + "/" + name }

// ParseSecretRef extracts the secret name from secret://project/<name>. Any
// other scope, extra segment or invalid name is rejected.
func ParseSecretRef(ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, RefScheme)
	if !ok {
		return "", invalidf("secret reference must start with %s", RefScheme)
	}
	scope, name, ok := strings.Cut(rest, "/")
	if !ok || scope != RefScope {
		return "", invalidf("only %s%s/<name> references are supported", RefScheme, RefScope)
	}
	if err := ValidateSecretName(name); err != nil {
		return "", err
	}
	return name, nil
}
