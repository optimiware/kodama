package installation

// Hook and placeholder-command chart checks. The chart is inspected as
// text: no helm binary, template engine, YAML decoder, cluster or network
// access is used.
//
// A Helm pre-install/pre-upgrade hook runs before the release's ordinary
// objects exist, so a hook may not depend on a ServiceAccount that Helm
// creates later in the same release: a fresh `helm upgrade --install --wait`
// would block until the timeout waiting for a pod that can never be scheduled.
// A hook also has to run a real command, not a placeholder such as `--help`
// that exits zero and can only prove the binary parses its flags.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const chartHookMarker = "helm.sh/hook"

// templateNames returns the sorted template file names.
func templateNames(bodies map[string]string) []string {
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// hookCommandTokens returns the args/command entries declared in a template.
func hookCommandTokens(body string) []string {
	var tokens []string
	for _, pattern := range []string{
		`(?ms)^[ \t]*args:[ \t]*\r?\n((?:[ \t]*-[ \t]*.*\r?\n?)+)`,
		`(?ms)^[ \t]*command:[ \t]*\r?\n((?:[ \t]*-[ \t]*.*\r?\n?)+)`,
	} {
		match := regexp.MustCompile(pattern).FindStringSubmatch(body)
		if match == nil {
			continue
		}
		for line := range strings.SplitSeq(match[1], "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "-") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			value = strings.Trim(value, `"'`)
			if value != "" {
				tokens = append(tokens, value)
			}
		}
	}
	return tokens
}

// hasExecutableHookCommand reports whether a hook runs a real command. A
// placeholder flag such as `--help` or `--version` is not a command; at least
// one non-flag token (the command or subcommand) is required.
func hasExecutableHookCommand(body string) bool {
	for _, token := range hookCommandTokens(body) {
		if strings.Contains(token, "{{") {
			// A templated command cannot be judged statically; require the
			// template to pass at least one non-placeholder literal too.
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		return true
	}
	return false
}

// hookServiceAccountName returns the literal serviceAccountName of a hook, or
// a template expression, or "" when the hook does not set one.
func hookServiceAccountName(body string) string {
	match := regexp.MustCompile(`(?m)^[ \t]*serviceAccountName:[ \t]*(\S.*)$`).FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// automountDisabled reports whether the pod explicitly disables the mounted
// service-account token, which removes the dependency on a release-owned
// ServiceAccount object.
func automountDisabled(body string) bool {
	return regexp.MustCompile(`(?m)^[ \t]*automountServiceAccountToken:[ \t]*false[ \t]*$`).MatchString(body)
}

// checkHookTemplate validates one hook template against the two hook
// invariants: it runs a real command, and it does not depend on a non-hook
// ServiceAccount.
func checkHookTemplate(name, body string, hookServiceAccounts map[string]bool) error {
	if !strings.Contains(body, chartHookMarker) {
		return nil
	}
	if !hasExecutableHookCommand(body) {
		return fmt.Errorf("%s is a pre-install/pre-upgrade hook without a real command (placeholder args: %v)", name, hookCommandTokens(body))
	}
	serviceAccount := hookServiceAccountName(body)
	if serviceAccount == "" || automountDisabled(body) {
		return nil
	}
	if strings.Contains(serviceAccount, "{{") {
		return fmt.Errorf("%s references a templated ServiceAccount %q and does not set automountServiceAccountToken: false; a hook must not depend on a release-owned ServiceAccount", name, serviceAccount)
	}
	if !hookServiceAccounts[serviceAccount] {
		return fmt.Errorf("%s references ServiceAccount %q, which is not itself a hook and is created after pre-install hooks", name, serviceAccount)
	}
	return nil
}

// TestHookTemplatesRunRealCommandsAndOwnTheirServiceAccounts scans every
// template carrying a Helm hook annotation. The chart ships no such template;
// the check still runs so a placeholder hook fails immediately.
func TestHookTemplatesRunRealCommandsAndOwnTheirServiceAccounts(t *testing.T) {
	bodies := templateBodies(t)
	hookServiceAccounts := make(map[string]bool)
	for _, body := range bodies {
		if !strings.Contains(body, chartHookMarker) {
			continue
		}
		if !regexp.MustCompile(`(?m)^[ \t]*kind:[ \t]*ServiceAccount[ \t]*$`).MatchString(body) {
			continue
		}
		match := regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*(\S+)[ \t]*$`).FindStringSubmatch(body)
		if match != nil {
			hookServiceAccounts[strings.Trim(match[1], `"'`)] = true
		}
	}
	for _, name := range templateNames(bodies) {
		if err := checkHookTemplate(name, bodies[name], hookServiceAccounts); err != nil {
			t.Error(err)
		}
	}
}

// TestNoHelpPlaceholderArguments rejects a container whose only argv is a
// flag that prints help and exits zero. Such a gate can never fail.
func TestNoHelpPlaceholderArguments(t *testing.T) {
	placeholder := regexp.MustCompile(`(?m)^[ \t]*-[ \t]*["']?(--help|-h|--version)["']?[ \t]*$`)
	for name, body := range templateBodies(t) {
		for _, line := range placeholder.FindAllString(body, -1) {
			t.Errorf("%s ships a placeholder container argument %q; a chart container must run a real command", name, strings.TrimSpace(line))
		}
	}
}

// TestInstallationCheckJobRemoved pins the absence of a placeholder
// pre-install Job. Without a real preflight command the chart ships no Job at
// all.
func TestInstallationCheckJobRemoved(t *testing.T) {
	bodies := templateBodies(t)
	if body, ok := bodies["job.yaml"]; ok {
		t.Fatalf("templates/job.yaml is back (%d bytes); the chart must not ship a placeholder pre-install Job", len(body))
	}
	jobKind := regexp.MustCompile(`(?m)^[ \t]*kind:[ \t]*(Job|CronJob)[ \t]*$`)
	for name, body := range bodies {
		for _, match := range jobKind.FindAllString(body, -1) {
			t.Errorf("%s declares %q; the chart ships no Job without a real preflight command", name, strings.TrimSpace(match))
		}
	}
}
