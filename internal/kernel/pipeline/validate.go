package pipeline

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxSteps        = 64
	maxCommands     = 64
	maxServices     = 32
	maxServicePorts = 8
)

var (
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	secretPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,127}$`)
)

var gitEvents = map[string]bool{"push": true, "tag": true, "merge": true}

var serviceScopes = map[string]bool{"pipeline": true, "project": true, "tenant": true}

// Validate checks the semantic rules and returns a *Diagnostic on failure. The
// first finding in document order is reported.
func (d Document) Validate() error {
	if !namePattern.MatchString(d.Name) {
		return &Diagnostic{Code: CodeInvalidName, Path: "name", Message: "the name must match ^[a-z][a-z0-9_-]{0,62}$"}
	}
	if len(d.Steps) < 1 || len(d.Steps) > maxSteps {
		return &Diagnostic{Code: CodeInvalidValue, Path: "steps", Message: "the document must declare between 1 and 64 steps"}
	}
	for i, step := range d.Steps {
		path := pathf("steps[%d]", i)
		if !namePattern.MatchString(step.Name) {
			return &Diagnostic{Code: CodeInvalidName, Path: path + ".name", Message: "the step name must match ^[a-z][a-z0-9_-]{0,62}$"}
		}
		if strings.TrimSpace(step.Image) == "" {
			return &Diagnostic{Code: CodeInvalidValue, Path: path + ".image", Message: "the step image must not be empty"}
		}
		if len(step.Commands) < 1 || len(step.Commands) > maxCommands {
			return &Diagnostic{Code: CodeInvalidValue, Path: path + ".commands", Message: "the step must declare between 1 and 64 commands"}
		}
		for j, command := range step.Commands {
			if strings.TrimSpace(command) == "" {
				return &Diagnostic{Code: CodeInvalidValue, Path: pathf("%s.commands[%d]", path, j), Message: "the command must not be empty"}
			}
		}
		if err := validateEnv(path+".env", step.Env); err != nil {
			return err
		}
		if err := validateSecrets(path+".secrets", step.Secrets); err != nil {
			return err
		}
		if step.Timeout != "" {
			timeout, err := time.ParseDuration(step.Timeout)
			if err != nil || timeout <= 0 {
				return &Diagnostic{Code: CodeInvalidValue, Path: path + ".timeout", Message: "the timeout must be a positive Go duration"}
			}
		}
	}

	// Order also rejects duplicate names, self or duplicate dependencies,
	// references to undeclared steps and cycles.
	if _, err := d.Order(); err != nil {
		return err
	}

	if len(d.Services) > maxServices {
		return &Diagnostic{Code: CodeInvalidValue, Path: "services", Message: "the document must declare at most 32 services"}
	}
	for _, name := range slices.Sorted(maps.Keys(d.Services)) {
		service := d.Services[name]
		path := "services." + name
		if !namePattern.MatchString(name) {
			return &Diagnostic{Code: CodeInvalidName, Path: path, Message: "the service name must match ^[a-z][a-z0-9_-]{0,62}$"}
		}
		if service.Scope != "" && !serviceScopes[service.Scope] {
			return &Diagnostic{Code: CodeInvalidScope, Path: path + ".scope", Message: "the scope must be pipeline, project or tenant"}
		}
		if strings.TrimSpace(service.Image) == "" {
			return &Diagnostic{Code: CodeInvalidValue, Path: path + ".image", Message: "the service image must not be empty"}
		}
		if err := validateEnv(path+".env", service.Env); err != nil {
			return err
		}
		if err := validateSecrets(path+".secrets", service.Secrets); err != nil {
			return err
		}
		if len(service.Ports) > maxServicePorts {
			return &Diagnostic{Code: CodeInvalidValue, Path: path + ".ports", Message: "the service must declare at most 8 ports"}
		}
		seen := make(map[int]bool, len(service.Ports))
		for i, port := range service.Ports {
			portPath := pathf("%s.ports[%d]", path, i)
			if port < 1 || port > 65535 {
				return &Diagnostic{Code: CodeInvalidValue, Path: portPath, Message: "the port must be between 1 and 65535"}
			}
			if seen[port] {
				return &Diagnostic{Code: CodeInvalidValue, Path: portPath, Message: "the port is declared more than once"}
			}
			seen[port] = true
		}
	}

	return validateTriggers(d.Triggers)
}

func validateEnv(path string, env map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !envKeyPattern.MatchString(key) {
			return &Diagnostic{Code: CodeInvalidEnv, Path: path + "[" + strconv.Quote(key) + "]", Message: "the environment key must match ^[A-Za-z_][A-Za-z0-9_]{0,127}$"}
		}
	}
	return nil
}

func validateSecrets(path string, secrets []string) error {
	seen := make(map[string]bool, len(secrets))
	for i, secret := range secrets {
		secretPath := pathf("%s[%d]", path, i)
		if !secretPattern.MatchString(secret) {
			return &Diagnostic{Code: CodeInvalidSecret, Path: secretPath, Message: "the secret name must match ^[A-Za-z_][A-Za-z0-9_-]{0,127}$"}
		}
		if seen[secret] {
			return &Diagnostic{Code: CodeInvalidSecret, Path: secretPath, Message: "the secret is declared more than once"}
		}
		seen[secret] = true
	}
	return nil
}

func validateTriggers(triggers Triggers) error {
	for i, event := range triggers.Git.Events {
		if !gitEvents[event] {
			return &Diagnostic{Code: CodeInvalidValue, Path: pathf("triggers.git.events[%d]", i), Message: "the event must be push, tag or merge"}
		}
	}
	for _, filter := range []struct {
		path     string
		patterns []string
	}{
		{"triggers.git.branches", triggers.Git.Branches},
		{"triggers.git.tags", triggers.Git.Tags},
	} {
		for i, pattern := range filter.patterns {
			if pattern == "" || strings.ContainsFunc(pattern, unicode.IsSpace) {
				return &Diagnostic{Code: CodeInvalidValue, Path: pathf("%s[%d]", filter.path, i), Message: "the pattern must be a non-empty string without whitespace"}
			}
		}
	}
	return nil
}

func pathf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
