//go:build qualification

package production

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// defaultKubectlTimeout bounds one kubectl invocation.
const defaultKubectlTimeout = 60 * time.Second

// maxKubectlOutput bounds one captured kubectl stdout.
const maxKubectlOutput = 1 << 20

// Kubectl runs kubectl through an argv array only. It never uses a shell, never
// interpolates a string, and never puts stdout or stderr in an error.
type Kubectl struct {
	Guard   *Guard
	Path    string
	Timeout time.Duration
}

// Run executes one guarded kubectl access and returns bounded stdout on
// success. Every failure is secret-free: it names the verb, resource and
// namespace, never the subprocess output.
func (client *Kubectl) Run(ctx context.Context, namespace, verb, resource string, extra ...string) ([]byte, error) {
	if client == nil || client.Guard == nil {
		return nil, fmt.Errorf("kubectl helper has no confinement guard")
	}
	path := client.Path
	if path == "" {
		path = "kubectl"
	}
	if err := client.Guard.CheckExecutable(path); err != nil {
		return nil, err
	}
	admitted, err := client.Guard.Normalize(Access{
		Context:   client.Guard.Context(),
		Namespace: namespace,
		Verb:      verb,
		Resource:  resource,
	})
	if err != nil {
		return nil, err
	}
	if err := validateKubectlExtras(extra); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = defaultKubectlTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"--context", client.Guard.Context(),
		"--namespace", admitted.Namespace,
		admitted.Verb, admitted.Resource,
	}
	args = append(args, extra...)
	command := exec.CommandContext(runCtx, path, args...)
	command.Env = safeChildEnvironment()
	command.Stdin = nil
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = io.Discard

	err = command.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("kubectl %s %s in namespace %s timed out after %s", admitted.Verb, admitted.Resource, admitted.Namespace, timeout)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("kubectl %s %s in namespace %s failed: %s", admitted.Verb, admitted.Resource, admitted.Namespace, safeExitStatus(err))
	}
	if stdout.Len() > maxKubectlOutput {
		return nil, fmt.Errorf("kubectl %s %s in namespace %s produced more than %d bytes", admitted.Verb, admitted.Resource, admitted.Namespace, maxKubectlOutput)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

// GetNamespace reads exactly one assigned namespace object. The guard admits
// only TYPE/NAME namespace reads in the assigned scope, so this helper can never
// list namespaces or reach a cluster-scoped object.
func (client *Kubectl) GetNamespace(ctx context.Context, name string) ([]byte, error) {
	if client == nil || client.Guard == nil {
		return nil, fmt.Errorf("kubectl helper has no confinement guard")
	}
	if !client.Guard.NamespaceAllowed(name) {
		return nil, fmt.Errorf("namespace %q is outside the assigned kubectl scope", name)
	}
	return client.Run(ctx, name, "get", "namespaces/"+name, "-o", "json")
}

// extraFlagSpec validates one allowlisted kubectl option value.
type extraFlagSpec struct {
	validate     func(string) bool
	attachedOnly bool
}

// allowedExtraFlags is the complete set of kubectl options the harness may
// append. Every other token that starts with '-' is refused.
var allowedExtraFlags = map[string]extraFlagSpec{
	"o":              {validate: validOutput},
	"output":         {validate: validOutput},
	"l":              {validate: validSelector},
	"selector":       {validate: validSelector},
	"replicas":       {validate: func(value string) bool { return validBoundedInt(value, 10) }},
	"grace-period":   {validate: func(value string) bool { return validBoundedInt(value, 300) }},
	"timeout":        {validate: validDuration},
	"for":            {validate: validCondition},
	"tail":           {validate: validTail},
	"since":          {validate: validSince},
	"field-selector": {validate: validCondition},
	"watch":          {validate: func(value string) bool { return value == "false" }, attachedOnly: true},
	"force":          {validate: func(value string) bool { return value == "true" }},
}

// refusedExtraFlags are scope-changing, credential-bearing, raw-read and
// cluster-selection options. They are refused explicitly as well as by the
// allowlist so a later allowlist edit cannot reintroduce them.
var refusedExtraFlags = map[string]bool{
	"n":                        true,
	"namespace":                true,
	"A":                        true,
	"all-namespaces":           true,
	"all":                      true,
	"context":                  true,
	"kubeconfig":               true,
	"server":                   true,
	"cluster":                  true,
	"user":                     true,
	"current-context":          true,
	"username":                 true,
	"password":                 true,
	"token":                    true,
	"as":                       true,
	"as-group":                 true,
	"raw":                      true,
	"client-certificate":       true,
	"client-key":               true,
	"certificate-authority":    true,
	"insecure-skip-tls-verify": true,
	"tls-server-name":          true,
	"request-timeout":          true,
	"match-server-version":     true,
}

var (
	outputExpressionPattern = regexp.MustCompile(`^[A-Za-z0-9_.{}\[\]@*?=,:/!<>-]{1,256}$`)
	selectorPattern         = regexp.MustCompile(`^[A-Za-z0-9_.:/=,!<>()\-]{1,256}$`)
	durationPattern         = regexp.MustCompile(`^[0-9]{1,6}(ms|s|m|h)$`)
	sincePattern            = regexp.MustCompile(`^[0-9]{1,6}(s|m|h|d)$`)
	boundedIntPattern       = regexp.MustCompile(`^[0-9]{1,9}$`)
	positionalExtraPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// validateKubectlExtras validates every appended option and positional name.
func validateKubectlExtras(extra []string) error {
	for index := 0; index < len(extra); index++ {
		token := extra[index]
		if !strings.HasPrefix(token, "-") {
			if !positionalExtraPattern.MatchString(token) {
				return fmt.Errorf("kubectl argument is not a safe name")
			}
			continue
		}
		long := strings.HasPrefix(token, "--")
		name, value, hasValue := splitFlagToken(token)
		if name == "" || refusedExtraFlags[name] {
			return fmt.Errorf("kubectl option is not allowlisted")
		}
		// pflag reads a single dash as a shorthand cluster: -selector=... is
		// -s elector=..., not --selector=.... Only the one-character shorthand
		// flags stay available, and only with a separate value token.
		if !long && (len(name) != 1 || hasValue) {
			return fmt.Errorf("kubectl option is not allowlisted")
		}
		spec, ok := allowedExtraFlags[name]
		if !ok {
			return fmt.Errorf("kubectl option is not allowlisted")
		}
		if spec.attachedOnly && !hasValue {
			return fmt.Errorf("kubectl option requires an attached value")
		}
		if !hasValue {
			index++
			if index >= len(extra) || strings.HasPrefix(extra[index], "-") {
				return fmt.Errorf("kubectl option requires a value")
			}
			value = extra[index]
		}
		if !spec.validate(value) {
			return fmt.Errorf("kubectl option value is not allowlisted")
		}
	}
	return nil
}

// splitFlagToken splits -o=json, --output=json, -o or --output.
func splitFlagToken(token string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(token, "-") {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(token[1:], "-")
	if trimmed == "" {
		return "", "", false
	}
	if index := strings.IndexByte(trimmed, '='); index >= 0 {
		return trimmed[:index], trimmed[index+1:], true
	}
	return trimmed, "", false
}

// validOutput accepts the bounded output formats the harness reads.
func validOutput(value string) bool {
	switch value {
	case "json", "yaml", "name", "wide":
		return true
	}
	const prefix = "jsonpath="
	if after, ok := strings.CutPrefix(value, prefix); ok {
		return outputExpressionPattern.MatchString(after)
	}
	return false
}

// validSelector accepts a Kubernetes label/field selector expression.
func validSelector(value string) bool { return selectorPattern.MatchString(value) }

// validCondition accepts a kubectl --for condition expression.
func validCondition(value string) bool { return selectorPattern.MatchString(value) }

// validDuration accepts a kubectl --timeout duration.
func validDuration(value string) bool { return durationPattern.MatchString(value) }

// validSince accepts a kubectl --since duration with at most six digits.
func validSince(value string) bool { return sincePattern.MatchString(value) }

// validTail accepts -1 (all lines) or a bounded non-negative line count.
func validTail(value string) bool {
	if value == "-1" {
		return true
	}
	return validBoundedInt(value, 10000)
}

// validBoundedInt accepts a non-negative bounded decimal.
func validBoundedInt(value string, maximum int) bool {
	if !boundedIntPattern.MatchString(value) {
		return false
	}
	number, err := strconv.Atoi(value)
	return err == nil && number >= 0 && number <= maximum
}

// safeChildEnvironment passes the process environment to kubectl without any
// credential-shaped variable. kubectl never needs a Kodama token.
func safeChildEnvironment() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name := entry
		if before, _, ok := strings.Cut(entry, "="); ok {
			name = before
		}
		if sensitiveEnvironmentName(name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// sensitiveEnvironmentName reports a variable name that can carry credential
// material.
func sensitiveEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "ACCESS_CODE", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// safeExitStatus renders only a process status. It never includes subprocess
// output or arguments.
func safeExitStatus(err error) string {
	var exitError *exec.ExitError
	if ok := asExitError(err, &exitError); ok {
		if code := exitError.ExitCode(); code >= 0 {
			return fmt.Sprintf("exit status %d", code)
		}
		return "terminated by a signal"
	}
	return "could not execute kubectl"
}

// asExitError is errors.As without importing errors in this file's surface.
func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if candidate, ok := err.(*exec.ExitError); ok {
			*target = candidate
			return true
		}
		type unwrapper interface{ Unwrap() error }
		next, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}
